// Package store is the confined document store of the sheet API's document
// plane: .tsvt files under one operator-fixed root, read and replaced through
// os.Root so no path can escape, every write atomic (temp + rename), every
// mutation revision-checked, and every stored byte the engine's canonical
// serialization. The store never evaluates an expression — parsing for
// canonicalization and reference rewriting only.
package store

import (
	"errors"
	"io/fs"
	"os"
	"path"
	"strings"
	"sync"
	"syscall"

	tsvsheet "github.com/tsvsheet/go-tsvsheet"

	"github.com/tsvsheet/tsvsheet.api/internal/constants"
)

// DocPath is a document's path relative to the store root. It must be clean,
// relative, and free of "!" (reserved by the HTTP binding's reference suffix).
type DocPath string

// Snapshot is one document state: the parsed document, its canonical bytes,
// and their content address. Carrying the Document means no consumer ever
// reparses served text.
type Snapshot struct {
	Doc  tsvsheet.Document
	Rev  tsvsheet.RevisionHex
	Text []byte
}

// Applied is the result of an op-batch application: the state folded from and
// the state produced.
type Applied struct {
	Old Snapshot
	New Snapshot
}

// Expect is a mutation's precondition: a revision the document must currently
// have, or the requirement that it not exist.
type Expect struct {
	rev      tsvsheet.RevisionHex
	isAbsent bool
}

// ExpectRev requires the document to currently have revision rev.
func ExpectRev(rev tsvsheet.RevisionHex) Expect { return Expect{rev: rev} }

// ExpectAbsent requires the document to not exist (create).
func ExpectAbsent() Expect { return Expect{isAbsent: true} }

// RootDir is the operator-fixed directory a store confines itself to.
type RootDir string

// Store serves and mutates the documents under one root directory.
//
// Pointer receivers are necessary here: the store wraps an os.Root handle and
// a mutex serializing check-and-set mutations, neither of which may be copied.
type Store struct {
	root   *os.Root
	limits tsvsheet.Limits
	mu     sync.Mutex
}

// Open confines a store to the directory at dir.
func Open(dir RootDir, limits tsvsheet.Limits) (*Store, error) {
	root, err := os.OpenRoot(string(dir))
	if err != nil {
		return nil, constants.ErrRootOpen.With(err, "dir", string(dir))
	}
	return &Store{root: root, limits: limits}, nil
}

// Get reads the document at p, canonicalized: the returned text is the
// engine's serialization of the stored file, and the revision addresses those
// canonical bytes.
func (s *Store) Get(p DocPath) (Snapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, snap, err := s.load(p)
	return snap, err
}

// Put replaces (or, with ExpectAbsent, creates) the document at p from body,
// storing the canonical serialization. It reports whether the document was
// created.
func (s *Store) Put(p DocPath, body []byte, expect Expect) (Snapshot, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	doc, err := tsvsheet.ParseDocument(body)
	if err != nil {
		return Snapshot{}, false, constants.ErrDocSyntax.With(err, "path", string(p))
	}
	created, err := s.check(p, expect)
	if err != nil {
		return Snapshot{}, false, err
	}
	snap := snapshotOf(doc)
	if err := s.write(p, snap.Text); err != nil {
		return Snapshot{}, false, err
	}
	return snap, created, nil
}

// Apply folds the op batch over the document at p, which must currently have
// revision expect; the result is persisted atomically and both states
// returned. Any refusal — stale revision, in-band base conflict, refused op —
// leaves the file untouched.
func (s *Store) Apply(p DocPath, batch tsvsheet.Edits, expect tsvsheet.RevisionHex) (Applied, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	doc, old, err := s.load(p)
	if err != nil {
		return Applied{}, err
	}
	if old.Rev != expect {
		return Applied{}, constants.ErrPrecond.With(nil, "expect", string(expect), "at", string(old.Rev))
	}
	next, err := tsvsheet.Apply(doc, batch, s.limits)
	if err != nil {
		return Applied{}, err
	}
	snap := snapshotOf(next)
	if err := s.write(p, snap.Text); err != nil {
		return Applied{}, err
	}
	return Applied{Old: old, New: snap}, nil
}

// Delete removes the document at p, which must currently have revision expect.
func (s *Store) Delete(p DocPath, expect tsvsheet.RevisionHex) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, snap, err := s.load(p)
	if err != nil {
		return err
	}
	if snap.Rev != expect {
		return constants.ErrPrecond.With(nil, "expect", string(expect), "at", string(snap.Rev))
	}
	if err := removeIn(s.root, string(p)); err != nil {
		return constants.ErrDocWrite.With(err, "path", string(p))
	}
	return nil
}

// load reads and parses the document at p under the held lock.
func (s *Store) load(p DocPath) (tsvsheet.Document, Snapshot, error) {
	if err := validate(p); err != nil {
		return tsvsheet.Document{}, Snapshot{}, err
	}
	raw, err := s.root.ReadFile(string(p))
	if errors.Is(err, fs.ErrNotExist) || isRefusedPath(err) {
		// A path os.Root refuses — an escaping symlink, a name the OS will not
		// open — is a client-named path that resolves to nothing servable, so
		// it is "not found", not a server fault. Reporting it as 500 would both
		// blame the server and disclose that the escaping name exists.
		return tsvsheet.Document{}, Snapshot{}, constants.ErrDocMissing.With(err, "path", string(p))
	}
	if err != nil {
		return tsvsheet.Document{}, Snapshot{}, constants.ErrDocRead.With(err, "path", string(p))
	}
	doc, err := tsvsheet.ParseDocument(raw)
	if err != nil {
		return tsvsheet.Document{}, Snapshot{}, constants.ErrDocParse.With(err, "path", string(p))
	}
	return doc, snapshotOf(doc), nil
}

// check verifies a Put precondition under the held lock, reporting whether the
// write will create the document.
func (s *Store) check(p DocPath, expect Expect) (bool, error) {
	_, current, err := s.load(p)
	missing := errors.Is(err, constants.ErrDocMissing)
	if err != nil && !missing {
		return false, err
	}
	if expect.isAbsent {
		if !missing {
			return false, constants.ErrDocExists.With(nil, "path", string(p))
		}
		return true, nil
	}
	if missing {
		return false, err
	}
	if current.Rev != expect.rev {
		return false, constants.ErrPrecond.With(nil, "expect", string(expect.rev), "at", string(current.Rev))
	}
	return false, nil
}

// write lands data at p atomically: staged in the same directory, then
// renamed over the target, so an interrupted write never truncates a document.
func (s *Store) write(p DocPath, data []byte) error {
	if dir := path.Dir(string(p)); dir != "." {
		if err := mkdirIn(s.root, dir, dirPerm); err != nil {
			return constants.ErrDocWrite.With(err, "path", string(p))
		}
	}
	tmp := string(p) + tempSuffix
	if err := writeFileIn(s.root, tmp, data, filePerm); err != nil {
		return constants.ErrDocWrite.With(err, "path", tmp)
	}
	if err := renameIn(s.root, tmp, string(p)); err != nil {
		_ = s.root.Remove(tmp)
		return constants.ErrDocWrite.With(err, "path", string(p))
	}
	return nil
}

// snapshotOf canonicalizes a parsed document into its served state.
func snapshotOf(doc tsvsheet.Document) Snapshot {
	return Snapshot{Doc: doc, Text: doc.Text(), Rev: tsvsheet.Revision(doc)}
}

// isRefusedPath reports whether the error is os.Root's refusal to resolve a
// name (an escape) or the OS refusing the name outright (an invalid byte).
func isRefusedPath(err error) bool {
	return errors.Is(err, fs.ErrInvalid) || errors.Is(err, syscall.EINVAL) ||
		errors.Is(err, syscall.ENOTDIR) || strings.Contains(errText(err), escapeMarker)
}

// escapeMarker is what os.Root reports when a symlink leaves the root; the
// package exports no sentinel for it.
const escapeMarker = "escapes from parent"

// errText is err's message, or "" for no error.
func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// validate refuses a path that is not a clean relative path inside the root,
// or that carries the HTTP binding's reserved "!" reference marker. os.Root
// enforces containment at the syscall level; this check makes the refusal a
// typed sentinel instead of a filesystem error.
func validate(p DocPath) error {
	s := string(p)
	clean := path.Clean(s)
	ok := s != "" && clean == s && clean != "." && !path.IsAbs(s) &&
		clean != ".." && !strings.HasPrefix(clean, "../") && !strings.Contains(s, "!")
	if !ok {
		return constants.ErrDocPath.With(nil, "path", s)
	}
	return nil
}

// The filesystem operations a mutation performs, as package vars so a test
// can force each failure — Go offers no way to make a real write, rename, or
// remove fail at a direct call site. These stubs are process-global, so tests
// that swap them restore them and run serially.
var (
	writeFileIn = func(root *os.Root, name string, data []byte, perm os.FileMode) error {
		return root.WriteFile(name, data, perm)
	}
	renameIn = func(root *os.Root, from, to string) error { return root.Rename(from, to) }
	removeIn = func(root *os.Root, name string) error { return root.Remove(name) }
	mkdirIn  = func(root *os.Root, dir string, perm os.FileMode) error { return root.MkdirAll(dir, perm) }
)

// filePerm is the mode for a stored document; dirPerm for created directories.
const (
	filePerm   = 0o600
	dirPerm    = 0o700
	tempSuffix = ".tsvsheet-api-staged"
)
