// The filesystem mechanics behind the embedded port: reading and parsing a
// stored document, checking a write's precondition, and landing bytes
// atomically inside the confined root.
package store

import (
	"errors"
	"io/fs"
	"os"
	"path"
	"strings"
	"syscall"

	tsvsheet "github.com/tsvsheet/go-tsvsheet"

	"github.com/tsvsheet/tsvsheet.api/document"
)

// load reads and parses the document at p under the held lock.
func (s *Store) load(p DocPath) (tsvsheet.Document, Snapshot, error) {
	if err := document.Validate(p); err != nil {
		return tsvsheet.Document{}, Snapshot{}, err
	}
	if err := s.vetFile(p); err != nil {
		return tsvsheet.Document{}, Snapshot{}, err
	}
	raw, err := s.root.ReadFile(string(p))
	if errors.Is(err, fs.ErrNotExist) || isRefusedPath(err) {
		// A path os.Root refuses — an escaping symlink, a name the OS will not
		// open — is a client-named path that resolves to nothing servable, so
		// it is "not found", not a server fault. Reporting it as 500 would both
		// blame the server and disclose that the escaping name exists.
		return tsvsheet.Document{}, Snapshot{}, document.ErrMissing.With(err, "path", string(p))
	}
	if err != nil {
		return tsvsheet.Document{}, Snapshot{}, ErrRead.With(err, "path", string(p))
	}
	doc, err := parseStored(raw, s.limits, p)
	if err != nil {
		return tsvsheet.Document{}, Snapshot{}, err
	}
	return doc, snapshotOf(doc), nil
}

// parseStored maps the bounded parse's outcomes for a stored document: an
// over-budget refusal passes through as the sentinel the API maps to its own
// documented status (the pre-flight vets the file first, but the bytes just
// read are what parse — a file that grew in between still refuses here);
// any other failure is a corrupt store, ErrParse naming the path.
func parseStored(raw []byte, limits tsvsheet.Limits, p DocPath) (tsvsheet.Document, error) {
	doc, err := tsvsheet.ParseDocumentWith(raw, limits)
	if errors.Is(err, tsvsheet.ErrDocTooLarge) {
		return tsvsheet.Document{}, err
	}
	if err != nil {
		return tsvsheet.Document{}, ErrParse.With(err, "path", string(p))
	}
	return doc, nil
}

// vetFile census-vets the stored file through ReadAt BEFORE buffering it
// (spec 018): an over-budget document refuses in O(index) memory instead of
// holding a whole-file transient just to say no —
// TestStore_RefusesOverBudgetDocuments discriminates this by the
// path-naming refusal only the pre-flight builds. Stat/open failures fall
// through to the read path so its own error mapping (missing, refused,
// unreadable) stays the single voice.
func (s *Store) vetFile(p DocPath) error {
	f, err := s.root.Open(string(p))
	if err != nil {
		return nil // the read path reports open failures with its own mapping
	}
	defer func() { _ = f.Close() }()
	var size int64
	if info, statErr := f.Stat(); statErr == nil {
		size = info.Size() // a failed stat leaves size 0: a trivially in-budget census, deferring to the read path
	}
	census, err := tsvsheet.Census(tsvsheet.ByteSource{ReadAt: f, Size: size})
	if err != nil {
		return nil // a scan refusal re-surfaces from the parse with ErrParse context
	}
	if census.Cells > s.limits.EffectiveResidentCells() {
		return tsvsheet.ErrDocTooLarge.With(nil,
			"cells", census.Cells, "budget", s.limits.EffectiveResidentCells(), "path", string(p))
	}
	return nil
}

// check verifies a Put precondition under the held lock, reporting whether the
// write will create the document.
func (s *Store) check(p DocPath, expect Expect) (Created, error) {
	_, current, err := s.load(p)
	missing := errors.Is(err, document.ErrMissing)
	if err != nil && !missing {
		return false, err
	}
	want, isAbsent := expect.Revision()
	if isAbsent {
		if !missing {
			return false, document.ErrExists.With(nil, "path", string(p))
		}
		return true, nil
	}
	if missing {
		return false, err
	}
	if current.Rev != want {
		return false, document.ErrPrecondition.With(nil, "expect", string(want), "at", string(current.Rev))
	}
	return false, nil
}

// write lands data at p atomically: staged in the same directory, then
// renamed over the target, so an interrupted write never truncates a document.
func (s *Store) write(p DocPath, data []byte) error {
	if err := s.ensureDir(p); err != nil {
		return err
	}
	tmp := string(p) + tempSuffix
	if err := writeFileIn(s.root, tmp, data, filePerm); err != nil {
		return ErrWrite.With(err, "path", tmp)
	}
	if err := renameIn(s.root, tmp, string(p)); err != nil {
		_ = s.root.Remove(tmp)
		return ErrWrite.With(err, "path", string(p))
	}
	return nil
}

// ensureDir creates the document's parent directory when it has one.
//
// A directory the filesystem will not create is a client-named path that
// resolves to nothing writable — the same judgement load makes for reads.
// Reporting it as a server fault would blame the server and echo raw syscall
// text back to whoever named the path. See
// TestConformanceARefusedWritePathIsMissingNotAServerFault.
func (s *Store) ensureDir(p DocPath) error {
	dir := path.Dir(string(p))
	if dir == "." {
		return nil
	}
	err := mkdirIn(s.root, dir, dirPerm)
	switch {
	case err == nil:
		return nil
	case isRefusedPath(err):
		return document.ErrMissing.With(document.ErrPath, "path", string(p))
	default:
		return ErrWrite.With(err, "path", string(p))
	}
}

// snapshotOf canonicalizes a parsed document into its served state.
func snapshotOf(doc tsvsheet.Document) Snapshot {
	return Snapshot{Doc: doc, Text: doc.Text(), Rev: tsvsheet.Revision(doc)}
}

// isRefusedPath reports whether the error is os.Root's refusal to resolve a
// name (an escape), the OS refusing the name outright (an invalid byte), or a
// name with no place to exist because a component is not a directory. See
// TestEscapingSymlinkIsMissingNotAServerFault and TestInvalidNameIsMissing
// assert these, since none is expressible as an exported sentinel.
func isRefusedPath(err error) bool {
	return errors.Is(err, fs.ErrInvalid) || errors.Is(err, syscall.EINVAL) ||
		errors.Is(err, syscall.ENOTDIR) || errors.Is(err, fs.ErrExist) ||
		strings.Contains(errText(err), escapeMarker)
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
