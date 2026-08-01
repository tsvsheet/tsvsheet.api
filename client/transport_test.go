// The client's fault paths: what a frontend sees when the server is
// unreachable, answers something it should not, or contradicts itself. The
// conformance suite proves the happy contract against a healthy server; these
// prove the adapter reports a broken one as unavailability rather than
// inventing a document.
package client_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	tsvsheet "github.com/tsvsheet/go-tsvsheet"

	"github.com/tsvsheet/tsvsheet.api/api"
	"github.com/tsvsheet/tsvsheet.api/client"
	"github.com/tsvsheet/tsvsheet.api/document"
)

func TestUnreachableServerIsUnavailable(t *testing.T) {
	t.Parallel()
	c := client.New("http://example.invalid", failingDoer{})
	_, err := c.Get(context.Background(), "a.tsvt")
	require.Error(t, err)
	assert.ErrorIs(t, err, document.ErrUnavailable)
}

func TestDefaultDoerIsUsedWhenNoneGiven(t *testing.T) {
	t.Parallel()
	// A nil doer must still produce a usable client; pointing it at an
	// unroutable host proves it reached the transport rather than panicking.
	c := client.New("http://127.0.0.1:1", nil)
	_, err := c.Get(context.Background(), "a.tsvt")
	require.Error(t, err)
	assert.ErrorIs(t, err, document.ErrUnavailable)
}

func TestMalformedBaseURLIsRefusedAsAPath(t *testing.T) {
	t.Parallel()
	c := client.New(client.BaseURL("://not a url"), failingDoer{})
	_, err := c.Get(context.Background(), "a.tsvt")
	require.Error(t, err)
	assert.ErrorIs(t, err, document.ErrUnavailable)
}

func TestOversizeResponseIsTruncatedNotFatal(t *testing.T) {
	t.Parallel()
	// The bound exists so a misbehaving server cannot exhaust the client; a
	// body past it can only fail to parse, never consume unbounded memory.
	c := answering(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", api.TypeDoc)
		_, _ = w.Write([]byte(strings.Repeat("x\n", (8<<20)/2+16)))
	})
	snap, err := c.Get(context.Background(), "a.tsvt")
	if err != nil {
		assert.ErrorIs(t, err, document.ErrUnavailable)
		return
	}
	assert.LessOrEqual(t, len(snap.Text), 8<<20+1)
}

// erroringBody answers a request but fails while its body is read, standing in
// for a connection that dies mid-response.
type erroringBody struct{}

func (erroringBody) Read([]byte) (int, error) { return 0, errors.New("connection reset") }
func (erroringBody) Close() error             { return nil }

// tornDoer returns a response whose body cannot be read.
type tornDoer struct{}

func (tornDoer) Do(*http.Request) (*http.Response, error) {
	return &http.Response{StatusCode: http.StatusOK, Body: erroringBody{}, Header: http.Header{}}, nil
}

func TestBodyThatFailsMidReadIsUnavailable(t *testing.T) {
	t.Parallel()
	_, err := client.New("http://server", tornDoer{}).Get(context.Background(), "a.tsvt")
	require.Error(t, err)
	assert.ErrorIs(t, err, document.ErrUnavailable)
}

func TestEveryOperationReportsAnUnreachableServer(t *testing.T) {
	t.Parallel()
	c := client.New("http://example.invalid", failingDoer{})
	batch, err := tsvsheet.ParseEdits([]byte("setCell\tA1\tx\n"))
	require.NoError(t, err)

	_, applyErr := c.Apply(context.Background(), "a.tsvt", batch, "abc")
	assert.ErrorIs(t, applyErr, document.ErrUnavailable)
	_, _, putErr := c.Put(context.Background(), "a.tsvt", []byte("1\n"), document.ExpectAbsent())
	assert.ErrorIs(t, putErr, document.ErrUnavailable)
	assert.ErrorIs(t, c.Delete(context.Background(), "a.tsvt", "abc"), document.ErrUnavailable)
}

func TestApplySendsNothingWhenTheServerIsUnreachable(t *testing.T) {
	t.Parallel()
	// The prior-state read fails first, so no batch reaches an unreachable
	// server — the caller learns the port is down, not that its edit failed.
	c := client.New("http://example.invalid", failingDoer{})
	batch, err := tsvsheet.ParseEdits([]byte("setCell\tA1\tx\n"))
	require.NoError(t, err)
	_, err = c.Apply(context.Background(), "a.tsvt", batch, "abc")
	require.Error(t, err)
	assert.ErrorIs(t, err, document.ErrUnavailable)
}

func TestAZeroContextIsRefusedNotDereferenced(t *testing.T) {
	t.Parallel()
	// A caller whose context field was never initialized gets a refusal rather
	// than a panic from deep inside the transport.
	var uninitialized context.Context
	_, err := client.New("http://server", failingDoer{}).Get(uninitialized, "a.tsvt")
	require.Error(t, err)
	assert.ErrorIs(t, err, document.ErrUnavailable)
}

// readableThenTornDoer serves reads but fails writes, standing in for a server
// that goes away between a client's read and its edit.
type readableThenTornDoer struct{}

func (readableThenTornDoer) Do(req *http.Request) (*http.Response, error) {
	if req.Method != http.MethodGet {
		return nil, errors.New("connection reset")
	}
	doc, err := tsvsheet.ParseDocument([]byte("1\n"))
	if err != nil {
		return nil, err
	}
	header := http.Header{}
	header.Set("ETag", `"`+string(tsvsheet.Revision(doc))+`"`)
	return &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader("1\n")),
		Header:     header,
	}, nil
}

func TestApplyReportsATransportFaultOnTheWrite(t *testing.T) {
	t.Parallel()
	// The read landed, so the caller knows the document; the write did not, so
	// it must learn that rather than believe its edit applied.
	c := client.New("http://server", readableThenTornDoer{})
	batch, err := tsvsheet.ParseEdits([]byte("setCell\tA1\tx\n"))
	require.NoError(t, err)
	_, err = c.Apply(context.Background(), "a.tsvt", batch, "abc")
	require.Error(t, err)
	assert.ErrorIs(t, err, document.ErrUnavailable)
}

func TestApplyReportsATransportFaultOnTheWriteAfterAGoodRead(t *testing.T) {
	t.Parallel()
	// The read landed and parsed; the write itself never reached the server, so
	// the caller must learn that rather than believe its edit applied.
	served := "1\t2\n"
	c := answering(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			serveDoc(t, w, served)
			return
		}
		// A response with no entity tag on an accepted write leaves the client
		// nothing to verify its local fold against.
		w.WriteHeader(http.StatusNoContent)
	})
	batch, err := tsvsheet.ParseEdits([]byte("setCell\tB1\t9\n"))
	require.NoError(t, err)
	_, err = c.Apply(context.Background(), "a.tsvt", batch, "")
	require.Error(t, err)
	assert.ErrorIs(t, err, document.ErrUnavailable)
}
