// The document plane's error contract. These sentinels are part of the Port
// interface, not of any one implementation: an embedded port returns them
// directly and a client port reconstructs them from the wire, so a caller
// matches one set of errors regardless of where the document lives.
package document

import errs "github.com/gomatic/go-error"

// Keep these constants sorted alphabetically.
const (
	// ErrExists is a create refused because the document is already there.
	ErrExists errs.Const = "document already exists"
	// ErrMissing is a read, edit, or delete of a document that is not there.
	ErrMissing errs.Const = "document not found"
	// ErrPath is a document path the port refuses to resolve.
	ErrPath errs.Const = "document path refused"
	// ErrPrecondition is a mutation whose expected revision is not the
	// document's current one — the conflict detection every writer relies on.
	ErrPrecondition errs.Const = "revision precondition failed"
	// ErrSyntax is a whole-document body that does not parse as a .tsvt.
	ErrSyntax errs.Const = "document body does not parse"
	// ErrUnavailable is a port that could not be reached or answered
	// unintelligibly — a transport fault, never a statement about the document.
	ErrUnavailable errs.Const = "document port unavailable"
)
