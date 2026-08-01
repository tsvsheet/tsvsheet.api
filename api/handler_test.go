package api

import (
	"errors"
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

	"github.com/tsvsheet/tsvsheet.api/document"
	"github.com/tsvsheet/tsvsheet.api/store"
)

func TestUnknownMethodIs405(t *testing.T) {
	h := newHandler(t, map[string]string{"a.tsvt": "1\n"}, DocumentPlaneOnly)
	rec := do(h, http.MethodPatch, "/a.tsvt", "", nil)
	assert.Equal(t, http.StatusMethodNotAllowed, rec.Code)
}

func TestEmptyPathIs404(t *testing.T) {
	h := newHandler(t, nil, DocumentPlaneOnly)
	rec := do(h, http.MethodGet, "/", "", nil)
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

// fixedClock keeps compute deterministic.
func fixedClock() time.Time { return time.Unix(1_700_000_000, 0).UTC() }

// newHandler builds a handler over a fresh root seeded with files.
func newHandler(t *testing.T, files map[string]string, compute ComputePlane) http.Handler {
	t.Helper()
	dir := t.TempDir()
	for name, content := range files {
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600))
	}
	st, err := store.Open(store.RootDir(dir), tsvsheet.DefaultLimits())
	require.NoError(t, err)
	return NewHandler(Config{
		Port: st, Limits: tsvsheet.DefaultLimits(), ComputeEnabled: compute, Clock: fixedClock,
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

// TestReferenceIsReadOnly pins that a reference URL cannot mutate: accepting
// it would let a cell address replace or delete the whole document.
func TestReferenceIsReadOnly(t *testing.T) {
	for _, method := range []string{http.MethodPut, http.MethodPost, http.MethodDelete} {
		h := newHandler(t, map[string]string{"a.tsvt": "1\t2\n"}, WithComputePlane)
		rec := do(h, method, "/a.tsvt!A1", "setCell\tA1\t9\n", map[string]string{
			"Content-Type": TypeEdits, "If-Match": etag(t, "1\t2\n"),
		})
		assert.Equal(t, http.StatusMethodNotAllowed, rec.Code, method)
		still := do(h, http.MethodGet, "/a.tsvt", "", nil)
		assert.Equal(t, "1\t2\n", still.Body.String(), method+" left the document untouched")
	}
}

func TestIdempotencyKeyRefusesADifferentBatch(t *testing.T) {
	h := newHandler(t, map[string]string{"k.tsvt": "1\t2\n"}, DocumentPlaneOnly)
	first := do(h, http.MethodPost, "/k.tsvt", "setCell\tA1\tFIRST\n", map[string]string{
		"Content-Type": TypeEdits, "If-Match": etag(t, "1\t2\n"), "Idempotency-Key": "k1",
	})
	require.Equal(t, http.StatusNoContent, first.Code)
	second := do(h, http.MethodPost, "/k.tsvt", "setCell\tB1\tSECOND\n", map[string]string{
		"Content-Type": TypeEdits, "If-Match": etag(t, "FIRST\t2\n"), "Idempotency-Key": "k1",
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
	h := newHandler(t, map[string]string{"a.tsvt": "1\n", "b.tsvt": "1\n"}, DocumentPlaneOnly)
	header := map[string]string{"Content-Type": TypeEdits, "If-Match": etag(t, "1\n"), "Idempotency-Key": "shared"}
	require.Equal(t, http.StatusNoContent, do(h, http.MethodPost, "/a.tsvt", "setCell\tA1\t2\n", header).Code)
	require.Equal(t, http.StatusNoContent, do(h, http.MethodPost, "/b.tsvt", "setCell\tA1\t2\n", header).Code)
	got := do(h, http.MethodGet, "/b.tsvt", "", nil)
	assert.Equal(t, "2\n", got.Body.String(), "the second document was really edited, not replayed")
}

// TestReversedReferenceRangeIsRenderedNotFatal pins the fix for a remote
// panic: a reversed range produced a negative extent, and the allocation that
// followed crashed the connection. A1:C3 and C3:A1 name the same rectangle, as
// they do in the language.
func TestReversedReferenceRangeIsRenderedNotFatal(t *testing.T) {
	h := newHandler(t, map[string]string{"a.tsvt": "1\t2\t3\n4\t5\t6\n"}, WithComputePlane)
	forward := do(h, http.MethodGet, "/a.tsvt!A1:C2", "", nil)
	reversed := do(h, http.MethodGet, "/a.tsvt!C2:A1", "", nil)
	assert.Equal(t, http.StatusOK, reversed.Code)
	assert.Equal(t, forward.Body.String(), reversed.Body.String())
	assert.Equal(t, "1\t2\t3\n4\t5\t6\n", reversed.Body.String())
}

// TestEmptyIfMatchIsBadRequestNotAPrecondition pins that an empty validator is
// malformed rather than a revision that happens not to match: a mutation
// accepting it previously survived.
func TestEmptyIfMatchIsBadRequestNotAPrecondition(t *testing.T) {
	h := newHandler(t, map[string]string{"a.tsvt": "1\n"}, DocumentPlaneOnly)
	rec := do(h, http.MethodPut, "/a.tsvt", "2\n", map[string]string{
		"Content-Type": TypeDoc, "If-Match": `""`,
	})
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

// failingBody errors on read without being a MaxBytesError.
type failingBody struct{}

func (failingBody) Read([]byte) (int, error) { return 0, errors.New("torn") }
func (failingBody) Close() error             { return nil }
func TestReadBodyPlainFailureIs400(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/doc", nil)
	req.Body = failingBody{}
	rec := httptest.NewRecorder()
	_, ok := readBody(rec, req)
	require.False(t, ok)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "unreadable")
}

// TestIfMatchEmitsThePreconditionSentinel pins the errors this package emits
// for a missing or malformed validator, each with the status it implies.
func TestIfMatchEmitsThePreconditionSentinel(t *testing.T) {
	absent := httptest.NewRequest(http.MethodPut, "/a.tsvt", nil)
	_, status, err := ifMatchRevision(absent)
	require.Error(t, err)
	assert.ErrorIs(t, err, document.ErrPrecondition)
	assert.Equal(t, http.StatusPreconditionRequired, int(status))

	malformed := httptest.NewRequest(http.MethodPut, "/a.tsvt", nil)
	malformed.Header.Set("If-Match", "unquoted")
	_, status, err = ifMatchRevision(malformed)
	require.Error(t, err)
	assert.ErrorIs(t, err, document.ErrPrecondition)
	assert.Equal(t, http.StatusBadRequest, int(status))
}
