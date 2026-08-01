package api

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestFeedOnMissingDocumentIs404(t *testing.T) {
	h := newHandler(t, nil, DocumentPlaneOnly)
	rec := do(h, http.MethodGet, "/absent.tsvt", "", map[string]string{"Accept": "text/event-stream"})
	assert.Equal(t, http.StatusNotFound, rec.Code)
}
