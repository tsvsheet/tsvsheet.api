package api

import (
	"encoding/json"
	"net/http"
)

// ProblemBase prefixes every problem type URI (RFC 9457). It is exported
// because a client reads the type URI to tell one refusal from another.
const ProblemBase = "https://tsvsheet.com/problems/"

// ProblemSlug names one error condition in a problem type URI. A client maps
// these back to the document plane's sentinels, so they are part of the wire
// contract rather than an implementation detail.
type ProblemSlug = problemSlug

// problemSlug names one error condition in a problem type URI.
type problemSlug string

// The API's problem vocabulary. Each condition a client must be able to act
// on differently gets its own slug: a client port reconstructs the document
// plane's sentinels from these, so two conditions sharing a slug would make
// the remote adapter return a different error than the embedded one for the
// same cause.
// The exported names for the slugs a client acts on.
const (
	SlugNotFound     = problemNotFound
	SlugPrecondition = problemPrecondition
	SlugConflict     = problemConflict
	SlugStaleBase    = problemStaleBase
	SlugRefusedEdits = problemRefusedEdits
	SlugBadDocument  = problemBadDocument
	SlugDocTooLarge  = problemDocTooLarge
	SlugBadEdits     = problemBadEdits
)

const (
	problemNotFound      problemSlug = "not-found"
	problemPrecondition  problemSlug = "precondition-failed"
	problemPrecondReq    problemSlug = "precondition-required"
	problemBadRequest    problemSlug = "bad-request"
	problemBadEdits      problemSlug = "bad-edits"
	problemRefusedEdits  problemSlug = "refused-edits"
	problemStaleBase     problemSlug = "stale-base"
	problemBadDocument   problemSlug = "bad-document"
	problemDocTooLarge   problemSlug = "document-too-large"
	problemConflict      problemSlug = "conflict"
	problemUnsupported   problemSlug = "unsupported-media-type"
	problemNotAcceptable problemSlug = "not-acceptable"
	problemInternal      problemSlug = "internal"
)

// httpStatus is an HTTP response status code.
type httpStatus int

// problemDetail is a problem response's human-readable detail.
type problemDetail string

// problemBody is the RFC 9457 response document.
type problemBody struct {
	Type   string `json:"type"`
	Title  string `json:"title"`
	Detail string `json:"detail,omitempty"`
	Status int    `json:"status"`
}

// writeProblem emits one RFC 9457 problem response.
func writeProblem(w http.ResponseWriter, status httpStatus, slug problemSlug, detail problemDetail) {
	w.Header().Set("Content-Type", TypeProblem)
	w.WriteHeader(int(status))
	body := problemBody{
		Type:   ProblemBase + string(slug),
		Title:  http.StatusText(int(status)),
		Status: int(status),
		Detail: string(detail),
	}
	_ = json.NewEncoder(w).Encode(body)
}
