// The read half of the HTTP surface: every representation a GET can serve —
// the change feed, a computed reference, a computed grid, or the source
// document — and the caching headers that keep them distinguishable.
package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"

	tsvsheet "github.com/tsvsheet/go-tsvsheet"

	"github.com/tsvsheet/tsvsheet.api/document"
)

// get serves every GET representation: the change feed, a computed reference,
// a computed grid, or the source document.
func (handler Handler) get(w http.ResponseWriter, r *http.Request, doc document.DocPath, ref string) {
	accept := parseAccept(acceptHeader(r.Header.Get("Accept")))
	if accept.has(TypeStream) {
		handler.feed(w, r, doc)
		return
	}
	snap, err := handler.pinned(r.Context(), doc, r.URL.Query().Get("rev"))
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
func (handler Handler) pinned(ctx context.Context, doc document.DocPath, rev string) (document.Snapshot, error) {
	snap, err := handler.config.Port.Get(ctx, doc)
	if err != nil {
		return document.Snapshot{}, err
	}
	if rev != "" && rev != string(snap.Rev) {
		return document.Snapshot{}, document.ErrMissing.With(nil, "rev", rev)
	}
	return snap, nil
}

// feed subscribes the client to the document's event stream (the document
// must exist).
func (handler Handler) feed(w http.ResponseWriter, r *http.Request, doc document.DocPath) {
	snap, err := handler.config.Port.Get(r.Context(), doc)
	if err != nil {
		handler.writeError(w, err)
		return
	}
	handler.serveFeed(w, r, doc, string(snap.Rev))
}

// document serves the source or the whole computed grid per the Accept set.
// The hard rule lives here: a values Accept (the §9 grid types) is never
// answered with source bytes — without the compute plane it is 406.
func (handler Handler) document(w http.ResponseWriter, r *http.Request, snap document.Snapshot, accept acceptSet) {
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
func (handler Handler) computedRef(w http.ResponseWriter, snap document.Snapshot, ref string, accept acceptSet) {
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
// its tag, the value mutations are conditioned on, while each
// computed rendering gets a tag derived from the revision and its media type,
// so no two representations are interchangeable to a cache —
// TestRepresentationsCarryDistinctValidators asserts the three differ and all
// carry Vary. A volatile
// sheet's computed body may differ between two reads of one revision, so it
// carries no strong validator and is not stored at all.
func setValidator(w http.ResponseWriter, snap document.Snapshot, served mediaType) {
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
// media type: distinct per representation, so a cache has no way to substitute
// one for another. See TestRepresentationsCarryDistinctValidators.
func representationTag(rev tsvsheet.RevisionHex, served mediaType) string {
	sum := sha256.Sum256([]byte(string(rev) + "\x00" + string(served)))
	return `"` + hex.EncodeToString(sum[:]) + `"`
}

// computedGrid evaluates a snapshot at the configured clock, bounded by the
// operator's limits.
//
// The limits are not decoration: an adversarial pass found this path running
// on engine defaults while the edit path honoured the configured cap, so one
// operator setting meant two different ceilings depending on which request
// arrived. See TestComputeHonoursTheConfiguredLimits.
func (handler Handler) computedGrid(snap document.Snapshot) tsvsheet.Grid {
	return snap.Doc.Sheet().ComputeWith(tsvsheet.ComputeOptions{
		At:     handler.config.Clock(),
		Limits: handler.config.Limits,
	})
}

// body writes one full representation (HEAD elides the body).
func (handler Handler) body(
	w http.ResponseWriter,
	r *http.Request,
	served mediaType,
	snap document.Snapshot,
	text string,
) {
	w.Header().Set("Content-Type", string(served))
	setValidator(w, snap, served)
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		_, _ = io.WriteString(w, text)
	}
}
