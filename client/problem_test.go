// The client's fault paths: what a frontend sees when the server is
// unreachable, answers something it should not, or contradicts itself. The
// conformance suite proves the happy contract against a healthy server; these
// prove the adapter reports a broken one as unavailability rather than
// inventing a document.
package client_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	tsvsheet "github.com/tsvsheet/go-tsvsheet"

	"github.com/tsvsheet/tsvsheet.api/api"
	"github.com/tsvsheet/tsvsheet.api/document"
)

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

// TestUnrecognizedRefusalIsErrUnavailableNotAGuess names ErrUnavailable's
// contract: a transport fault, never a statement about the document — asserted
// both ways, ErrorIs the transport sentinel and NotErrorIs a document one.
func TestUnrecognizedRefusalIsErrUnavailableNotAGuess(t *testing.T) {
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
			serveDoc(t, w, "1\n")
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

func TestApplyReportsARefusedBatchAfterAGoodRead(t *testing.T) {
	t.Parallel()
	// The read succeeds and the batch is sent; the server refuses the write,
	// and that refusal — not the read — is what the caller sees.
	c := answering(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			serveDoc(t, w, "1\n")
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

// TestApplyReportsWhatItProducedNotWhatItLaterRead pins the fix for a race an
// adversarial pass reproduced: the client used to read the document back after
// its write, so an interposing writer made it report a state some other batch
// produced — with a nil error. It now folds the accepted batch locally and
// checks the result against the revision the server named.
func TestApplyReportsWhatItProducedNotWhatItLaterRead(t *testing.T) {
	t.Parallel()
	const before = "1\t2\n"
	after := "1\t9\n"
	c := answering(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			serveDoc(t, w, before)
			return
		}
		// The server accepted the batch and names the revision it produced;
		// any later read could show a third party's write instead.
		w.Header().Set("ETag", tagFor(t, after))
		w.WriteHeader(http.StatusNoContent)
	})
	batch, err := tsvsheet.ParseEdits([]byte("setCell\tB1\t9\n"))
	require.NoError(t, err)
	applied, err := c.Apply(context.Background(), "a.tsvt", batch, "")
	require.NoError(t, err)
	assert.Equal(t, after, string(applied.New.Text), "the reported state is the one the batch produced")
	assert.Equal(t, before, string(applied.Old.Text))
}

// TestApplyRefusesAServerThatNamesADifferentResult pins the verification the
// local fold buys: one engine means the client's result must equal the
// server's, and a disagreement is a broken server, not something to paper over.
func TestApplyRefusesAServerThatNamesADifferentResult(t *testing.T) {
	t.Parallel()
	c := answering(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			serveDoc(t, w, "1\t2\n")
			return
		}
		w.Header().Set("ETag", tagFor(t, "SOMETHING\tELSE\n"))
		w.WriteHeader(http.StatusNoContent)
	})
	batch, err := tsvsheet.ParseEdits([]byte("setCell\tB1\t9\n"))
	require.NoError(t, err)
	_, err = c.Apply(context.Background(), "a.tsvt", batch, "")
	require.Error(t, err)
	assert.ErrorIs(t, err, document.ErrUnavailable)
}

func TestApplyRefusesABatchTheLocalFoldRejects(t *testing.T) {
	t.Parallel()
	// The server said yes to something the engine will not do; the client must
	// not manufacture a result for it.
	c := answering(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			serveDoc(t, w, "1\t2\n")
			return
		}
		w.Header().Set("ETag", tagFor(t, "1\t2\n"))
		w.WriteHeader(http.StatusNoContent)
	})
	batch, err := tsvsheet.ParseEdits([]byte("setCell\tA1\t#.note\n"))
	require.NoError(t, err)
	_, err = c.Apply(context.Background(), "a.tsvt", batch, "")
	require.Error(t, err)
	assert.ErrorIs(t, err, document.ErrUnavailable)
}

// TestGetRefusesAnAnswerWithNoEntityTag pins that an unverifiable answer is an
// unusable one: without a tag there is nothing to check a possibly-truncated
// body against.
func TestGetRefusesAnAnswerWithNoEntityTag(t *testing.T) {
	t.Parallel()
	c := answering(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", api.TypeDoc)
		_, _ = w.Write([]byte("EVIL\n"))
	})
	_, err := c.Get(context.Background(), "a.tsvt")
	require.Error(t, err)
	assert.ErrorIs(t, err, document.ErrUnavailable)
}
