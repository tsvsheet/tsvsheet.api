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

	tsvsheet "github.com/tsvsheet/go-tsvsheet"
)

// DocPath addresses a document within a port's namespace: a clean relative
// path, no traversal, no "!" (reserved by the HTTP binding's reference
// suffix).
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
// replace or create it whole, remove it. Every mutation is conditioned, so two
// writers cannot silently overwrite one another whichever implementation is in
// hand.
//
// Implementations return this package's sentinel errors (ErrMissing,
// ErrPrecondition, ErrExists, ErrPath) and the engine's own for a refused
// batch (tsvsheet.ErrEditsBase, tsvsheet.ErrEditsApply, tsvsheet.ErrSyntax),
// so a caller matches one set of errors regardless of where the document is.
type Port interface {
	Get(ctx context.Context, doc DocPath) (Snapshot, error)
	Apply(ctx context.Context, doc DocPath, batch tsvsheet.Edits, expect tsvsheet.RevisionHex) (Applied, error)
	Put(ctx context.Context, doc DocPath, body []byte, expect Expect) (Snapshot, Created, error)
	Delete(ctx context.Context, doc DocPath, expect tsvsheet.RevisionHex) error
}

// Change is one observed document change: the batch that caused it and the
// revisions it moved between.
type Change struct {
	Old   tsvsheet.RevisionHex
	New   tsvsheet.RevisionHex
	Batch tsvsheet.Edits
}

// Watcher is the optional liveness capability. It is a separate interface a
// caller type-asserts rather than a Port method, because an implementation
// that cannot observe changes must not advertise that it can: an embedded port
// over a local file has no feed, and answering "unsupported" from a method
// every caller must handle is worse than the capability being absent from the
// type.
type Watcher interface {
	// Watch delivers changes until the context is cancelled or the stream
	// ends. The returned channel is closed when no further change will arrive.
	Watch(ctx context.Context, doc DocPath) (<-chan Change, error)
}
