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
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	tsvsheet "github.com/tsvsheet/go-tsvsheet"

	"github.com/tsvsheet/tsvsheet.api/api"
	"github.com/tsvsheet/tsvsheet.api/client"
	"github.com/tsvsheet/tsvsheet.api/document"
)

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

func TestETagDisagreeingWithTheBodyIsUnavailable(t *testing.T) {
	t.Parallel()
	// A server whose entity tag does not address the bytes it sent has broken
	// the one invariant the whole protocol rests on. Trusting it would let a
	// client build edits against a revision that never existed.
	c := answering(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", api.TypeDoc)
		w.Header().Set("ETag", `"0000000000000000000000000000000000000000000000000000000000000000"`)
		_, _ = w.Write([]byte("1\t2\n"))
	})
	_, err := c.Get(context.Background(), "a.tsvt")
	require.Error(t, err)
	assert.ErrorIs(t, err, document.ErrUnavailable)
}

func TestUnparsableBodyIsUnavailable(t *testing.T) {
	t.Parallel()
	c := answering(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", api.TypeDoc)
		_, _ = w.Write([]byte("=(\n"))
	})
	_, err := c.Get(context.Background(), "a.tsvt")
	require.Error(t, err)
	assert.ErrorIs(t, err, document.ErrUnavailable)
}

func TestUnrecognizedRefusalIsUnavailableNotAGuess(t *testing.T) {
	t.Parallel()
	// A refusal the adapter cannot map must not be turned into a document
	// sentinel: guessing would make the two adapters disagree silently.
	for name, respond := range map[string]http.HandlerFunc{
		"unknown slug": func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", api.TypeProblem)
			w.WriteHeader(http.StatusTeapot)
			_, _ = w.Write([]byte(`{"type":"` + api.ProblemBase + `something-else","detail":"nope"}`))
		},
		"not json": func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusBadGateway)
			_, _ = w.Write([]byte("<html>gateway</html>"))
		},
		"empty body": func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := answering(t, respond).Get(context.Background(), "a.tsvt")
			require.Error(t, err)
			assert.ErrorIs(t, err, document.ErrUnavailable)
			assert.NotErrorIs(t, err, document.ErrMissing)
		})
	}
}

func TestRefusalDetailReachesTheCaller(t *testing.T) {
	t.Parallel()
	c := answering(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", api.TypeProblem)
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"type":"` + api.ProblemBase + string(api.SlugNotFound) + `","detail":"no such sheet"}`))
	})
	_, err := c.Get(context.Background(), "a.tsvt")
	require.Error(t, err)
	assert.ErrorIs(t, err, document.ErrMissing)
	assert.Contains(t, err.Error(), "no such sheet", "the server's reason survives the mapping")
}

func TestMalformedEditsRefusalMapsToTheEngineSentinel(t *testing.T) {
	t.Parallel()
	c := answering(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			w.Header().Set("Content-Type", api.TypeDoc)
			_, _ = w.Write([]byte("1\n"))
			return
		}
		w.Header().Set("Content-Type", api.TypeProblem)
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"type":"` + api.ProblemBase + string(api.SlugBadEdits) + `","detail":"line 1"}`))
	})
	batch, err := tsvsheet.ParseEdits([]byte("setCell\tA1\tx\n"))
	require.NoError(t, err)
	_, err = c.Apply(context.Background(), "a.tsvt", batch, "")
	require.Error(t, err)
	assert.ErrorIs(t, err, tsvsheet.ErrEditsOp)
}

func TestApplyReportsAFaultyReadBeforeWriting(t *testing.T) {
	t.Parallel()
	// Apply reads the prior state first; if that read is unusable the batch is
	// not sent, so a broken server cannot be edited blind.
	sent := false
	c := answering(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			sent = true
		}
		w.Header().Set("Content-Type", api.TypeDoc)
		_, _ = w.Write([]byte("=(\n"))
	})
	batch, err := tsvsheet.ParseEdits([]byte("setCell\tA1\tx\n"))
	require.NoError(t, err)
	_, err = c.Apply(context.Background(), "a.tsvt", batch, "")
	require.Error(t, err)
	assert.ErrorIs(t, err, document.ErrUnavailable)
	assert.False(t, sent, "no batch is sent to a server whose reads are unusable")
}

func TestDeleteRefusalMaps(t *testing.T) {
	t.Parallel()
	c := answering(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", api.TypeProblem)
		w.WriteHeader(http.StatusPreconditionFailed)
		_, _ = w.Write([]byte(`{"type":"` + api.ProblemBase + string(api.SlugPrecondition) + `","detail":"stale"}`))
	})
	err := c.Delete(context.Background(), "a.tsvt", "abc")
	require.Error(t, err)
	assert.ErrorIs(t, err, document.ErrPrecondition)
}

func TestPutRefusalMaps(t *testing.T) {
	t.Parallel()
	c := answering(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", api.TypeProblem)
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"type":"` + api.ProblemBase + string(api.SlugConflict) + `","detail":"exists"}`))
	})
	_, _, err := c.Put(context.Background(), "a.tsvt", []byte("1\n"), document.ExpectAbsent())
	require.Error(t, err)
	assert.ErrorIs(t, err, document.ErrExists)
}

func TestPutReportsAFaultyReadBackAsUnavailable(t *testing.T) {
	t.Parallel()
	// The write succeeded, but the server then serves something unusable; the
	// caller must not receive a snapshot it cannot trust.
	c := answering(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut {
			w.WriteHeader(http.StatusCreated)
			return
		}
		w.Header().Set("Content-Type", api.TypeDoc)
		_, _ = w.Write([]byte("=(\n"))
	})
	_, _, err := c.Put(context.Background(), "a.tsvt", []byte("1\n"), document.ExpectAbsent())
	require.Error(t, err)
	assert.ErrorIs(t, err, document.ErrUnavailable)
}

func TestApplyReportsAFaultyReadBackAsUnavailable(t *testing.T) {
	t.Parallel()
	reads := 0
	c := answering(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		reads++
		w.Header().Set("Content-Type", api.TypeDoc)
		if reads == 1 {
			_, _ = w.Write([]byte("1\n"))
			return
		}
		_, _ = w.Write([]byte("=(\n"))
	})
	batch, err := tsvsheet.ParseEdits([]byte("setCell\tA1\tx\n"))
	require.NoError(t, err)
	_, err = c.Apply(context.Background(), "a.tsvt", batch, "")
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

func TestApplyReportsARefusedBatchAfterAGoodRead(t *testing.T) {
	t.Parallel()
	// The read succeeds and the batch is sent; the server refuses the write,
	// and that refusal — not the read — is what the caller sees.
	c := answering(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			w.Header().Set("Content-Type", api.TypeDoc)
			_, _ = w.Write([]byte("1\n"))
			return
		}
		w.Header().Set("Content-Type", api.TypeProblem)
		w.WriteHeader(http.StatusPreconditionFailed)
		_, _ = w.Write([]byte(`{"type":"` + api.ProblemBase + string(api.SlugPrecondition) + `","detail":"stale"}`))
	})
	batch, err := tsvsheet.ParseEdits([]byte("setCell\tA1\tx\n"))
	require.NoError(t, err)
	_, err = c.Apply(context.Background(), "a.tsvt", batch, "abc")
	require.Error(t, err)
	assert.ErrorIs(t, err, document.ErrPrecondition)
}

// readableThenTornDoer serves reads but fails writes, standing in for a server
// that goes away between a client's read and its edit.
type readableThenTornDoer struct{}

func (readableThenTornDoer) Do(req *http.Request) (*http.Response, error) {
	if req.Method != http.MethodGet {
		return nil, errors.New("connection reset")
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader("1\n")),
		Header:     http.Header{},
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
