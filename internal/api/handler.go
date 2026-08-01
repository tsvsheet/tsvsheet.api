// Package api is the sheet API's HTTP surface: one URL per document, the
// action selected by method × media type. The document plane (store, version,
// apply, notify) evaluates no expression; the compute plane — enabled when the
// operator turns it on — serves computed values in the SPECIFICATION §9 vendor
// types, making every served sheet an IMPORT*-able origin.
package api

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	tsvsheet "github.com/tsvsheet/go-tsvsheet"

	"github.com/tsvsheet/tsvsheet.api/internal/constants"
	"github.com/tsvsheet/tsvsheet.api/internal/store"
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

// Config assembles a handler.
type Config struct {
	Store          *store.Store
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
func (handler Handler) route(w http.ResponseWriter, r *http.Request, doc store.DocPath, ref string) {
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

// get serves every GET representation: the change feed, a computed reference,
// a computed grid, or the source document.
func (handler Handler) get(w http.ResponseWriter, r *http.Request, doc store.DocPath, ref string) {
	accept := parseAccept(acceptHeader(r.Header.Get("Accept")))
	if accept.has(TypeStream) {
		handler.feed(w, r, doc)
		return
	}
	snap, err := handler.pinned(doc, r.URL.Query().Get("rev"))
	if err != nil {
		handler.writeError(w, err)
		return
	}
	if ref != "" {
		handler.computedRef(w, snap, ref, accept)
		return
	}
	handler.document(w, r, snap, accept)
}

// pinned loads the document, honouring a ?rev= pin: this server keeps no
// history, so only the head revision is addressable.
func (handler Handler) pinned(doc store.DocPath, rev string) (store.Snapshot, error) {
	snap, err := handler.config.Store.Get(doc)
	if err != nil {
		return store.Snapshot{}, err
	}
	if rev != "" && rev != string(snap.Rev) {
		return store.Snapshot{}, constants.ErrDocMissing.With(nil, "rev", rev)
	}
	return snap, nil
}

// feed subscribes the client to the document's event stream (the document
// must exist).
func (handler Handler) feed(w http.ResponseWriter, r *http.Request, doc store.DocPath) {
	snap, err := handler.config.Store.Get(doc)
	if err != nil {
		handler.writeError(w, err)
		return
	}
	handler.serveFeed(w, r, doc, string(snap.Rev))
}

// document serves the source or the whole computed grid per the Accept set.
// The hard rule lives here: a values Accept (the §9 grid types) is never
// answered with source bytes — without the compute plane it is 406.
func (handler Handler) document(w http.ResponseWriter, r *http.Request, snap store.Snapshot, accept acceptSet) {
	if accept.has(TypeDoc) || (accept.wildcard() && !accept.hasAny(TypeSheet, TypeTSV, TypeCSV)) {
		handler.body(w, r, TypeDoc, snap, string(snap.Text))
		return
	}
	if !accept.hasAny(TypeSheet, TypeTSV, TypeCSV) || handler.config.ComputeEnabled == DocumentPlaneOnly {
		writeProblem(w, http.StatusNotAcceptable, problemNotAcceptable, "no servable representation")
		return
	}
	grid := handler.computedGrid(snap)
	switch {
	case accept.has(TypeSheet):
		handler.body(w, r, TypeSheet, snap, renderGridTSV(grid))
	case accept.has(TypeTSV):
		handler.body(w, r, TypeTSV, snap, renderGridTSV(grid))
	default:
		handler.body(w, r, TypeCSV, snap, renderGridCSV(grid))
	}
}

// computedRef serves one computed reference read in its §9 shape type.
func (handler Handler) computedRef(w http.ResponseWriter, snap store.Snapshot, ref string, accept acceptSet) {
	if handler.config.ComputeEnabled == DocumentPlaneOnly {
		writeProblem(w, http.StatusNotAcceptable, problemNotAcceptable, "this deployment has no compute plane")
		return
	}
	span, err := parseRef(refText(ref))
	if err != nil {
		handler.writeError(w, err)
		return
	}
	served := span.shapeOf().typeOf()
	if !accept.wildcard() && !accept.has(string(served)) {
		detail := problemDetail("reference shape serves " + string(served))
		writeProblem(w, http.StatusNotAcceptable, problemNotAcceptable, detail)
		return
	}
	w.Header().Set("Content-Type", string(served))
	setValidator(w, snap, served)
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, renderSpan(handler.computedGrid(snap), span))
}

// mediaType is a response representation's media type.
type mediaType string

// setValidator writes the caching headers for one representation.
//
// One URL serves the source and several computed renderings, so `Vary: Accept`
// is mandatory: without it a shared cache keyed on the URL alone could hand a
// values client the source body, defeating in one hop the source/computed rule
// the handler enforces in-process. The source keeps the document revision as
// its tag — mutations are conditioned on exactly that value — while each
// computed rendering gets a tag derived from the revision and its media type,
// so no two representations are ever interchangeable to a cache. A volatile
// sheet's computed body may differ between two reads of one revision, so it
// carries no strong validator and is not stored at all.
func setValidator(w http.ResponseWriter, snap store.Snapshot, served mediaType) {
	w.Header().Set("Vary", "Accept")
	if served == TypeDoc {
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("ETag", quote(snap.Rev))
		return
	}
	if snap.Doc.Sheet().IsVolatile() {
		w.Header().Set("Cache-Control", "no-store")
		return
	}
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("ETag", representationTag(snap.Rev, served))
}

// representationTag is the strong validator for one revision rendered as one
// media type — distinct per representation, so a cache can never substitute
// one for another.
func representationTag(rev tsvsheet.RevisionHex, served mediaType) string {
	sum := sha256.Sum256([]byte(string(rev) + "\x00" + string(served)))
	return `"` + hex.EncodeToString(sum[:]) + `"`
}

// computedGrid evaluates a snapshot at the configured clock.
func (handler Handler) computedGrid(snap store.Snapshot) tsvsheet.Grid {
	return snap.Doc.Sheet().ComputeAt(handler.config.Clock())
}

// body writes one full representation (HEAD elides the body).
func (handler Handler) body(
	w http.ResponseWriter,
	r *http.Request,
	served mediaType,
	snap store.Snapshot,
	text string,
) {
	w.Header().Set("Content-Type", string(served))
	setValidator(w, snap, served)
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		_, _ = io.WriteString(w, text)
	}
}

// put replaces or creates the document from a source body.
func (handler Handler) put(w http.ResponseWriter, r *http.Request, doc store.DocPath) {
	if mediaBase(mediaExpr(r.Header.Get("Content-Type"))) != TypeDoc {
		writeProblem(w, http.StatusUnsupportedMediaType, problemUnsupported, "PUT bodies are "+TypeDoc)
		return
	}
	expect, status, err := putExpectation(r)
	if err != nil {
		writeProblem(w, status, problemOf(status), problemDetail(err.Error()))
		return
	}
	body, ok := readBody(w, r)
	if !ok {
		return
	}
	snap, created, err := handler.config.Store.Put(doc, body, expect)
	if err != nil {
		handler.writeError(w, err)
		return
	}
	handler.hub.broadcast(doc, eventReset, metaRev+"\t"+string(snap.Rev))
	w.Header().Set("ETag", quote(snap.Rev))
	w.WriteHeader(statusOfPut(docCreated(created)))
}

// docCreated reports whether a PUT created its document.
type docCreated bool

// statusOfPut maps creation to its status code.
func statusOfPut(isCreated docCreated) int {
	if isCreated {
		return http.StatusCreated
	}
	return http.StatusOK
}

// putExpectation reads PUT's precondition: If-Match names the revision to
// replace, If-None-Match: * requires creation; neither is 428.
func putExpectation(r *http.Request) (store.Expect, httpStatus, error) {
	if r.Header.Get("If-None-Match") == "*" {
		return store.ExpectAbsent(), 0, nil
	}
	rev, status, err := ifMatchRevision(r)
	if err != nil {
		return store.Expect{}, status, err
	}
	return store.ExpectRev(rev), 0, nil
}

// post applies an edits batch.
func (handler Handler) post(w http.ResponseWriter, r *http.Request, doc store.DocPath) {
	if mediaBase(mediaExpr(r.Header.Get("Content-Type"))) != TypeEdits {
		writeProblem(w, http.StatusUnsupportedMediaType, problemUnsupported, "POST bodies are "+TypeEdits)
		return
	}
	rev, status, err := ifMatchRevision(r)
	if err != nil {
		writeProblem(w, status, problemOf(status), problemDetail(err.Error()))
		return
	}
	body, ok := readBody(w, r)
	if !ok {
		return
	}
	if handler.replayed(w, r, doc, body) {
		return
	}
	handler.apply(w, r, doc, body, rev)
}

// apply parses and folds the batch, records the outcome, and broadcasts.
func (handler Handler) apply(
	w http.ResponseWriter,
	r *http.Request,
	doc store.DocPath,
	body []byte,
	rev tsvsheet.RevisionHex,
) {
	batch, err := tsvsheet.ParseEdits(body)
	if err != nil {
		writeProblem(w, http.StatusBadRequest, problemBadEdits, problemDetail(err.Error()))
		return
	}
	applied, err := handler.config.Store.Apply(doc, batch, rev)
	if err != nil {
		handler.writeError(w, err)
		return
	}
	handler.remember(r, doc, applied.New.Rev, body)
	handler.announce(doc, applied, body)
	w.Header().Set("ETag", quote(applied.New.Rev))
	w.WriteHeader(http.StatusNoContent)
}

// announce broadcasts the applied batch (and, with the compute plane, the
// recomputed-cells delta).
func (handler Handler) announce(doc store.DocPath, applied store.Applied, body []byte) {
	payload := metaBase + "\t" + string(applied.Old.Rev) + "\n" +
		metaRev + "\t" + string(applied.New.Rev) + "\n" + string(body)
	handler.hub.broadcast(doc, eventChanged, payload)
	if handler.config.ComputeEnabled == DocumentPlaneOnly {
		return
	}
	delta := cellsDelta(handler.computedGrid(applied.Old), handler.computedGrid(applied.New))
	if delta != "" {
		handler.hub.broadcast(doc, eventComputed, metaRev+"\t"+string(applied.New.Rev)+"\n"+delta)
	}
}

// delete removes the document and ends its feed.
func (handler Handler) delete(w http.ResponseWriter, r *http.Request, doc store.DocPath) {
	rev, status, err := ifMatchRevision(r)
	if err != nil {
		writeProblem(w, status, problemOf(status), problemDetail(err.Error()))
		return
	}
	if err := handler.config.Store.Delete(doc, rev); err != nil {
		handler.writeError(w, err)
		return
	}
	handler.hub.broadcast(doc, eventDeleted, "")
	w.WriteHeader(http.StatusNoContent)
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
		return "", http.StatusPreconditionRequired, constants.ErrPrecond.With(nil, "missing", "If-Match")
	}
	rev, ok := strings.CutPrefix(header, `"`)
	rev, ok2 := strings.CutSuffix(rev, `"`)
	if !ok || !ok2 || rev == "" {
		return "", http.StatusBadRequest, constants.ErrPrecond.With(nil, "malformed", header)
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
	{is: constants.ErrDocMissing, slug: problemNotFound, status: http.StatusNotFound},
	{is: constants.ErrDocPath, slug: problemNotFound, status: http.StatusNotFound},
	{is: constants.ErrPrecond, slug: problemPrecondition, status: http.StatusPreconditionFailed},
	{is: constants.ErrDocExists, slug: problemConflict, status: http.StatusConflict},
	{is: constants.ErrDocSyntax, slug: problemRefusedEdits, status: http.StatusUnprocessableEntity},
	{is: tsvsheet.ErrEditsBase, slug: problemRefusedEdits, status: http.StatusUnprocessableEntity},
	{is: tsvsheet.ErrEditsApply, slug: problemRefusedEdits, status: http.StatusUnprocessableEntity},
}

// requestPath is a request URL path.
type requestPath string

// splitDocRef separates the document path from a `!` reference suffix.
func splitDocRef(urlPath requestPath) (store.DocPath, string) {
	trimmed := strings.TrimPrefix(string(urlPath), "/")
	doc, ref, _ := strings.Cut(trimmed, "!")
	return store.DocPath(doc), ref
}

// replayOutcome is what one Idempotency-Key produced: the revision it left
// behind and a digest of the batch it was used for.
type replayOutcome struct {
	rev  tsvsheet.RevisionHex
	body [sha256.Size]byte
}

// replayCache remembers each Idempotency-Key's outcome so a retried POST
// returns its original result instead of re-applying (or failing a stale
// precondition). Bounded: when full, the cache resets — a replay after that
// simply re-runs the normal precondition path.
type replayCache struct {
	seen map[string]replayOutcome
	mu   sync.Mutex
}

// replayCap bounds the cache.
const replayCap = 1024

// newReplayCache builds an empty cache.
func newReplayCache() *replayCache { return &replayCache{seen: map[string]replayOutcome{}} }

// replayed answers a remembered key with its original outcome, or refuses the
// key when it was first used for a different batch. A replay must be a repeat
// of the same request: answering a *new* batch with an old result would drop
// the edit and report success, which the client cannot detect.
func (handler Handler) replayed(w http.ResponseWriter, r *http.Request, doc store.DocPath, body []byte) bool {
	key := r.Header.Get("Idempotency-Key")
	if key == "" {
		return false
	}
	handler.replay.mu.Lock()
	prior, ok := handler.replay.seen[replayKey(doc, idempotencyKey(key))]
	handler.replay.mu.Unlock()
	if !ok {
		return false
	}
	if prior.body != bodyDigest(body) {
		writeProblem(
			w,
			http.StatusUnprocessableEntity,
			problemBadRequest,
			"idempotency key reused for a different batch",
		)
		return true
	}
	w.Header().Set("ETag", quote(prior.rev))
	w.WriteHeader(http.StatusNoContent)
	return true
}

// idempotencyKey is a client-supplied retry key.
type idempotencyKey string

// replayKey scopes a key to its document, so one client's key on one sheet can
// never answer a request against another.
func replayKey(doc store.DocPath, key idempotencyKey) string {
	return string(doc) + "\x00" + string(key)
}

// bodyDigest content-addresses a request body, so a reused key is checked
// against what it was first used for without retaining the batch.
func bodyDigest(body []byte) [sha256.Size]byte { return sha256.Sum256(body) }

// remember records a successful application under its Idempotency-Key.
func (handler Handler) remember(r *http.Request, doc store.DocPath, rev tsvsheet.RevisionHex, body []byte) {
	key := r.Header.Get("Idempotency-Key")
	if key == "" {
		return
	}
	handler.replay.mu.Lock()
	defer handler.replay.mu.Unlock()
	if len(handler.replay.seen) >= replayCap {
		handler.replay.seen = map[string]replayOutcome{}
	}
	handler.replay.seen[replayKey(doc, idempotencyKey(key))] = replayOutcome{rev: rev, body: bodyDigest(body)}
}
