package api_test

import (
	"bufio"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	tsvsheet "github.com/tsvsheet/go-tsvsheet"

	"github.com/tsvsheet/tsvsheet.api/internal/api"
	"github.com/tsvsheet/tsvsheet.api/internal/store"
)

// fixedClock keeps compute deterministic.
func fixedClock() time.Time { return time.Unix(1_700_000_000, 0).UTC() }

// newHandler builds a handler over a fresh root seeded with files.
func newHandler(t *testing.T, files map[string]string, compute api.ComputePlane) http.Handler {
	t.Helper()
	dir := t.TempDir()
	for name, content := range files {
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600))
	}
	st, err := store.Open(store.RootDir(dir), tsvsheet.DefaultLimits())
	require.NoError(t, err)
	return api.NewHandler(api.Config{
		Store: st, Limits: tsvsheet.DefaultLimits(), ComputeEnabled: compute, Clock: fixedClock,
	})
}

// revision content-addresses src through the engine.
func revision(t *testing.T, src string) string {
	t.Helper()
	doc, err := tsvsheet.ParseDocument([]byte(src))
	require.NoError(t, err)
	return string(tsvsheet.Revision(doc))
}

// etag is the quoted strong validator for src.
func etag(t *testing.T, src string) string {
	t.Helper()
	return `"` + revision(t, src) + `"`
}

// do runs one request against the handler.
func do(h http.Handler, method, target, body string, header map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	for k, v := range header {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestGetServesCanonicalSourceWithETag(t *testing.T) {
	h := newHandler(t, map[string]string{"a.tsvt": "1\t=A1+1\n"}, api.DocumentPlaneOnly)
	rec := do(h, http.MethodGet, "/a.tsvt", "", nil)
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "1\t=A1+1\n", rec.Body.String())
	assert.Equal(t, api.TypeDoc, rec.Header().Get("Content-Type"))
	assert.Equal(t, etag(t, "1\t=A1+1\n"), rec.Header().Get("ETag"))
}

func TestGetExplicitDocAccept(t *testing.T) {
	h := newHandler(t, map[string]string{"a.tsvt": "1\n"}, api.DocumentPlaneOnly)
	rec := do(h, http.MethodGet, "/a.tsvt", "", map[string]string{"Accept": api.TypeDoc})
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "1\n", rec.Body.String())
}

func TestGetMissingIsProblem404(t *testing.T) {
	h := newHandler(t, nil, api.DocumentPlaneOnly)
	rec := do(h, http.MethodGet, "/absent.tsvt", "", nil)
	assert.Equal(t, http.StatusNotFound, rec.Code)
	assert.Equal(t, api.TypeProblem, rec.Header().Get("Content-Type"))
	assert.Contains(t, rec.Body.String(), "not-found")
}

func TestGetValuesAcceptWithoutComputeIs406(t *testing.T) {
	// The hard rule: a document-plane server must never serve source bytes to
	// a values Accept — an importer would ingest formulas as literal text.
	h := newHandler(t, map[string]string{"a.tsvt": "=1+1\n"}, api.DocumentPlaneOnly)
	for _, accept := range []string{api.TypeSheet, "text/tab-separated-values", "text/csv"} {
		rec := do(h, http.MethodGet, "/a.tsvt", "", map[string]string{"Accept": accept})
		assert.Equal(t, http.StatusNotAcceptable, rec.Code, accept)
	}
}

func TestGetUnservableAcceptIs406(t *testing.T) {
	h := newHandler(t, map[string]string{"a.tsvt": "1\n"}, api.DocumentPlaneOnly)
	rec := do(h, http.MethodGet, "/a.tsvt", "", map[string]string{"Accept": "application/json"})
	assert.Equal(t, http.StatusNotAcceptable, rec.Code)
}

func TestHeadServesETagOnly(t *testing.T) {
	h := newHandler(t, map[string]string{"a.tsvt": "1\n"}, api.DocumentPlaneOnly)
	rec := do(h, http.MethodHead, "/a.tsvt", "", nil)
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, etag(t, "1\n"), rec.Header().Get("ETag"))
	assert.Empty(t, rec.Body.String())
}

func TestCapabilitiesHeaderOnEveryResponse(t *testing.T) {
	h := newHandler(t, map[string]string{"a.tsvt": "1\n"}, api.WithComputePlane)
	rec := do(h, http.MethodGet, "/a.tsvt", "", nil)
	assert.Equal(t, "edits, events, compute", rec.Header().Get("Tsvsheet-Capabilities"))
	doc := newHandler(t, nil, api.DocumentPlaneOnly)
	rec = do(doc, http.MethodOptions, "/a.tsvt", "", nil)
	assert.Equal(t, "edits, events", rec.Header().Get("Tsvsheet-Capabilities"))
	assert.Contains(t, rec.Header().Get("Allow"), http.MethodPost)
}

func TestPutCreates201(t *testing.T) {
	h := newHandler(t, nil, api.DocumentPlaneOnly)
	rec := do(h, http.MethodPut, "/new.tsvt", "1\t2", map[string]string{
		"Content-Type": api.TypeDoc, "If-None-Match": "*",
	})
	assert.Equal(t, http.StatusCreated, rec.Code)
	assert.Equal(t, etag(t, "1\t2\n"), rec.Header().Get("ETag"))
	got := do(h, http.MethodGet, "/new.tsvt", "", nil)
	assert.Equal(t, "1\t2\n", got.Body.String())
}

func TestPutCreateOverExistingIs409(t *testing.T) {
	h := newHandler(t, map[string]string{"a.tsvt": "1\n"}, api.DocumentPlaneOnly)
	rec := do(h, http.MethodPut, "/a.tsvt", "2\n", map[string]string{
		"Content-Type": api.TypeDoc, "If-None-Match": "*",
	})
	assert.Equal(t, http.StatusConflict, rec.Code)
}

func TestPutReplaceWithIfMatch(t *testing.T) {
	h := newHandler(t, map[string]string{"a.tsvt": "1\n"}, api.DocumentPlaneOnly)
	rec := do(h, http.MethodPut, "/a.tsvt", "2\n", map[string]string{
		"Content-Type": api.TypeDoc, "If-Match": etag(t, "1\n"),
	})
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, etag(t, "2\n"), rec.Header().Get("ETag"))
}

func TestPutStaleIfMatchIs412(t *testing.T) {
	h := newHandler(t, map[string]string{"a.tsvt": "1\n"}, api.DocumentPlaneOnly)
	rec := do(h, http.MethodPut, "/a.tsvt", "2\n", map[string]string{
		"Content-Type": api.TypeDoc, "If-Match": etag(t, "stale\n"),
	})
	assert.Equal(t, http.StatusPreconditionFailed, rec.Code)
}

func TestPutWithoutPreconditionIs428(t *testing.T) {
	h := newHandler(t, map[string]string{"a.tsvt": "1\n"}, api.DocumentPlaneOnly)
	rec := do(h, http.MethodPut, "/a.tsvt", "2\n", map[string]string{"Content-Type": api.TypeDoc})
	assert.Equal(t, http.StatusPreconditionRequired, rec.Code)
}

func TestPutWrongContentTypeIs415(t *testing.T) {
	h := newHandler(t, nil, api.DocumentPlaneOnly)
	rec := do(h, http.MethodPut, "/a.tsvt", "1\n", map[string]string{
		"Content-Type": "text/plain", "If-None-Match": "*",
	})
	assert.Equal(t, http.StatusUnsupportedMediaType, rec.Code)
}

func TestPutUnparsableBodyIs422(t *testing.T) {
	h := newHandler(t, nil, api.DocumentPlaneOnly)
	rec := do(h, http.MethodPut, "/a.tsvt", "=(\n", map[string]string{
		"Content-Type": api.TypeDoc, "If-None-Match": "*",
	})
	assert.Equal(t, http.StatusUnprocessableEntity, rec.Code)
}

func TestPostAppliesEdits(t *testing.T) {
	h := newHandler(t, map[string]string{"a.tsvt": "1\t2\n"}, api.DocumentPlaneOnly)
	rec := do(h, http.MethodPost, "/a.tsvt", "setCell\tB1\t9\n", map[string]string{
		"Content-Type": api.TypeEdits, "If-Match": etag(t, "1\t2\n"),
	})
	assert.Equal(t, http.StatusNoContent, rec.Code)
	assert.Equal(t, etag(t, "1\t9\n"), rec.Header().Get("ETag"))
	got := do(h, http.MethodGet, "/a.tsvt", "", nil)
	assert.Equal(t, "1\t9\n", got.Body.String())
}

func TestPostWithoutIfMatchIs428(t *testing.T) {
	h := newHandler(t, map[string]string{"a.tsvt": "1\n"}, api.DocumentPlaneOnly)
	rec := do(h, http.MethodPost, "/a.tsvt", "setCell\tA1\t2\n", map[string]string{"Content-Type": api.TypeEdits})
	assert.Equal(t, http.StatusPreconditionRequired, rec.Code)
}

func TestPostStaleIfMatchIs412(t *testing.T) {
	h := newHandler(t, map[string]string{"a.tsvt": "1\n"}, api.DocumentPlaneOnly)
	rec := do(h, http.MethodPost, "/a.tsvt", "setCell\tA1\t2\n", map[string]string{
		"Content-Type": api.TypeEdits, "If-Match": etag(t, "stale\n"),
	})
	assert.Equal(t, http.StatusPreconditionFailed, rec.Code)
}

func TestPostMalformedIfMatchIs400(t *testing.T) {
	h := newHandler(t, map[string]string{"a.tsvt": "1\n"}, api.DocumentPlaneOnly)
	rec := do(h, http.MethodPost, "/a.tsvt", "setCell\tA1\t2\n", map[string]string{
		"Content-Type": api.TypeEdits, "If-Match": "not-quoted",
	})
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestPostWrongContentTypeIs415(t *testing.T) {
	h := newHandler(t, map[string]string{"a.tsvt": "1\n"}, api.DocumentPlaneOnly)
	rec := do(h, http.MethodPost, "/a.tsvt", "setCell\tA1\t2\n", map[string]string{
		"Content-Type": "application/json", "If-Match": etag(t, "1\n"),
	})
	assert.Equal(t, http.StatusUnsupportedMediaType, rec.Code)
}

func TestPostMalformedEditsIs400(t *testing.T) {
	h := newHandler(t, map[string]string{"a.tsvt": "1\n"}, api.DocumentPlaneOnly)
	rec := do(h, http.MethodPost, "/a.tsvt", "nope\tA1\n", map[string]string{
		"Content-Type": api.TypeEdits, "If-Match": etag(t, "1\n"),
	})
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "bad-edits")
}

func TestPostConflictingInBandBaseIs422(t *testing.T) {
	h := newHandler(t, map[string]string{"a.tsvt": "1\n"}, api.DocumentPlaneOnly)
	body := "#.base\t" + revision(t, "other\n") + "\nsetCell\tA1\t2\n"
	rec := do(h, http.MethodPost, "/a.tsvt", body, map[string]string{
		"Content-Type": api.TypeEdits, "If-Match": etag(t, "1\n"),
	})
	assert.Equal(t, http.StatusUnprocessableEntity, rec.Code)
}

func TestPostRefusedOpIs422NamingTheLine(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "a.tsvt"), []byte("1\n"), 0o600))
	st, err := store.Open(store.RootDir(dir), tsvsheet.Limits{ResultCells: 4, GridDim: 2, ResultBytes: 64})
	require.NoError(t, err)
	h := api.NewHandler(
		api.Config{
			Store: st, Limits: tsvsheet.DefaultLimits(), ComputeEnabled: api.DocumentPlaneOnly, Clock: fixedClock,
		},
	)
	rec := do(h, http.MethodPost, "/a.tsvt", "setCell\tE9\tfar\n", map[string]string{
		"Content-Type": api.TypeEdits, "If-Match": etag(t, "1\n"),
	})
	assert.Equal(t, http.StatusUnprocessableEntity, rec.Code)
	assert.Contains(t, rec.Body.String(), "refused-edits")
}

func TestPostIdempotencyKeyReplays(t *testing.T) {
	h := newHandler(t, map[string]string{"a.tsvt": "1\t2\n"}, api.DocumentPlaneOnly)
	header := map[string]string{
		"Content-Type": api.TypeEdits, "If-Match": etag(t, "1\t2\n"), "Idempotency-Key": "k1",
	}
	first := do(h, http.MethodPost, "/a.tsvt", "setCell\tB1\t9\n", header)
	require.Equal(t, http.StatusNoContent, first.Code)
	// The retry carries the now-stale If-Match; the stored outcome replays
	// instead of a second application or a 412.
	second := do(h, http.MethodPost, "/a.tsvt", "setCell\tB1\t9\n", header)
	assert.Equal(t, http.StatusNoContent, second.Code)
	assert.Equal(t, first.Header().Get("ETag"), second.Header().Get("ETag"))
	got := do(h, http.MethodGet, "/a.tsvt", "", nil)
	assert.Equal(t, "1\t9\n", got.Body.String())
}

func TestDeleteWithIfMatch(t *testing.T) {
	h := newHandler(t, map[string]string{"a.tsvt": "1\n"}, api.DocumentPlaneOnly)
	rec := do(h, http.MethodDelete, "/a.tsvt", "", map[string]string{"If-Match": etag(t, "1\n")})
	assert.Equal(t, http.StatusNoContent, rec.Code)
	got := do(h, http.MethodGet, "/a.tsvt", "", nil)
	assert.Equal(t, http.StatusNotFound, got.Code)
}

func TestDeleteStaleIs412AndMissingIs404(t *testing.T) {
	h := newHandler(t, map[string]string{"a.tsvt": "1\n"}, api.DocumentPlaneOnly)
	rec := do(h, http.MethodDelete, "/a.tsvt", "", map[string]string{"If-Match": etag(t, "x\n")})
	assert.Equal(t, http.StatusPreconditionFailed, rec.Code)
	rec = do(h, http.MethodDelete, "/absent.tsvt", "", map[string]string{"If-Match": etag(t, "1\n")})
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

func TestUnknownMethodIs405(t *testing.T) {
	h := newHandler(t, map[string]string{"a.tsvt": "1\n"}, api.DocumentPlaneOnly)
	rec := do(h, http.MethodPatch, "/a.tsvt", "", nil)
	assert.Equal(t, http.StatusMethodNotAllowed, rec.Code)
}

func TestEmptyPathIs404(t *testing.T) {
	h := newHandler(t, nil, api.DocumentPlaneOnly)
	rec := do(h, http.MethodGet, "/", "", nil)
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

func TestComputedWholeGrid(t *testing.T) {
	h := newHandler(t, map[string]string{"a.tsvt": "2\t=A1*3\n"}, api.WithComputePlane)
	rec := do(h, http.MethodGet, "/a.tsvt", "", map[string]string{"Accept": api.TypeSheet})
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, api.TypeSheet, rec.Header().Get("Content-Type"))
	assert.Equal(t, "2\t6\n", rec.Body.String())
	assert.Equal(t, "Accept", rec.Header().Get("Vary"))
	assert.NotEqual(t, etag(t, "2\t=A1*3\n"), rec.Header().Get("ETag"), "a computed rendering is not the source entity")
}

func TestComputedPlainTSVAndCSV(t *testing.T) {
	h := newHandler(t, map[string]string{"a.tsvt": "a,b\t=1+1\n"}, api.WithComputePlane)
	rec := do(h, http.MethodGet, "/a.tsvt", "", map[string]string{"Accept": "text/tab-separated-values"})
	assert.Equal(t, "a,b\t2\n", rec.Body.String())
	rec = do(h, http.MethodGet, "/a.tsvt", "", map[string]string{"Accept": "text/csv"})
	assert.Equal(t, "\"a,b\",2\n", rec.Body.String())
	assert.Equal(t, "text/csv", rec.Header().Get("Content-Type"))
}

func TestComputedCellRead(t *testing.T) {
	h := newHandler(t, map[string]string{"a.tsvt": "5\t=A1*2\n"}, api.WithComputePlane)
	rec := do(h, http.MethodGet, "/a.tsvt!B1", "", map[string]string{"Accept": api.TypeCell})
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, api.TypeCell, rec.Header().Get("Content-Type"))
	assert.Equal(t, "10\n", rec.Body.String())
}

func TestComputedRangeRowColumnShapes(t *testing.T) {
	h := newHandler(t, map[string]string{"a.tsvt": "1\t2\n3\t4\n"}, api.WithComputePlane)
	rec := do(h, http.MethodGet, "/a.tsvt!A1:B2", "", map[string]string{"Accept": api.TypeRange})
	assert.Equal(t, "1\t2\n3\t4\n", rec.Body.String())
	rec = do(h, http.MethodGet, "/a.tsvt!A1:B1", "", map[string]string{"Accept": api.TypeRow})
	assert.Equal(t, "1\t2\n", rec.Body.String())
	rec = do(h, http.MethodGet, "/a.tsvt!A1:A2", "", map[string]string{"Accept": api.TypeColumn})
	assert.Equal(t, "1\n3\n", rec.Body.String())
}

func TestComputedShapeMismatchIs406(t *testing.T) {
	h := newHandler(t, map[string]string{"a.tsvt": "1\t2\n3\t4\n"}, api.WithComputePlane)
	rec := do(h, http.MethodGet, "/a.tsvt!A1:B2", "", map[string]string{"Accept": api.TypeCell})
	assert.Equal(t, http.StatusNotAcceptable, rec.Code)
}

func TestComputedDefaultAcceptMatchesShape(t *testing.T) {
	h := newHandler(t, map[string]string{"a.tsvt": "1\t2\n"}, api.WithComputePlane)
	rec := do(h, http.MethodGet, "/a.tsvt!A1", "", nil)
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, api.TypeCell, rec.Header().Get("Content-Type"))
	assert.Equal(t, "1\n", rec.Body.String())
}

func TestComputedRefWithoutComputeIs406(t *testing.T) {
	h := newHandler(t, map[string]string{"a.tsvt": "1\n"}, api.DocumentPlaneOnly)
	rec := do(h, http.MethodGet, "/a.tsvt!A1", "", nil)
	assert.Equal(t, http.StatusNotAcceptable, rec.Code)
}

func TestComputedBadRefIs404(t *testing.T) {
	h := newHandler(t, map[string]string{"a.tsvt": "1\n"}, api.WithComputePlane)
	for _, ref := range []string{"x", "B0", "A1:zz", "A1:B2:C3"} {
		rec := do(h, http.MethodGet, "/a.tsvt!"+ref, "", nil)
		assert.Equal(t, http.StatusNotFound, rec.Code, ref)
	}
}

func TestComputedOutOfExtentCellIsEmpty(t *testing.T) {
	h := newHandler(t, map[string]string{"a.tsvt": "1\n"}, api.WithComputePlane)
	rec := do(h, http.MethodGet, "/a.tsvt!E9", "", nil)
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "\n", rec.Body.String())
}

func TestPinnedRevisionRead(t *testing.T) {
	h := newHandler(t, map[string]string{"a.tsvt": "1\n"}, api.WithComputePlane)
	rec := do(h, http.MethodGet, "/a.tsvt?rev="+revision(t, "1\n"), "", nil)
	assert.Equal(t, http.StatusOK, rec.Code)
	rec = do(h, http.MethodGet, "/a.tsvt?rev="+revision(t, "other\n"), "", nil)
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

func TestFeedOnMissingDocumentIs404(t *testing.T) {
	h := newHandler(t, nil, api.DocumentPlaneOnly)
	rec := do(h, http.MethodGet, "/absent.tsvt", "", map[string]string{"Accept": "text/event-stream"})
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

func TestDeleteWithoutIfMatchIs428(t *testing.T) {
	h := newHandler(t, map[string]string{"a.tsvt": "1\n"}, api.DocumentPlaneOnly)
	rec := do(h, http.MethodDelete, "/a.tsvt", "", nil)
	assert.Equal(t, http.StatusPreconditionRequired, rec.Code)
}

func TestOversizeBodiesAre413(t *testing.T) {
	h := newHandler(t, map[string]string{"a.tsvt": "1\n"}, api.DocumentPlaneOnly)
	huge := strings.Repeat("x", (4<<20)+1)
	rec := do(h, http.MethodPost, "/a.tsvt", huge, map[string]string{
		"Content-Type": api.TypeEdits, "If-Match": etag(t, "1\n"),
	})
	assert.Equal(t, http.StatusRequestEntityTooLarge, rec.Code)
	rec = do(h, http.MethodPut, "/a.tsvt", huge, map[string]string{
		"Content-Type": api.TypeDoc, "If-Match": etag(t, "1\n"),
	})
	assert.Equal(t, http.StatusRequestEntityTooLarge, rec.Code)
}

func TestUnreadableStoredDocumentIs500(t *testing.T) {
	// A directory where a document should be drives the store's read-failure
	// path through the handler's internal-error fallback.
	dir := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(dir, "d.tsvt"), 0o700))
	st, err := store.Open(store.RootDir(dir), tsvsheet.DefaultLimits())
	require.NoError(t, err)
	h := api.NewHandler(
		api.Config{
			Store: st, Limits: tsvsheet.DefaultLimits(), ComputeEnabled: api.DocumentPlaneOnly, Clock: fixedClock,
		},
	)
	rec := do(h, http.MethodGet, "/d.tsvt", "", nil)
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
	assert.Contains(t, rec.Body.String(), "internal")
}

// TestReferenceIsReadOnly pins that a reference URL cannot mutate: accepting
// it would let a cell address replace or delete the whole document.
func TestReferenceIsReadOnly(t *testing.T) {
	for _, method := range []string{http.MethodPut, http.MethodPost, http.MethodDelete} {
		h := newHandler(t, map[string]string{"a.tsvt": "1\t2\n"}, api.WithComputePlane)
		rec := do(h, method, "/a.tsvt!A1", "setCell\tA1\t9\n", map[string]string{
			"Content-Type": api.TypeEdits, "If-Match": etag(t, "1\t2\n"),
		})
		assert.Equal(t, http.StatusMethodNotAllowed, rec.Code, method)
		still := do(h, http.MethodGet, "/a.tsvt", "", nil)
		assert.Equal(t, "1\t2\n", still.Body.String(), method+" left the document untouched")
	}
}

// TestComputedSpanIsBoundedByTheGrid pins that a far reference renders the
// grid's tail once instead of materializing every addressable cell — one GET
// must not be able to exhaust the server.
func TestComputedSpanIsBoundedByTheGrid(t *testing.T) {
	h := newHandler(t, map[string]string{"a.tsvt": "1\t2\n3\t4\n"}, api.WithComputePlane)
	rec := do(h, http.MethodGet, "/a.tsvt!A1:B9223372036854775806", "", nil)
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "1\t2\n3\t4\n", rec.Body.String())
}

func TestComputedSpanBeyondTheGridStillRendersOneCell(t *testing.T) {
	h := newHandler(t, map[string]string{"a.tsvt": "1\n"}, api.WithComputePlane)
	rec := do(h, http.MethodGet, "/a.tsvt!E9", "", nil)
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "\n", rec.Body.String())
}

// TestRepresentationsCarryDistinctValidators pins the cache contract: the
// source keeps the revision (mutations are conditioned on it) while each
// computed rendering gets its own tag, and Vary states the axis.
func TestRepresentationsCarryDistinctValidators(t *testing.T) {
	h := newHandler(t, map[string]string{"a.tsvt": "1\t2\n3\t=A1+B1\n"}, api.WithComputePlane)
	source := do(h, http.MethodGet, "/a.tsvt", "", map[string]string{"Accept": api.TypeDoc})
	csv := do(h, http.MethodGet, "/a.tsvt", "", map[string]string{"Accept": "text/csv"})
	sheet := do(h, http.MethodGet, "/a.tsvt", "", map[string]string{"Accept": api.TypeSheet})
	assert.Equal(t, etag(t, "1\t2\n3\t=A1+B1\n"), source.Header().Get("ETag"), "If-Match keeps working")
	assert.NotEqual(t, source.Header().Get("ETag"), csv.Header().Get("ETag"))
	assert.NotEqual(t, csv.Header().Get("ETag"), sheet.Header().Get("ETag"))
	for _, rec := range []*httptest.ResponseRecorder{source, csv, sheet} {
		assert.Equal(t, "Accept", rec.Header().Get("Vary"))
	}
}

func TestVolatileComputedBodyCarriesNoStrongValidator(t *testing.T) {
	h := newHandler(t, map[string]string{"v.tsvt": "=random()|volatile\n"}, api.WithComputePlane)
	rec := do(h, http.MethodGet, "/v.tsvt", "", map[string]string{"Accept": "text/csv"})
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Empty(t, rec.Header().Get("ETag"), "a body that varies within one revision is not an entity")
	assert.Equal(t, "no-store", rec.Header().Get("Cache-Control"))
}

func TestIdempotencyKeyRefusesADifferentBatch(t *testing.T) {
	h := newHandler(t, map[string]string{"k.tsvt": "1\t2\n"}, api.DocumentPlaneOnly)
	first := do(h, http.MethodPost, "/k.tsvt", "setCell\tA1\tFIRST\n", map[string]string{
		"Content-Type": api.TypeEdits, "If-Match": etag(t, "1\t2\n"), "Idempotency-Key": "k1",
	})
	require.Equal(t, http.StatusNoContent, first.Code)
	second := do(h, http.MethodPost, "/k.tsvt", "setCell\tB1\tSECOND\n", map[string]string{
		"Content-Type": api.TypeEdits, "If-Match": etag(t, "FIRST\t2\n"), "Idempotency-Key": "k1",
	})
	assert.Equal(
		t,
		http.StatusUnprocessableEntity,
		second.Code,
		"a reused key with a new batch is refused, never silently dropped",
	)
	got := do(h, http.MethodGet, "/k.tsvt", "", nil)
	assert.Equal(t, "FIRST\t2\n", got.Body.String())
}

func TestIdempotencyKeyIsScopedToItsDocument(t *testing.T) {
	h := newHandler(t, map[string]string{"a.tsvt": "1\n", "b.tsvt": "1\n"}, api.DocumentPlaneOnly)
	header := map[string]string{"Content-Type": api.TypeEdits, "If-Match": etag(t, "1\n"), "Idempotency-Key": "shared"}
	require.Equal(t, http.StatusNoContent, do(h, http.MethodPost, "/a.tsvt", "setCell\tA1\t2\n", header).Code)
	require.Equal(t, http.StatusNoContent, do(h, http.MethodPost, "/b.tsvt", "setCell\tA1\t2\n", header).Code)
	got := do(h, http.MethodGet, "/b.tsvt", "", nil)
	assert.Equal(t, "2\n", got.Body.String(), "the second document was really edited, not replayed")
}

func TestEscapingSymlinkIs404(t *testing.T) {
	outside := filepath.Join(t.TempDir(), "secret.tsvt")
	require.NoError(t, os.WriteFile(outside, []byte("secret\n"), 0o600))
	dir := t.TempDir()
	require.NoError(t, os.Symlink(outside, filepath.Join(dir, "link.tsvt")))
	st, err := store.Open(store.RootDir(dir), tsvsheet.DefaultLimits())
	require.NoError(t, err)
	h := api.NewHandler(api.Config{
		Store: st, Limits: tsvsheet.DefaultLimits(), ComputeEnabled: api.DocumentPlaneOnly, Clock: fixedClock,
	})
	rec := do(h, http.MethodGet, "/link.tsvt", "", nil)
	assert.Equal(t, http.StatusNotFound, rec.Code)
	assert.NotContains(t, rec.Body.String(), "secret")
}

// sseSession is one live event-stream connection.
type sseSession struct {
	resp    *http.Response
	scanner *bufio.Scanner
}

// openSSE subscribes to a.tsvt's change feed on the live server.
func openSSE(t *testing.T, base, lastID string) *sseSession {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, base+"/a.tsvt", nil)
	require.NoError(t, err)
	req.Header.Set("Accept", "text/event-stream")
	if lastID != "" {
		req.Header.Set("Last-Event-ID", lastID)
	}
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	t.Cleanup(func() { _ = resp.Body.Close() })
	require.Equal(t, http.StatusOK, resp.StatusCode)
	return &sseSession{resp: resp, scanner: bufio.NewScanner(resp.Body)}
}

// next reads one event (to its blank-line terminator) with a deadline.
func (s *sseSession) next(t *testing.T) map[string]string {
	t.Helper()
	event := map[string]string{}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for s.scanner.Scan() {
			line := s.scanner.Text()
			if line == "" {
				return
			}
			key, value, _ := strings.Cut(line, ": ")
			if prev, ok := event[key]; ok {
				value = prev + "\n" + value
			}
			event[key] = value
		}
	}()
	select {
	case <-done:
		return event
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for an SSE event")
		return nil
	}
}

func (s *sseSession) close() { _ = s.resp.Body.Close() }

func TestFeedDeliversChangedAndComputedEvents(t *testing.T) {
	h := newHandler(t, map[string]string{"a.tsvt": "1\t=A1*2\n"}, api.WithComputePlane)
	server := httptest.NewServer(h)
	defer server.Close()

	session := openSSE(t, server.URL, "")
	defer session.close()

	body := "setCell\tA1\t5\n"
	req, err := http.NewRequest(http.MethodPost, server.URL+"/a.tsvt", strings.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("Content-Type", api.TypeEdits)
	req.Header.Set("If-Match", etag(t, "1\t=A1*2\n"))
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	require.Equal(t, http.StatusNoContent, resp.StatusCode)
	_ = resp.Body.Close()

	changed := session.next(t)
	assert.Equal(t, "changed", changed["event"])
	assert.Contains(t, changed["data"], "setCell\tA1\t5")
	assert.Contains(t, changed["data"], "#.base\t"+revision(t, "1\t=A1*2\n"))
	assert.Contains(t, changed["data"], "#.rev\t"+revision(t, "5\t=A1*2\n"))

	computed := session.next(t)
	assert.Equal(t, "computed", computed["event"])
	assert.Contains(t, computed["data"], "A1\t5")
	assert.Contains(t, computed["data"], "B1\t10")
}

func TestFeedComputedDeltaOnRowDeletion(t *testing.T) {
	// Deleting a row shrinks the grid: the delta names the vacated cells with
	// their new (empty or shifted) values.
	h := newHandler(t, map[string]string{"a.tsvt": "1\t2\n3\t4\n"}, api.WithComputePlane)
	server := httptest.NewServer(h)
	defer server.Close()
	session := openSSE(t, server.URL, "")
	defer session.close()

	req, err := http.NewRequest(http.MethodPost, server.URL+"/a.tsvt", strings.NewReader("deleteRow\t1\n"))
	require.NoError(t, err)
	req.Header.Set("Content-Type", api.TypeEdits)
	req.Header.Set("If-Match", etag(t, "1\t2\n3\t4\n"))
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	require.Equal(t, http.StatusNoContent, resp.StatusCode)
	_ = resp.Body.Close()

	changed := session.next(t)
	require.Equal(t, "changed", changed["event"])
	computed := session.next(t)
	require.Equal(t, "computed", computed["event"])
	assert.Contains(t, computed["data"], "A1\t3")
	assert.Contains(t, computed["data"], "B1\t4")
	assert.Contains(t, computed["data"], "A2\t\n")
}

func TestFeedReplaysFromLastEventID(t *testing.T) {
	h := newHandler(t, map[string]string{"a.tsvt": "1\n"}, api.DocumentPlaneOnly)
	server := httptest.NewServer(h)
	defer server.Close()

	first := openSSE(t, server.URL, "")
	post := func(ifMatch, cell string) {
		req, err := http.NewRequest(http.MethodPost, server.URL+"/a.tsvt", strings.NewReader("setCell\tA1\t"+cell+"\n"))
		require.NoError(t, err)
		req.Header.Set("Content-Type", api.TypeEdits)
		req.Header.Set("If-Match", ifMatch)
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		require.Equal(t, http.StatusNoContent, resp.StatusCode)
		_ = resp.Body.Close()
	}
	post(etag(t, "1\n"), "2")
	one := first.next(t)
	require.Equal(t, "changed", one["event"])
	firstID := one["id"]
	post(etag(t, "2\n"), "3")
	_ = first.next(t)
	first.close()

	// Reconnecting after the first event replays the second.
	second := openSSE(t, server.URL, firstID)
	defer second.close()
	replayed := second.next(t)
	assert.Equal(t, "changed", replayed["event"])
	assert.Contains(t, replayed["data"], "setCell\tA1\t3")
}

func TestFeedResetWhenHorizonGone(t *testing.T) {
	h := newHandler(t, map[string]string{"a.tsvt": "1\n"}, api.DocumentPlaneOnly)
	server := httptest.NewServer(h)
	defer server.Close()
	// An ID far below the retained window (which is empty) forces a reset.
	session := openSSE(t, server.URL, "999")
	defer session.close()
	event := session.next(t)
	assert.Equal(t, "reset", event["event"])
	assert.Contains(t, event["data"], "#.rev\t"+revision(t, "1\n"))
}

func TestPutBroadcastsReset(t *testing.T) {
	h := newHandler(t, map[string]string{"a.tsvt": "1\n"}, api.DocumentPlaneOnly)
	server := httptest.NewServer(h)
	defer server.Close()
	session := openSSE(t, server.URL, "")
	defer session.close()

	req, err := http.NewRequest(http.MethodPut, server.URL+"/a.tsvt", strings.NewReader("9\n"))
	require.NoError(t, err)
	req.Header.Set("Content-Type", api.TypeDoc)
	req.Header.Set("If-Match", etag(t, "1\n"))
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	_ = resp.Body.Close()

	event := session.next(t)
	assert.Equal(t, "reset", event["event"])
	assert.Contains(t, event["data"], "#.rev\t"+revision(t, "9\n"))
}

func TestDeleteBroadcastsDeletedAndEndsStream(t *testing.T) {
	h := newHandler(t, map[string]string{"a.tsvt": "1\n"}, api.DocumentPlaneOnly)
	server := httptest.NewServer(h)
	defer server.Close()
	session := openSSE(t, server.URL, "")
	defer session.close()

	req, err := http.NewRequest(http.MethodDelete, server.URL+"/a.tsvt", nil)
	require.NoError(t, err)
	req.Header.Set("If-Match", etag(t, "1\n"))
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	require.Equal(t, http.StatusNoContent, resp.StatusCode)
	_ = resp.Body.Close()

	event := session.next(t)
	assert.Equal(t, "deleted", event["event"])
	rest, err := io.ReadAll(session.resp.Body)
	require.NoError(t, err)
	assert.Empty(t, string(rest))
}
