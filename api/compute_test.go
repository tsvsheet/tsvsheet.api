package api

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	tsvsheet "github.com/tsvsheet/go-tsvsheet"

	"github.com/tsvsheet/tsvsheet.api/document"
	"github.com/tsvsheet/tsvsheet.api/store"
)

func TestComputedWholeGrid(t *testing.T) {
	h := newHandler(t, map[string]string{"a.tsvt": "2\t=A1*3\n"}, WithComputePlane)
	rec := do(h, http.MethodGet, "/a.tsvt", "", map[string]string{"Accept": TypeSheet})
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, TypeSheet, rec.Header().Get("Content-Type"))
	assert.Equal(t, "2\t6\n", rec.Body.String())
	assert.Equal(t, "Accept", rec.Header().Get("Vary"))
	assert.NotEqual(t, etag(t, "2\t=A1*3\n"), rec.Header().Get("ETag"), "a computed rendering is not the source entity")
}

func TestComputedPlainTSVAndCSV(t *testing.T) {
	h := newHandler(t, map[string]string{"a.tsvt": "a,b\t=1+1\n"}, WithComputePlane)
	rec := do(h, http.MethodGet, "/a.tsvt", "", map[string]string{"Accept": "text/tab-separated-values"})
	assert.Equal(t, "a,b\t2\n", rec.Body.String())
	rec = do(h, http.MethodGet, "/a.tsvt", "", map[string]string{"Accept": "text/csv"})
	assert.Equal(t, "\"a,b\",2\n", rec.Body.String())
	assert.Equal(t, "text/csv", rec.Header().Get("Content-Type"))
}

func TestComputedCellRead(t *testing.T) {
	h := newHandler(t, map[string]string{"a.tsvt": "5\t=A1*2\n"}, WithComputePlane)
	rec := do(h, http.MethodGet, "/a.tsvt!B1", "", map[string]string{"Accept": TypeCell})
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, TypeCell, rec.Header().Get("Content-Type"))
	assert.Equal(t, "10\n", rec.Body.String())
}

func TestComputedRangeRowColumnShapes(t *testing.T) {
	h := newHandler(t, map[string]string{"a.tsvt": "1\t2\n3\t4\n"}, WithComputePlane)
	rec := do(h, http.MethodGet, "/a.tsvt!A1:B2", "", map[string]string{"Accept": TypeRange})
	assert.Equal(t, "1\t2\n3\t4\n", rec.Body.String())
	rec = do(h, http.MethodGet, "/a.tsvt!A1:B1", "", map[string]string{"Accept": TypeRow})
	assert.Equal(t, "1\t2\n", rec.Body.String())
	rec = do(h, http.MethodGet, "/a.tsvt!A1:A2", "", map[string]string{"Accept": TypeColumn})
	assert.Equal(t, "1\n3\n", rec.Body.String())
}

func TestComputedShapeMismatchIs406(t *testing.T) {
	h := newHandler(t, map[string]string{"a.tsvt": "1\t2\n3\t4\n"}, WithComputePlane)
	rec := do(h, http.MethodGet, "/a.tsvt!A1:B2", "", map[string]string{"Accept": TypeCell})
	assert.Equal(t, http.StatusNotAcceptable, rec.Code)
}

func TestComputedDefaultAcceptMatchesShape(t *testing.T) {
	h := newHandler(t, map[string]string{"a.tsvt": "1\t2\n"}, WithComputePlane)
	rec := do(h, http.MethodGet, "/a.tsvt!A1", "", nil)
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, TypeCell, rec.Header().Get("Content-Type"))
	assert.Equal(t, "1\n", rec.Body.String())
}

func TestComputedRefWithoutComputeIs406(t *testing.T) {
	h := newHandler(t, map[string]string{"a.tsvt": "1\n"}, DocumentPlaneOnly)
	rec := do(h, http.MethodGet, "/a.tsvt!A1", "", nil)
	assert.Equal(t, http.StatusNotAcceptable, rec.Code)
}

func TestComputedBadRefIs404(t *testing.T) {
	h := newHandler(t, map[string]string{"a.tsvt": "1\n"}, WithComputePlane)
	for _, ref := range []string{"x", "B0", "A1:zz", "A1:B2:C3"} {
		rec := do(h, http.MethodGet, "/a.tsvt!"+ref, "", nil)
		assert.Equal(t, http.StatusNotFound, rec.Code, ref)
	}
}

func TestComputedOutOfExtentCellIsEmpty(t *testing.T) {
	h := newHandler(t, map[string]string{"a.tsvt": "1\n"}, WithComputePlane)
	rec := do(h, http.MethodGet, "/a.tsvt!E9", "", nil)
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "\n", rec.Body.String())
}

func TestPinnedRevisionRead(t *testing.T) {
	h := newHandler(t, map[string]string{"a.tsvt": "1\n"}, WithComputePlane)
	rec := do(h, http.MethodGet, "/a.tsvt?rev="+revision(t, "1\n"), "", nil)
	assert.Equal(t, http.StatusOK, rec.Code)
	rec = do(h, http.MethodGet, "/a.tsvt?rev="+revision(t, "other\n"), "", nil)
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

// TestComputedSpanIsBoundedByTheGrid pins that a far reference renders the
// grid's tail once instead of materializing every addressable cell — one GET
// must not be able to exhaust the server.
func TestComputedSpanIsBoundedByTheGrid(t *testing.T) {
	h := newHandler(t, map[string]string{"a.tsvt": "1\t2\n3\t4\n"}, WithComputePlane)
	rec := do(h, http.MethodGet, "/a.tsvt!A1:B9223372036854775806", "", nil)
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "1\t2\n3\t4\n", rec.Body.String())
}

func TestComputedSpanBeyondTheGridStillRendersOneCell(t *testing.T) {
	h := newHandler(t, map[string]string{"a.tsvt": "1\n"}, WithComputePlane)
	rec := do(h, http.MethodGet, "/a.tsvt!E9", "", nil)
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "\n", rec.Body.String())
}

// TestRepresentationsCarryDistinctValidators pins the cache contract: the
// source keeps the revision (mutations are conditioned on it) while each
// computed rendering gets its own tag, and Vary states the axis.
func TestRepresentationsCarryDistinctValidators(t *testing.T) {
	h := newHandler(t, map[string]string{"a.tsvt": "1\t2\n3\t=A1+B1\n"}, WithComputePlane)
	source := do(h, http.MethodGet, "/a.tsvt", "", map[string]string{"Accept": TypeDoc})
	csv := do(h, http.MethodGet, "/a.tsvt", "", map[string]string{"Accept": "text/csv"})
	sheet := do(h, http.MethodGet, "/a.tsvt", "", map[string]string{"Accept": TypeSheet})
	assert.Equal(t, etag(t, "1\t2\n3\t=A1+B1\n"), source.Header().Get("ETag"), "If-Match keeps working")
	assert.NotEqual(t, source.Header().Get("ETag"), csv.Header().Get("ETag"))
	assert.NotEqual(t, csv.Header().Get("ETag"), sheet.Header().Get("ETag"))
	for _, rec := range []*httptest.ResponseRecorder{source, csv, sheet} {
		assert.Equal(t, "Accept", rec.Header().Get("Vary"))
	}
}

func TestVolatileComputedBodyCarriesNoStrongValidator(t *testing.T) {
	h := newHandler(t, map[string]string{"v.tsvt": "=random()|volatile\n"}, WithComputePlane)
	rec := do(h, http.MethodGet, "/v.tsvt", "", map[string]string{"Accept": "text/csv"})
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Empty(t, rec.Header().Get("ETag"), "a body that varies within one revision is not an entity")
	assert.Equal(t, "no-store", rec.Header().Get("Cache-Control"))
}

func TestReversedSingleAxisRangesAreRendered(t *testing.T) {
	h := newHandler(t, map[string]string{"a.tsvt": "1\t2\n3\t4\n"}, WithComputePlane)
	for ref, want := range map[string]string{
		"/a.tsvt!C1:A1": "1\t2\n",
		"/a.tsvt!A3:A1": "1\n3\n",
		"/a.tsvt!B2:A1": "1\t2\n3\t4\n",
	} {
		rec := do(h, http.MethodGet, ref, "", nil)
		assert.Equal(t, http.StatusOK, rec.Code, ref)
		assert.Equal(t, want, rec.Body.String(), ref)
	}
}

// TestComputedSpanIsBoundedOnBothAxes pins that the clip bounds columns as
// well as rows: a mutation removing the column bound previously survived, and
// it is the axis a far reference is most likely to name.
func TestComputedSpanIsBoundedOnBothAxes(t *testing.T) {
	h := newHandler(t, map[string]string{"a.tsvt": "1\t2\n"}, WithComputePlane)
	wide := do(h, http.MethodGet, "/a.tsvt!A1:ZZZ1", "", nil)
	assert.Equal(t, http.StatusOK, wide.Code)
	assert.Equal(t, "1\t2\n", wide.Body.String(), "the column extent bounds the row, not the reference")
	tall := do(h, http.MethodGet, "/a.tsvt!A1:A9999", "", nil)
	assert.Equal(t, "1\n", tall.Body.String())
}

func TestCellsDeltaCoversShrinkAndGrowth(t *testing.T) {
	old := tsvsheet.Grid{{"1", "2"}, {"3"}}
	next := tsvsheet.Grid{{"1", "9", "8"}}
	delta := cellsDelta(old, next)
	assert.Contains(t, delta, "B1\t9")
	assert.Contains(t, delta, "C1\t8")
	assert.Contains(t, delta, "A2\t\n")
	assert.NotContains(t, delta, "A1")
}

func TestCellsDeltaEqualGridsIsEmpty(t *testing.T) {
	grid := tsvsheet.Grid{{"1"}}
	assert.Empty(t, cellsDelta(grid, grid))
}

// TestParseRefEmitsThePathSentinel pins the error this package emits for a
// reference it cannot read, which the handler turns into a 404.
func TestParseRefEmitsThePathSentinel(t *testing.T) {
	for _, bad := range []refText{"x", "B0", "A1:zz", "A1:B2:C3", ""} {
		_, err := parseRef(bad)
		require.Error(t, err, bad)
		assert.ErrorIs(t, err, document.ErrPath, bad)
	}
}

// TestComputeHonoursTheConfiguredLimits pins the fix for a defect an
// adversarial pass reproduced: the compute plane ran on engine defaults while
// the edit path honoured the operator's cap, so one setting meant two
// different ceilings depending on which request arrived.
func TestComputeHonoursTheConfiguredLimits(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "big.tsvt"), []byte(`=rept("x",5000)`+"\n"), 0o600))
	documents, err := store.Open(store.RootDir(dir), tsvsheet.DefaultLimits())
	require.NoError(t, err)

	capped := NewHandler(Config{
		Port:           documents,
		Limits:         tsvsheet.Limits{ResultCells: 10, GridDim: 10, ResultBytes: 16},
		ComputeEnabled: WithComputePlane,
		Clock:          fixedClock,
	})
	rec := httptest.NewRecorder()
	capped.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/big.tsvt!A1", nil))
	assert.Equal(t, "#VALUE!\n", rec.Body.String(), "the operator's byte ceiling bounds a computed read")

	uncapped := NewHandler(Config{
		Port:           documents,
		Limits:         tsvsheet.DefaultLimits(),
		ComputeEnabled: WithComputePlane,
		Clock:          fixedClock,
	})
	wide := httptest.NewRecorder()
	uncapped.ServeHTTP(wide, httptest.NewRequest(http.MethodGet, "/big.tsvt!A1", nil))
	assert.Len(t, wide.Body.String(), 5001, "and without it the same read is served in full")
}
