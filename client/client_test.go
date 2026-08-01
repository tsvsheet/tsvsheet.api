// The client's fault paths: what a frontend sees when the server is
// unreachable, answers something it should not, or contradicts itself. The
// conformance suite proves the happy contract against a healthy server; these
// prove the adapter reports a broken one as unavailability rather than
// inventing a document.
package client_test

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
	tsvsheet "github.com/tsvsheet/go-tsvsheet"

	"github.com/tsvsheet/tsvsheet.api/api"
	"github.com/tsvsheet/tsvsheet.api/client"
)

// tagFor is the entity tag a well-behaved server sends for source bytes.
func tagFor(t *testing.T, src string) string {
	t.Helper()
	doc, err := tsvsheet.ParseDocument([]byte(src))
	require.NoError(t, err)
	return `"` + string(tsvsheet.Revision(doc)) + `"`
}

// serveDoc writes a source document the way the real handler does, tag and all.
func serveDoc(t *testing.T, w http.ResponseWriter, src string) {
	t.Helper()
	w.Header().Set("Content-Type", api.TypeDoc)
	w.Header().Set("ETag", tagFor(t, src))
	_, _ = w.Write([]byte(src))
}

// answering builds a client against a server that returns one canned
// response, so a specific malformed answer can be put in front of the adapter.
func answering(t *testing.T, respond http.HandlerFunc) client.Client {
	t.Helper()
	server := httptest.NewServer(respond)
	t.Cleanup(server.Close)
	return client.New(client.BaseURL(server.URL), server.Client())
}

// failingDoer fails every request, standing in for an unreachable server.
type failingDoer struct{}

func (failingDoer) Do(*http.Request) (*http.Response, error) {
	return nil, errors.New("connection refused")
}
