// Package store is the confined document store of the sheet API's document
// plane: .tsvt files under one operator-fixed root, read and replaced through
// os.Root so no path can escape, every write atomic (temp + rename), every
// mutation revision-checked, and every stored byte the engine's canonical
// serialization. The store never evaluates an expression — parsing for
// canonicalization and reference rewriting only.
package store

import (
	"context"
	"os"
	"sync"

	tsvsheet "github.com/tsvsheet/go-tsvsheet"

	"github.com/tsvsheet/tsvsheet.api/document"
)

// The document vocabulary this store speaks; the contract lives in the
// document package so a caller matches one set of types and errors whether it
// holds this embedded store or a client over HTTP.
type (
	DocPath  = document.DocPath
	Snapshot = document.Snapshot
	Applied  = document.Applied
	Expect   = document.Expect
	Created  = document.Created
)

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
		return nil, ErrRootOpen.With(err, "dir", string(dir))
	}
	return &Store{root: root, limits: limits}, nil
}

// Get reads the document at p, canonicalized: the returned text is the
// engine's serialization of the stored file, and the revision addresses those
// canonical bytes.
func (s *Store) Get(_ context.Context, p DocPath) (Snapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, snap, err := s.load(p)
	return snap, err
}

// Put replaces (or, with ExpectAbsent, creates) the document at p from body,
// storing the canonical serialization. It reports whether the document was
// created.
func (s *Store) Put(_ context.Context, p DocPath, body []byte, expect Expect) (Snapshot, Created, error) {
	// The path is checked before anything else: a refusal that only surfaced
	// from a later read would be classified as "missing" by check, and a
	// create-only write would then proceed against the aliased target.
	if err := document.Validate(p); err != nil {
		return Snapshot{}, false, err
	}
	if expect.IsZero() {
		return Snapshot{}, false, document.ErrPrecondition.With(nil, "path", string(p), "expect", "none")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	doc, err := tsvsheet.ParseDocument(body)
	if err != nil {
		return Snapshot{}, false, document.ErrSyntax.With(err, "path", string(p))
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
func (s *Store) Apply(
	_ context.Context,
	p DocPath,
	batch tsvsheet.Edits,
	expect tsvsheet.RevisionHex,
) (Applied, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	doc, old, err := s.load(p)
	if err != nil {
		return Applied{}, err
	}
	if old.Rev != expect {
		return Applied{}, document.ErrPrecondition.With(nil, "expect", string(expect), "at", string(old.Rev))
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
func (s *Store) Delete(_ context.Context, p DocPath, expect tsvsheet.RevisionHex) error {
	if err := document.Validate(p); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, snap, err := s.load(p)
	if err != nil {
		return err
	}
	if snap.Rev != expect {
		return document.ErrPrecondition.With(nil, "expect", string(expect), "at", string(snap.Rev))
	}
	if err := removeIn(s.root, string(p)); err != nil {
		return ErrWrite.With(err, "path", string(p))
	}
	return nil
}
