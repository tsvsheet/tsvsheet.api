// Package constants declares tsvsheet.api's sentinel error values. The error
// mechanism (the matchable string type) lives in the shared gomatic/go-error
// library; these values are this package's own.
package constants

import errs "github.com/gomatic/go-error"

// Keep these constants sorted alphabetically.
const (
	ErrDocExists   errs.Const = "document already exists"
	ErrDocMissing  errs.Const = "document not found"
	ErrDocParse    errs.Const = "stored document does not parse"
	ErrDocPath     errs.Const = "document path refused"
	ErrDocRead     errs.Const = "failed to read document"
	ErrDocSyntax   errs.Const = "document body does not parse"
	ErrDocWrite    errs.Const = "failed to write document"
	ErrListen      errs.Const = "failed to bind the api server"
	ErrNotLoopback errs.Const = "refusing a non-loopback bind"
	ErrPrecond     errs.Const = "revision precondition failed"
	ErrRootOpen    errs.Const = "document root cannot be opened"
)
