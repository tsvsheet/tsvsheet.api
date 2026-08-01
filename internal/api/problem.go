package api

import (
	"encoding/json"
	"net/http"
)

// problemBase prefixes every problem type URI (RFC 9457).
const problemBase = "https://tsvsheet.com/problems/"

// problemSlug names one error condition in a problem type URI.
type problemSlug string

// The API's problem vocabulary.
const (
	problemNotFound      problemSlug = "not-found"
	problemPrecondition  problemSlug = "precondition-failed"
	problemPrecondReq    problemSlug = "precondition-required"
	problemBadRequest    problemSlug = "bad-request"
	problemBadEdits      problemSlug = "bad-edits"
	problemRefusedEdits  problemSlug = "refused-edits"
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
		Type:   problemBase + string(slug),
		Title:  http.StatusText(int(status)),
		Status: int(status),
		Detail: string(detail),
	}
	_ = json.NewEncoder(w).Encode(body)
}
