// Package document states the sheet API's document plane as an interface, so
// a frontend edits a .tsvt the same way whether the document sits on the local
// filesystem or behind an HTTP server it addresses.
//
// The vocabulary is the edit language's (work order 011): a document is
// addressed by path, its state is content-addressed bytes, and every change is
// an edits batch conditioned on the revision it was authored against. Nothing
// here parses, computes, or serializes — that is the engine's, so the two
// implementations cannot drift in semantics.
//
// This package imports no transport. A Port implementation that reaches the
// network is a client; one that does not is embedded, and a frontend holding
// the embedded one starts no server and binds no address.
package document

import (
	"context"
	"path"
	"strings"

	tsvsheet "github.com/tsvsheet/go-tsvsheet"
)

// DocPath addresses a document within a port's namespace: a clean relative
// path naming a .tsvt file, with no traversal and no "!" (reserved by the HTTP
// binding's reference suffix).
type DocPath string

// Snapshot is one document state: the parsed document, its canonical bytes,
// and their content address. Carrying the parsed form means no consumer
// reparses text a port already read.
type Snapshot struct {
	Doc  tsvsheet.Document
	Rev  tsvsheet.RevisionHex
	Text []byte
}

// Applied is an edit batch's result: the state it folded from and the state it
// produced, so a caller can report or verify the transition it caused.
type Applied struct {
	Old Snapshot
	New Snapshot
}

// Created reports whether a write brought a document into existence.
type Created bool

// Expect is a write's precondition: a revision the document must currently
// have, or the requirement that it not exist.
type Expect struct {
	rev      tsvsheet.RevisionHex
	isAbsent bool
}

// ExpectRev requires the document to currently have revision rev.
func ExpectRev(rev tsvsheet.RevisionHex) Expect { return Expect{rev: rev} }

// ExpectAbsent requires the document to not exist (create).
func ExpectAbsent() Expect { return Expect{isAbsent: true} }

// Revision is the revision the precondition requires, and whether it instead
// requires absence. Implementations read a precondition through this rather
// than reaching into the value, so its shape stays this package's business.
func (e Expect) Revision() (tsvsheet.RevisionHex, Created) {
	return e.rev, Created(e.isAbsent)
}

// Port is the document plane: read a document, apply an edits batch to it,
// replace or create it whole, remove it. Every mutation is conditioned, so a
// second writer is refused rather than silently overwriting the first,
// whichever implementation is in hand. See the conformance suite, which
// asserts that for both.
//
// Implementations return this package's sentinel errors (ErrMissing,
// ErrPrecondition, ErrExists, ErrSyntax — a refused path is ErrMissing
// wrapping ErrPath) and the engine's own for a refused batch
// (tsvsheet.ErrEditsBase, tsvsheet.ErrEditsApply), so a caller matches one set
// of errors regardless of where the document is.
//
// Liveness is deliberately absent: the embedded implementation has no feed,
// and an interface method every caller must handle as "unsupported" is worse
// than a capability that is simply not here yet. It arrives as its own
// type-asserted interface when something implements it.
type Port interface {
	Get(ctx context.Context, doc DocPath) (Snapshot, error)
	Apply(ctx context.Context, doc DocPath, batch tsvsheet.Edits, expect tsvsheet.RevisionHex) (Applied, error)
	Put(ctx context.Context, doc DocPath, body []byte, expect Expect) (Snapshot, Created, error)
	Delete(ctx context.Context, doc DocPath, expect tsvsheet.RevisionHex) error
}

// Validate refuses a path that is not a clean relative .tsvt path within a
// port's namespace, or that carries the HTTP binding's reserved "!" reference
// marker.
//
// The extension is part of the rule, not a convention. A port is pointed at a
// directory, and every file under it would otherwise be readable and
// writable through the same requests — an operator who points one at a project
// checkout would be serving its dotfiles and keys, not its spreadsheets. A
// document plane for .tsvt files serves .tsvt files.
//
// The rule lives here rather than in an implementation because both adapters
// must apply it identically. An embedded port would otherwise refuse what a
// client silently rewrites: URL joining cleans "..", so an unvalidated
// traversal reaches a different document instead of being refused — the
// adapters would disagree about what a caller even addressed.
//
// A refusal is indistinguishable from a missing document on purpose. Whether a
// refused name exists is not a caller's business, and the HTTP binding answers
// 404 for both, so an embedded caller must see what a remote one sees; the
// specific cause rides along for a local caller that wants it.
func Validate(p DocPath) error {
	s := string(p)
	clean := path.Clean(s)
	ok := s != "" && clean == s && clean != "." && !path.IsAbs(s) &&
		clean != ".." && !strings.HasPrefix(clean, "../") && !strings.Contains(s, "!") &&
		strings.HasSuffix(s, DocExtension) && s != DocExtension
	if !ok {
		return ErrMissing.With(ErrPath, "path", s)
	}
	return nil
}

// DocExtension is the file extension a document plane serves. It is the
// language's own: a .tsvt file IS the spreadsheet.
const DocExtension = ".tsvt"

// IsZero reports whether the precondition was built by neither ExpectRev nor
// ExpectAbsent. A zero value states no expectation at all, and a mutation
// refuses it: writing without a precondition is the silent overwrite the plane
// exists to prevent. See TestExpectStatesOneRequirement and
// TestConformanceZeroPreconditionIsRefused.
func (e Expect) IsZero() bool { return e.rev == "" && !e.isAbsent }
