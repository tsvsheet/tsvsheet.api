package api

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	tsvsheet "github.com/tsvsheet/go-tsvsheet"

	"github.com/tsvsheet/tsvsheet.api/store"
)

func TestPutCreates201(t *testing.T) {
	h := newHandler(t, nil, DocumentPlaneOnly)
	rec := do(h, http.MethodPut, "/new.tsvt", "1\t2", map[string]string{
		"Content-Type": TypeDoc, "If-None-Match": "*",
	})
	assert.Equal(t, http.StatusCreated, rec.Code)
	assert.Equal(t, etag(t, "1\t2\n"), rec.Header().Get("ETag"))
	got := do(h, http.MethodGet, "/new.tsvt", "", nil)
	assert.Equal(t, "1\t2\n", got.Body.String())
}

func TestPutCreateOverExistingIs409(t *testing.T) {
	h := newHandler(t, map[string]string{"a.tsvt": "1\n"}, DocumentPlaneOnly)
	rec := do(h, http.MethodPut, "/a.tsvt", "2\n", map[string]string{
		"Content-Type": TypeDoc, "If-None-Match": "*",
	})
	assert.Equal(t, http.StatusConflict, rec.Code)
}

func TestPutReplaceWithIfMatch(t *testing.T) {
	h := newHandler(t, map[string]string{"a.tsvt": "1\n"}, DocumentPlaneOnly)
	rec := do(h, http.MethodPut, "/a.tsvt", "2\n", map[string]string{
		"Content-Type": TypeDoc, "If-Match": etag(t, "1\n"),
	})
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, etag(t, "2\n"), rec.Header().Get("ETag"))
}

func TestPutStaleIfMatchIs412(t *testing.T) {
	h := newHandler(t, map[string]string{"a.tsvt": "1\n"}, DocumentPlaneOnly)
	rec := do(h, http.MethodPut, "/a.tsvt", "2\n", map[string]string{
		"Content-Type": TypeDoc, "If-Match": etag(t, "stale\n"),
	})
	assert.Equal(t, http.StatusPreconditionFailed, rec.Code)
}

func TestPutWithoutPreconditionIs428(t *testing.T) {
	h := newHandler(t, map[string]string{"a.tsvt": "1\n"}, DocumentPlaneOnly)
	rec := do(h, http.MethodPut, "/a.tsvt", "2\n", map[string]string{"Content-Type": TypeDoc})
	assert.Equal(t, http.StatusPreconditionRequired, rec.Code)
}

func TestPutWrongContentTypeIs415(t *testing.T) {
	h := newHandler(t, nil, DocumentPlaneOnly)
	rec := do(h, http.MethodPut, "/a.tsvt", "1\n", map[string]string{
		"Content-Type": "text/plain", "If-None-Match": "*",
	})
	assert.Equal(t, http.StatusUnsupportedMediaType, rec.Code)
}

func TestPutUnparsableBodyIs422(t *testing.T) {
	h := newHandler(t, nil, DocumentPlaneOnly)
	rec := do(h, http.MethodPut, "/a.tsvt", "=(\n", map[string]string{
		"Content-Type": TypeDoc, "If-None-Match": "*",
	})
	assert.Equal(t, http.StatusUnprocessableEntity, rec.Code)
}

func TestPostAppliesEdits(t *testing.T) {
	h := newHandler(t, map[string]string{"a.tsvt": "1\t2\n"}, DocumentPlaneOnly)
	rec := do(h, http.MethodPost, "/a.tsvt", "setCell\tB1\t9\n", map[string]string{
		"Content-Type": TypeEdits, "If-Match": etag(t, "1\t2\n"),
	})
	assert.Equal(t, http.StatusNoContent, rec.Code)
	assert.Equal(t, etag(t, "1\t9\n"), rec.Header().Get("ETag"))
	got := do(h, http.MethodGet, "/a.tsvt", "", nil)
	assert.Equal(t, "1\t9\n", got.Body.String())
}

func TestPostWithoutIfMatchIs428(t *testing.T) {
	h := newHandler(t, map[string]string{"a.tsvt": "1\n"}, DocumentPlaneOnly)
	rec := do(h, http.MethodPost, "/a.tsvt", "setCell\tA1\t2\n", map[string]string{"Content-Type": TypeEdits})
	assert.Equal(t, http.StatusPreconditionRequired, rec.Code)
}

func TestPostStaleIfMatchIs412(t *testing.T) {
	h := newHandler(t, map[string]string{"a.tsvt": "1\n"}, DocumentPlaneOnly)
	rec := do(h, http.MethodPost, "/a.tsvt", "setCell\tA1\t2\n", map[string]string{
		"Content-Type": TypeEdits, "If-Match": etag(t, "stale\n"),
	})
	assert.Equal(t, http.StatusPreconditionFailed, rec.Code)
}

func TestPostMalformedIfMatchIs400(t *testing.T) {
	h := newHandler(t, map[string]string{"a.tsvt": "1\n"}, DocumentPlaneOnly)
	rec := do(h, http.MethodPost, "/a.tsvt", "setCell\tA1\t2\n", map[string]string{
		"Content-Type": TypeEdits, "If-Match": "not-quoted",
	})
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestPostWrongContentTypeIs415(t *testing.T) {
	h := newHandler(t, map[string]string{"a.tsvt": "1\n"}, DocumentPlaneOnly)
	rec := do(h, http.MethodPost, "/a.tsvt", "setCell\tA1\t2\n", map[string]string{
		"Content-Type": "application/json", "If-Match": etag(t, "1\n"),
	})
	assert.Equal(t, http.StatusUnsupportedMediaType, rec.Code)
}

func TestPostMalformedEditsIs400(t *testing.T) {
	h := newHandler(t, map[string]string{"a.tsvt": "1\n"}, DocumentPlaneOnly)
	rec := do(h, http.MethodPost, "/a.tsvt", "nope\tA1\n", map[string]string{
		"Content-Type": TypeEdits, "If-Match": etag(t, "1\n"),
	})
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "bad-edits")
}

func TestPostConflictingInBandBaseIs422(t *testing.T) {
	h := newHandler(t, map[string]string{"a.tsvt": "1\n"}, DocumentPlaneOnly)
	body := "#.base\t" + revision(t, "other\n") + "\nsetCell\tA1\t2\n"
	rec := do(h, http.MethodPost, "/a.tsvt", body, map[string]string{
		"Content-Type": TypeEdits, "If-Match": etag(t, "1\n"),
	})
	assert.Equal(t, http.StatusUnprocessableEntity, rec.Code)
}

func TestPostRefusedOpIs422NamingTheLine(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "a.tsvt"), []byte("1\n"), 0o600))
	st, err := store.Open(store.RootDir(dir), tsvsheet.Limits{ResultCells: 4, GridDim: 2, ResultBytes: 64})
	require.NoError(t, err)
	h := NewHandler(
		Config{
			Port: st, Limits: tsvsheet.DefaultLimits(), ComputeEnabled: DocumentPlaneOnly, Clock: fixedClock,
		},
	)
	rec := do(h, http.MethodPost, "/a.tsvt", "setCell\tE9\tfar\n", map[string]string{
		"Content-Type": TypeEdits, "If-Match": etag(t, "1\n"),
	})
	assert.Equal(t, http.StatusUnprocessableEntity, rec.Code)
	assert.Contains(t, rec.Body.String(), "refused-edits")
}

func TestPostIdempotencyKeyReplays(t *testing.T) {
	h := newHandler(t, map[string]string{"a.tsvt": "1\t2\n"}, DocumentPlaneOnly)
	header := map[string]string{
		"Content-Type": TypeEdits, "If-Match": etag(t, "1\t2\n"), "Idempotency-Key": "k1",
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
	h := newHandler(t, map[string]string{"a.tsvt": "1\n"}, DocumentPlaneOnly)
	rec := do(h, http.MethodDelete, "/a.tsvt", "", map[string]string{"If-Match": etag(t, "1\n")})
	assert.Equal(t, http.StatusNoContent, rec.Code)
	got := do(h, http.MethodGet, "/a.tsvt", "", nil)
	assert.Equal(t, http.StatusNotFound, got.Code)
}

func TestDeleteStaleIs412AndMissingIs404(t *testing.T) {
	h := newHandler(t, map[string]string{"a.tsvt": "1\n"}, DocumentPlaneOnly)
	rec := do(h, http.MethodDelete, "/a.tsvt", "", map[string]string{"If-Match": etag(t, "x\n")})
	assert.Equal(t, http.StatusPreconditionFailed, rec.Code)
	rec = do(h, http.MethodDelete, "/absent.tsvt", "", map[string]string{"If-Match": etag(t, "1\n")})
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

func TestDeleteWithoutIfMatchIs428(t *testing.T) {
	h := newHandler(t, map[string]string{"a.tsvt": "1\n"}, DocumentPlaneOnly)
	rec := do(h, http.MethodDelete, "/a.tsvt", "", nil)
	assert.Equal(t, http.StatusPreconditionRequired, rec.Code)
}

func TestOversizeBodiesAre413(t *testing.T) {
	h := newHandler(t, map[string]string{"a.tsvt": "1\n"}, DocumentPlaneOnly)
	huge := strings.Repeat("x", (4<<20)+1)
	rec := do(h, http.MethodPost, "/a.tsvt", huge, map[string]string{
		"Content-Type": TypeEdits, "If-Match": etag(t, "1\n"),
	})
	assert.Equal(t, http.StatusRequestEntityTooLarge, rec.Code)
	rec = do(h, http.MethodPut, "/a.tsvt", huge, map[string]string{
		"Content-Type": TypeDoc, "If-Match": etag(t, "1\n"),
	})
	assert.Equal(t, http.StatusRequestEntityTooLarge, rec.Code)
}
