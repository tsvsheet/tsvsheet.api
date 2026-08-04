// Package api is the sheet API's HTTP surface: one URL per document, the
// action selected by method × media type. The document plane (store, version,
// apply, notify) evaluates no expression; the compute plane — enabled when the
// operator turns it on — serves computed values in the SPECIFICATION §9 vendor
// types, making every served sheet an IMPORT*-able origin.
package api

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	tsvsheet "github.com/tsvsheet/go-tsvsheet"

	"github.com/tsvsheet/tsvsheet.api/document"
)

// ComputePlane selects whether the server carries the engine's compute plane.
type ComputePlane bool

// The two plane configurations, named at the call sites.
const (
	DocumentPlaneOnly ComputePlane = false
	WithComputePlane  ComputePlane = true
)

// Clock supplies the compute plane's evaluation time, injected so volatile
// functions are deterministic under test.
type Clock func() time.Time

// Config assembles a handler. Observability is deliberately absent: it is a
// decorator a server applies (observe.Handler), not a handler feature — see
// R17 and the observe package.
type Config struct {
	Port           document.Port
	Clock          Clock
	Limits         tsvsheet.Limits
	ComputeEnabled ComputePlane
}

// maxBodyBytes bounds any request body.
const maxBodyBytes = 4 << 20

// Handler is the sheet API's http.Handler.
//
// Pointer receivers are necessary: the handler owns the feed hub and the
// idempotency replay cache, which carry mutexes and shared maps.
type Handler struct {
	hub    *hub
	replay *replayCache
	config Config
}

// NewHandler builds the API handler over its store.
func NewHandler(config Config) Handler {
	return Handler{hub: newHub(), replay: newReplayCache(), config: config}
}

// ServeHTTP routes one request: the path names a document (optionally with a
// `!` reference suffix), the method and media types name the action.
func (handler Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Tsvsheet-Capabilities", handler.capabilities())
	doc, ref := splitDocRef(requestPath(r.URL.Path))
	if doc == "" {
		writeProblem(w, http.StatusNotFound, problemNotFound, "no document named")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	handler.route(w, r, doc, ref)
}

// route dispatches by method.
func (handler Handler) route(w http.ResponseWriter, r *http.Request, doc document.DocPath, ref string) {
	// A reference names a computed projection, which only a read can serve.
	// Accepting it on a mutation would let a cell URL replace or delete the
	// whole document, so it is refused rather than silently ignored.
	if ref != "" && r.Method != http.MethodGet && r.Method != http.MethodHead {
		writeProblem(w, http.StatusMethodNotAllowed, problemBadRequest, "a reference is read-only")
		return
	}
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		handler.get(w, r, doc, ref)
	case http.MethodPut:
		handler.put(w, r, doc)
	case http.MethodPost:
		handler.post(w, r, doc)
	case http.MethodDelete:
		handler.delete(w, r, doc)
	case http.MethodOptions:
		w.Header().Set("Allow", allowedMethods)
		w.WriteHeader(http.StatusNoContent)
	default:
		detail := problemDetail(r.Method + " is not part of the sheet API")
		writeProblem(w, http.StatusMethodNotAllowed, problemBadRequest, detail)
	}
}

// allowedMethods is the OPTIONS Allow surface.
const allowedMethods = "GET, HEAD, PUT, POST, DELETE, OPTIONS"

// capabilities names what this deployment serves.
func (handler Handler) capabilities() string {
	if handler.config.ComputeEnabled == WithComputePlane {
		return "edits, events, compute"
	}
	return "edits, events"
}

// readBody reads a bounded request body; an over-limit read is 413, any other
// read failure 400. The second result reports success.
func readBody(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	body, err := io.ReadAll(r.Body)
	if err == nil {
		return body, true
	}
	var tooBig *http.MaxBytesError
	if errors.As(err, &tooBig) {
		writeProblem(w, http.StatusRequestEntityTooLarge, problemBadRequest, "body exceeds the request limit")
	} else {
		writeProblem(w, http.StatusBadRequest, problemBadRequest, "body unreadable")
	}
	return nil, false
}

// ifMatchRevision reads the mandatory If-Match validator: absent is 428,
// anything but one quoted strong revision is 400.
func ifMatchRevision(r *http.Request) (tsvsheet.RevisionHex, httpStatus, error) {
	header := r.Header.Get("If-Match")
	if header == "" {
		return "", http.StatusPreconditionRequired, document.ErrPrecondition.With(nil, "missing", "If-Match")
	}
	rev, ok := strings.CutPrefix(header, `"`)
	rev, ok2 := strings.CutSuffix(rev, `"`)
	if !ok || !ok2 || rev == "" {
		return "", http.StatusBadRequest, document.ErrPrecondition.With(nil, "malformed", header)
	}
	return tsvsheet.RevisionHex(rev), 0, nil
}

// quote renders a revision as its strong entity tag.
func quote(rev tsvsheet.RevisionHex) string { return `"` + string(rev) + `"` }

// problemOf maps a precondition status to its slug.
func problemOf(status httpStatus) problemSlug {
	if status == http.StatusPreconditionRequired {
		return problemPrecondReq
	}
	return problemBadRequest
}

// writeError maps a sentinel to its problem response.
func (handler Handler) writeError(w http.ResponseWriter, err error) {
	for _, mapping := range errorMap {
		if errors.Is(err, mapping.is) {
			writeProblem(w, mapping.status, mapping.slug, problemDetail(err.Error()))
			return
		}
	}
	writeProblem(w, http.StatusInternalServerError, problemInternal, problemDetail(err.Error()))
}

// errorMapping binds one sentinel to its problem slug and status.
type errorMapping struct {
	is     error
	slug   problemSlug
	status httpStatus
}

// errorMap orders the sentinel-to-status mappings; first match wins, so the
// more specific edits sentinels precede the general ones.
var errorMap = []errorMapping{
	{is: document.ErrMissing, slug: problemNotFound, status: http.StatusNotFound},
	{is: document.ErrPath, slug: problemNotFound, status: http.StatusNotFound},
	{is: document.ErrPrecondition, slug: problemPrecondition, status: http.StatusPreconditionFailed},
	{is: document.ErrExists, slug: problemConflict, status: http.StatusConflict},
	{is: document.ErrSyntax, slug: problemBadDocument, status: http.StatusUnprocessableEntity},
	{is: tsvsheet.ErrDocTooLarge, slug: problemDocTooLarge, status: http.StatusRequestEntityTooLarge},
	{is: tsvsheet.ErrEditsBase, slug: problemStaleBase, status: http.StatusUnprocessableEntity},
	{is: tsvsheet.ErrEditsApply, slug: problemRefusedEdits, status: http.StatusUnprocessableEntity},
}

// requestPath is a request URL path.
type requestPath string

// splitDocRef separates the document path from a `!` reference suffix.
func splitDocRef(urlPath requestPath) (document.DocPath, string) {
	trimmed := strings.TrimPrefix(string(urlPath), "/")
	doc, ref, _ := strings.Cut(trimmed, "!")
	return document.DocPath(doc), ref
}
