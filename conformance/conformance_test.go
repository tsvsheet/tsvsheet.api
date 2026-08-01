// The document plane's conformance suite: one table of behaviours, run
// against every Port implementation.
//
// This is the artifact that makes the port real rather than decorative. The
// embedded adapter reaches a filesystem and the client adapter reaches an HTTP
// server, but a frontend holding either must observe the same results and the
// same sentinels — so the assertions below are shared verbatim, with no
// adapter-specific branches. A behaviour the two cannot share is a design
// flaw, and the honest place to discover it is here.
package conformance_test

import (
	"context"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	tsvsheet "github.com/tsvsheet/go-tsvsheet"

	"github.com/tsvsheet/tsvsheet.api/api"
	"github.com/tsvsheet/tsvsheet.api/client"
	"github.com/tsvsheet/tsvsheet.api/conformance"
	"github.com/tsvsheet/tsvsheet.api/document"
	"github.com/tsvsheet/tsvsheet.api/store"
)

// adapters are the Port implementations every behaviour below must satisfy.
// Each builder returns a port over a root seeded with the given documents.
var adapters = map[string]func(*testing.T, map[string]string) document.Port{
	"embedded": embeddedPort,
	"client":   clientPort,
}

// embeddedPort is the in-process adapter: a confined store, no listener.
func embeddedPort(t *testing.T, files map[string]string) document.Port {
	t.Helper()
	return openStore(t, files)
}

// clientPort is the remote adapter, pointed at a live server over the same
// kind of store — so any difference the suite sees is the adapters', not the
// backing.
func clientPort(t *testing.T, files map[string]string) document.Port {
	t.Helper()
	handler := api.NewHandler(api.Config{
		Port:           openStore(t, files),
		Limits:         tsvsheet.DefaultLimits(),
		ComputeEnabled: api.WithComputePlane,
		Clock:          func() time.Time { return time.Unix(1_700_000_000, 0).UTC() },
	})
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return client.New(client.BaseURL(server.URL), server.Client())
}

// openStore seeds a temp root and confines a store to it.
func openStore(t *testing.T, files map[string]string) *store.Store {
	t.Helper()
	dir := t.TempDir()
	for name, content := range files {
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600))
	}
	st, err := store.Open(store.RootDir(dir), tsvsheet.DefaultLimits())
	require.NoError(t, err)
	return st
}

// seeded is the document every behaviour starts from.
const seeded = conformance.Seed

// revisionOf content-addresses source through the engine.
func revisionOf(t *testing.T, src string) tsvsheet.RevisionHex {
	t.Helper()
	doc, err := tsvsheet.ParseDocument([]byte(src))
	require.NoError(t, err)
	return tsvsheet.Revision(doc)
}

// editsOf parses one batch.
func editsOf(t *testing.T, src string) tsvsheet.Edits {
	t.Helper()
	batch, err := tsvsheet.ParseEdits([]byte(src))
	require.NoError(t, err)
	return batch
}

// conform runs one behaviour against every adapter.
func conform(t *testing.T, name string, files map[string]string, behave func(*testing.T, document.Port)) {
	t.Helper()
	for adapter, build := range adapters {
		t.Run(name+"/"+adapter, func(t *testing.T) {
			behave(t, build(t, files))
		})
	}
}

// oneDoc is the seeded fixture set the suite starts every behaviour from.
func oneDoc() map[string]string { return conformance.Seeded() }

func TestConformanceGetServesCanonicalSourceAndRevision(t *testing.T) {
	conform(t, "get", oneDoc(), func(t *testing.T, port document.Port) {
		snap, err := port.Get(context.Background(), "a.tsvt")
		require.NoError(t, err)
		assert.Equal(t, seeded, string(snap.Text))
		assert.Equal(t, revisionOf(t, seeded), snap.Rev)
	})
}

func TestConformanceGetCanonicalizes(t *testing.T) {
	// A stored document missing its trailing newline is served canonical, and
	// the revision addresses the canonical bytes rather than the raw file.
	conform(t, "canonicalize", map[string]string{"a.tsvt": "1\t2"}, func(t *testing.T, port document.Port) {
		snap, err := port.Get(context.Background(), "a.tsvt")
		require.NoError(t, err)
		assert.Equal(t, "1\t2\n", string(snap.Text))
		assert.Equal(t, revisionOf(t, "1\t2\n"), snap.Rev)
	})
}

func TestConformanceGetMissing(t *testing.T) {
	conform(t, "missing", oneDoc(), func(t *testing.T, port document.Port) {
		_, err := port.Get(context.Background(), "absent.tsvt")
		require.Error(t, err)
		assert.ErrorIs(t, err, document.ErrMissing)
	})
}

func TestConformanceGetRefusedPath(t *testing.T) {
	conform(t, "refused-path", oneDoc(), func(t *testing.T, port document.Port) {
		_, err := port.Get(context.Background(), "../escape.tsvt")
		require.Error(t, err)
		assert.ErrorIs(t, err, document.ErrMissing)
	})
}

func TestConformanceApplyAdvancesTheDocument(t *testing.T) {
	conform(t, "apply", oneDoc(), func(t *testing.T, port document.Port) {
		applied, err := port.Apply(
			context.Background(),
			"a.tsvt",
			editsOf(t, "setCell\tB1\t9\n"),
			revisionOf(t, seeded),
		)
		require.NoError(t, err)
		assert.Equal(t, revisionOf(t, seeded), applied.Old.Rev)
		assert.Equal(t, revisionOf(t, "1\t9\n3\t=A1+B1\n"), applied.New.Rev)
		snap, err := port.Get(context.Background(), "a.tsvt")
		require.NoError(t, err)
		assert.Equal(t, "1\t9\n3\t=A1+B1\n", string(snap.Text))
	})
}

func TestConformanceApplyStaleRevisionRefuses(t *testing.T) {
	conform(t, "apply-stale", oneDoc(), func(t *testing.T, port document.Port) {
		_, err := port.Apply(context.Background(), "a.tsvt", editsOf(t, "setCell\tB1\t9\n"), revisionOf(t, "other\n"))
		require.Error(t, err)
		assert.ErrorIs(t, err, document.ErrPrecondition)
		snap, getErr := port.Get(context.Background(), "a.tsvt")
		require.NoError(t, getErr)
		assert.Equal(t, seeded, string(snap.Text), "a refused batch leaves the document untouched")
	})
}

func TestConformanceApplyConflictingInBandBaseRefuses(t *testing.T) {
	conform(t, "apply-base-conflict", oneDoc(), func(t *testing.T, port document.Port) {
		batch := editsOf(t, "#.base\t"+string(revisionOf(t, "other\n"))+"\nsetCell\tB1\t9\n")
		_, err := port.Apply(context.Background(), "a.tsvt", batch, revisionOf(t, seeded))
		require.Error(t, err)
		assert.ErrorIs(t, err, tsvsheet.ErrEditsBase)
	})
}

func TestConformanceApplyRefusedOp(t *testing.T) {
	conform(t, "apply-refused-op", oneDoc(), func(t *testing.T, port document.Port) {
		// A comment-marker cell would delete its row on the next read, so the
		// engine refuses it — through either adapter, as the same sentinel.
		batch := editsOf(t, "setCell\tA1\t#.note\n")
		_, err := port.Apply(context.Background(), "a.tsvt", batch, revisionOf(t, seeded))
		require.Error(t, err)
		assert.ErrorIs(t, err, tsvsheet.ErrEditsApply)
	})
}

func TestConformanceApplyMissingDocument(t *testing.T) {
	conform(t, "apply-missing", oneDoc(), func(t *testing.T, port document.Port) {
		_, err := port.Apply(context.Background(), "absent.tsvt", editsOf(t, "setCell\tA1\t1\n"), revisionOf(t, seeded))
		require.Error(t, err)
		assert.ErrorIs(t, err, document.ErrMissing)
	})
}
