// The store's own failures — the mechanical ones a filesystem can produce.
// The document plane's contract errors (missing, precondition, exists, path,
// syntax) belong to the document package, because a client port must be able
// to return the same ones.
package store

import errs "github.com/gomatic/go-error"

// Keep these constants sorted alphabetically.
const (
	// ErrParse is a stored document that no longer parses — corruption or an
	// out-of-band edit, never something a request did.
	ErrParse errs.Const = "stored document does not parse"
	// ErrRead is a document that exists but could not be read.
	ErrRead errs.Const = "failed to read document"
	// ErrRootOpen is a document root that cannot be opened for confinement.
	ErrRootOpen errs.Const = "document root cannot be opened"
	// ErrWrite is a write that did not land; the previous document survives.
	ErrWrite errs.Const = "failed to write document"
)
