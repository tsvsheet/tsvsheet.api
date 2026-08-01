// The write half of the HTTP surface: replacing or creating a document,
// applying an edits batch, deleting, and the preconditions every one of them
// requires.
package api

import (
	"net/http"

	tsvsheet "github.com/tsvsheet/go-tsvsheet"

	"github.com/tsvsheet/tsvsheet.api/document"
)

// put replaces or creates the document from a source body.
func (handler Handler) put(w http.ResponseWriter, r *http.Request, doc document.DocPath) {
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
	snap, created, err := handler.config.Port.Put(r.Context(), doc, body, expect)
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
func putExpectation(r *http.Request) (document.Expect, httpStatus, error) {
	if r.Header.Get("If-None-Match") == "*" {
		return document.ExpectAbsent(), 0, nil
	}
	rev, status, err := ifMatchRevision(r)
	if err != nil {
		return document.Expect{}, status, err
	}
	return document.ExpectRev(rev), 0, nil
}

// post applies an edits batch.
func (handler Handler) post(w http.ResponseWriter, r *http.Request, doc document.DocPath) {
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
	doc document.DocPath,
	body []byte,
	rev tsvsheet.RevisionHex,
) {
	batch, err := tsvsheet.ParseEdits(body)
	if err != nil {
		writeProblem(w, http.StatusBadRequest, problemBadEdits, problemDetail(err.Error()))
		return
	}
	applied, err := handler.config.Port.Apply(r.Context(), doc, batch, rev)
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
func (handler Handler) announce(doc document.DocPath, applied document.Applied, body []byte) {
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
func (handler Handler) delete(w http.ResponseWriter, r *http.Request, doc document.DocPath) {
	rev, status, err := ifMatchRevision(r)
	if err != nil {
		writeProblem(w, status, problemOf(status), problemDetail(err.Error()))
		return
	}
	if err := handler.config.Port.Delete(r.Context(), doc, rev); err != nil {
		handler.writeError(w, err)
		return
	}
	handler.hub.broadcast(doc, eventDeleted, "")
	w.WriteHeader(http.StatusNoContent)
}
