package api

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	tsvsheet "github.com/tsvsheet/go-tsvsheet"

	"github.com/tsvsheet/tsvsheet.api/document"
	"github.com/tsvsheet/tsvsheet.api/store"
)

func TestGetServesCanonicalSourceWithETag(t *testing.T) {
	h := newHandler(t, map[string]string{"a.tsvt": "1\t=A1+1\n"}, DocumentPlaneOnly)
	rec := do(h, http.MethodGet, "/a.tsvt", "", nil)
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "1\t=A1+1\n", rec.Body.String())
	assert.Equal(t, TypeDoc, rec.Header().Get("Content-Type"))
	assert.Equal(t, etag(t, "1\t=A1+1\n"), rec.Header().Get("ETag"))
}

func TestGetExplicitDocAccept(t *testing.T) {
	h := newHandler(t, map[string]string{"a.tsvt": "1\n"}, DocumentPlaneOnly)
	rec := do(h, http.MethodGet, "/a.tsvt", "", map[string]string{"Accept": TypeDoc})
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "1\n", rec.Body.String())
}

func TestGetMissingIsProblem404(t *testing.T) {
	h := newHandler(t, nil, DocumentPlaneOnly)
	rec := do(h, http.MethodGet, "/absent.tsvt", "", nil)
	assert.Equal(t, http.StatusNotFound, rec.Code)
	assert.Equal(t, TypeProblem, rec.Header().Get("Content-Type"))
	assert.Contains(t, rec.Body.String(), "not-found")
}

func TestGetValuesAcceptWithoutComputeIs406(t *testing.T) {
	// The hard rule: a document-plane server must never serve source bytes to
	// a values Accept — an importer would ingest formulas as literal text.
	h := newHandler(t, map[string]string{"a.tsvt": "=1+1\n"}, DocumentPlaneOnly)
	for _, accept := range []string{TypeSheet, "text/tab-separated-values", "text/csv"} {
		rec := do(h, http.MethodGet, "/a.tsvt", "", map[string]string{"Accept": accept})
		assert.Equal(t, http.StatusNotAcceptable, rec.Code, accept)
	}
}

func TestGetUnservableAcceptIs406(t *testing.T) {
	h := newHandler(t, map[string]string{"a.tsvt": "1\n"}, DocumentPlaneOnly)
	rec := do(h, http.MethodGet, "/a.tsvt", "", map[string]string{"Accept": "application/json"})
	assert.Equal(t, http.StatusNotAcceptable, rec.Code)
}

func TestHeadServesETagOnly(t *testing.T) {
	h := newHandler(t, map[string]string{"a.tsvt": "1\n"}, DocumentPlaneOnly)
	rec := do(h, http.MethodHead, "/a.tsvt", "", nil)
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, etag(t, "1\n"), rec.Header().Get("ETag"))
	assert.Empty(t, rec.Body.String())
}

func TestCapabilitiesHeaderOnEveryResponse(t *testing.T) {
	h := newHandler(t, map[string]string{"a.tsvt": "1\n"}, WithComputePlane)
	rec := do(h, http.MethodGet, "/a.tsvt", "", nil)
	assert.Equal(t, "edits, events, compute", rec.Header().Get("Tsvsheet-Capabilities"))
	doc := newHandler(t, nil, DocumentPlaneOnly)
	rec = do(doc, http.MethodOptions, "/a.tsvt", "", nil)
	assert.Equal(t, "edits, events", rec.Header().Get("Tsvsheet-Capabilities"))
	assert.Contains(t, rec.Header().Get("Allow"), http.MethodPost)
}

func TestUnreadableStoredDocumentIs500(t *testing.T) {
	// A directory where a document should be drives the store's read-failure
	// path through the handler's internal-error fallback.
	dir := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(dir, "d.tsvt"), 0o700))
	st, err := store.Open(store.RootDir(dir), tsvsheet.DefaultLimits())
	require.NoError(t, err)
	h := NewHandler(
		Config{
			Port: st, Limits: tsvsheet.DefaultLimits(), ComputeEnabled: DocumentPlaneOnly, Clock: fixedClock,
		},
	)
	rec := do(h, http.MethodGet, "/d.tsvt", "", nil)
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
	assert.Contains(t, rec.Body.String(), "internal")
}

func TestEscapingSymlinkIs404(t *testing.T) {
	outside := filepath.Join(t.TempDir(), "secret.tsvt")
	require.NoError(t, os.WriteFile(outside, []byte("secret\n"), 0o600))
	dir := t.TempDir()
	require.NoError(t, os.Symlink(outside, filepath.Join(dir, "link.tsvt")))
	st, err := store.Open(store.RootDir(dir), tsvsheet.DefaultLimits())
	require.NoError(t, err)
	h := NewHandler(Config{
		Port: st, Limits: tsvsheet.DefaultLimits(), ComputeEnabled: DocumentPlaneOnly, Clock: fixedClock,
	})
	rec := do(h, http.MethodGet, "/link.tsvt", "", nil)
	assert.Equal(t, http.StatusNotFound, rec.Code)
	assert.NotContains(t, rec.Body.String(), "secret")
}

// TestPinnedRevisionEmitsTheMissingSentinel pins the error this package emits
// for a revision that is not the head: a caller matching document.ErrMissing
// gets the same answer whether the document is absent or the pin is stale.
func TestPinnedRevisionEmitsTheMissingSentinel(t *testing.T) {
	handler := newHandler(t, map[string]string{"a.tsvt": "1\n"}, DocumentPlaneOnly)
	_, err := handler.(Handler).pinned(context.Background(), "a.tsvt", "not-the-head")
	require.Error(t, err)
	assert.ErrorIs(t, err, document.ErrMissing)
}
