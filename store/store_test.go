package store_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	tsvsheet "github.com/tsvsheet/go-tsvsheet"

	"github.com/tsvsheet/tsvsheet.api/document"
	"github.com/tsvsheet/tsvsheet.api/store"
)

// opened returns a store over a fresh temp root seeded with the given files.
func opened(t *testing.T, files map[string]string) *store.Store {
	t.Helper()
	dir := t.TempDir()
	for name, content := range files {
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600))
	}
	st, err := store.Open(store.RootDir(dir), tsvsheet.DefaultLimits())
	require.NoError(t, err)
	return st
}

// revision content-addresses src through the engine.
func revision(t *testing.T, src string) tsvsheet.RevisionHex {
	t.Helper()
	doc, err := tsvsheet.ParseDocument([]byte(src))
	require.NoError(t, err)
	return tsvsheet.Revision(doc)
}

// edits parses src as an edits document.
func edits(t *testing.T, src string) tsvsheet.Edits {
	t.Helper()
	batch, err := tsvsheet.ParseEdits([]byte(src))
	require.NoError(t, err)
	return batch
}

func TestOpenMissingRoot(t *testing.T) {
	_, err := store.Open(store.RootDir(filepath.Join(t.TempDir(), "absent")), tsvsheet.DefaultLimits())
	require.Error(t, err)
	assert.ErrorIs(t, err, store.ErrRootOpen)
}

func TestGetReturnsCanonicalTextAndRevision(t *testing.T) {
	st := opened(t, map[string]string{"a.tsvt": "1\t2\n"})
	snap, err := st.Get(t.Context(), "a.tsvt")
	require.NoError(t, err)
	assert.Equal(t, "1\t2\n", string(snap.Text))
	assert.Equal(t, revision(t, "1\t2\n"), snap.Rev)
}

func TestGetCanonicalizesLegacyForms(t *testing.T) {
	// A file missing its trailing newline is served canonicalized, and the
	// revision names the canonical bytes, not the raw file.
	st := opened(t, map[string]string{"a.tsvt": "1\t2"})
	snap, err := st.Get(t.Context(), "a.tsvt")
	require.NoError(t, err)
	assert.Equal(t, "1\t2\n", string(snap.Text))
	assert.Equal(t, revision(t, "1\t2\n"), snap.Rev)
}

func TestGetMissing(t *testing.T) {
	st := opened(t, nil)
	_, err := st.Get(t.Context(), "absent.tsvt")
	require.Error(t, err)
	assert.ErrorIs(t, err, document.ErrMissing)
}

func TestGetUnparsableStoredDocument(t *testing.T) {
	st := opened(t, map[string]string{"bad.tsvt": "=(\n"})
	_, err := st.Get(t.Context(), "bad.tsvt")
	require.Error(t, err)
	assert.ErrorIs(t, err, store.ErrParse)
}

func TestPathRefusals(t *testing.T) {
	st := opened(t, map[string]string{"a.tsvt": "1\n"})
	for name, p := range map[string]store.DocPath{
		"empty":       "",
		"dot":         ".",
		"absolute":    "/etc/passwd",
		"traversal":   "../a.tsvt",
		"inner up":    "x/../../a.tsvt",
		"bang":        "a!b.tsvt",
		"non-clean":   "x//y.tsvt",
		"trailing up": "..",
	} {
		t.Run(name, func(t *testing.T) {
			_, err := st.Get(t.Context(), p)
			require.Error(t, err)
			assert.ErrorIs(t, err, document.ErrPath)
		})
	}
}

func TestPutCreates(t *testing.T) {
	st := opened(t, nil)
	snap, created, err := st.Put(t.Context(), "new.tsvt", []byte("1\t2\n"), document.ExpectAbsent())
	require.NoError(t, err)
	assert.True(t, bool(created))
	assert.Equal(t, revision(t, "1\t2\n"), snap.Rev)
	got, err := st.Get(t.Context(), "new.tsvt")
	require.NoError(t, err)
	assert.Equal(t, "1\t2\n", string(got.Text))
}

func TestPutCreateOverExistingRefused(t *testing.T) {
	st := opened(t, map[string]string{"a.tsvt": "1\n"})
	_, _, err := st.Put(t.Context(), "a.tsvt", []byte("2\n"), document.ExpectAbsent())
	require.Error(t, err)
	assert.ErrorIs(t, err, document.ErrExists)
}

func TestPutReplaceWithMatchingRevision(t *testing.T) {
	st := opened(t, map[string]string{"a.tsvt": "1\n"})
	snap, created, err := st.Put(t.Context(), "a.tsvt", []byte("2\n"), document.ExpectRev(revision(t, "1\n")))
	require.NoError(t, err)
	assert.False(t, bool(created))
	assert.Equal(t, revision(t, "2\n"), snap.Rev)
}

func TestPutReplaceStaleRevision(t *testing.T) {
	st := opened(t, map[string]string{"a.tsvt": "1\n"})
	_, _, err := st.Put(t.Context(), "a.tsvt", []byte("2\n"), document.ExpectRev(revision(t, "stale\n")))
	require.Error(t, err)
	assert.ErrorIs(t, err, document.ErrPrecondition)
}

func TestPutReplaceMissingDocument(t *testing.T) {
	st := opened(t, nil)
	_, _, err := st.Put(t.Context(), "a.tsvt", []byte("2\n"), document.ExpectRev(revision(t, "1\n")))
	require.Error(t, err)
	assert.ErrorIs(t, err, document.ErrMissing)
}

func TestPutCanonicalizesBody(t *testing.T) {
	st := opened(t, map[string]string{})
	snap, _, err := st.Put(t.Context(), "a.tsvt", []byte("1\t2"), document.ExpectAbsent())
	require.NoError(t, err)
	assert.Equal(t, revision(t, "1\t2\n"), snap.Rev)
}

func TestPutUnparsableBody(t *testing.T) {
	st := opened(t, nil)
	_, _, err := st.Put(t.Context(), "a.tsvt", []byte("=(\n"), document.ExpectAbsent())
	require.Error(t, err)
	assert.ErrorIs(t, err, document.ErrSyntax)
}

func TestPutIntoSubdirectory(t *testing.T) {
	st := opened(t, nil)
	_, created, err := st.Put(t.Context(), "lib/inner.tsvt", []byte("1\n"), document.ExpectAbsent())
	require.NoError(t, err)
	assert.True(t, bool(created))
	got, err := st.Get(t.Context(), "lib/inner.tsvt")
	require.NoError(t, err)
	assert.Equal(t, "1\n", string(got.Text))
}

func TestApplyAdvancesTheDocument(t *testing.T) {
	st := opened(t, map[string]string{"a.tsvt": "1\t2\n"})
	applied, err := st.Apply(t.Context(), "a.tsvt", edits(t, "setCell\tB1\t9\n"), revision(t, "1\t2\n"))
	require.NoError(t, err)
	assert.Equal(t, revision(t, "1\t2\n"), applied.Old.Rev)
	assert.Equal(t, revision(t, "1\t9\n"), applied.New.Rev)
	got, err := st.Get(t.Context(), "a.tsvt")
	require.NoError(t, err)
	assert.Equal(t, "1\t9\n", string(got.Text))
}

func TestApplyStaleRevisionLeavesFile(t *testing.T) {
	st := opened(t, map[string]string{"a.tsvt": "1\t2\n"})
	_, err := st.Apply(t.Context(), "a.tsvt", edits(t, "setCell\tB1\t9\n"), revision(t, "other\n"))
	require.Error(t, err)
	assert.ErrorIs(t, err, document.ErrPrecondition)
	got, getErr := st.Get(t.Context(), "a.tsvt")
	require.NoError(t, getErr)
	assert.Equal(t, "1\t2\n", string(got.Text))
}

func TestApplyMissingDocument(t *testing.T) {
	st := opened(t, nil)
	_, err := st.Apply(t.Context(), "a.tsvt", edits(t, "setCell\tB1\t9\n"), revision(t, "1\n"))
	require.Error(t, err)
	assert.ErrorIs(t, err, document.ErrMissing)
}

func TestApplyConflictingInBandBaseRefused(t *testing.T) {
	// The engine's own base check: an in-band #.base that disagrees with the
	// document is ErrEditsBase, and the file is untouched.
	st := opened(t, map[string]string{"a.tsvt": "1\t2\n"})
	batch := edits(t, "#.base\t"+string(revision(t, "other\n"))+"\nsetCell\tB1\t9\n")
	_, err := st.Apply(t.Context(), "a.tsvt", batch, revision(t, "1\t2\n"))
	require.Error(t, err)
	assert.ErrorIs(t, err, tsvsheet.ErrEditsBase)
	got, getErr := st.Get(t.Context(), "a.tsvt")
	require.NoError(t, getErr)
	assert.Equal(t, "1\t2\n", string(got.Text))
}

func TestApplyRefusedOpLeavesFile(t *testing.T) {
	// A store with a tiny grid limit refuses an op targeting a far cell —
	// ErrEditsApply wrapping the engine's refusal, file untouched.
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "a.tsvt"), []byte("1\t2\n"), 0o600))
	st, err := store.Open(store.RootDir(dir), tsvsheet.Limits{ResultCells: 4, GridDim: 2, ResultBytes: 64})
	require.NoError(t, err)
	_, applyErr := st.Apply(t.Context(), "a.tsvt", edits(t, "setCell\tE9\tfar\n"), revision(t, "1\t2\n"))
	require.Error(t, applyErr)
	assert.ErrorIs(t, applyErr, tsvsheet.ErrEditsApply)
	got, getErr := st.Get(t.Context(), "a.tsvt")
	require.NoError(t, getErr)
	assert.Equal(t, "1\t2\n", string(got.Text))
}

func TestDeleteWithMatchingRevision(t *testing.T) {
	st := opened(t, map[string]string{"a.tsvt": "1\n"})
	require.NoError(t, st.Delete(t.Context(), "a.tsvt", revision(t, "1\n")))
	_, err := st.Get(t.Context(), "a.tsvt")
	require.Error(t, err)
	assert.ErrorIs(t, err, document.ErrMissing)
}

func TestDeleteStaleRevision(t *testing.T) {
	st := opened(t, map[string]string{"a.tsvt": "1\n"})
	err := st.Delete(t.Context(), "a.tsvt", revision(t, "2\n"))
	require.Error(t, err)
	assert.ErrorIs(t, err, document.ErrPrecondition)
	_, getErr := st.Get(t.Context(), "a.tsvt")
	require.NoError(t, getErr)
}

func TestDeleteMissing(t *testing.T) {
	st := opened(t, nil)
	err := st.Delete(t.Context(), "a.tsvt", revision(t, "1\n"))
	require.Error(t, err)
	assert.ErrorIs(t, err, document.ErrMissing)
}

// TestStore_RefusesOverBudgetDocuments pins the 018 bounded store on both
// sides of the port: reading a stored document past the resident budget
// refuses as ErrDocTooLarge (never an unbounded materialization), and a Put
// whose body is over the same budget refuses identically — a client may not
// store what the server could never load back. An in-budget write on the same
// store succeeds, so the budget is the only thing refusing.
func TestStore_RefusesOverBudgetDocuments(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "big.tsvt"), []byte("1\t2\n3\t4\n"), 0o600))
	tight := tsvsheet.DefaultLimits()
	tight.ResidentCells = 1
	st, err := store.Open(store.RootDir(dir), tight)
	require.NoError(t, err)

	_, err = st.Get(context.Background(), "big.tsvt")
	require.ErrorIs(t, err, tsvsheet.ErrDocTooLarge)
	assert.Contains(
		t,
		err.Error(),
		"path",
		"the refusal comes from the census pre-flight (which names the path), never from a parse after buffering",
	)

	_, _, err = st.Put(context.Background(), "new.tsvt", []byte("5\t6\n7\t8\n"), document.ExpectAbsent())
	require.ErrorIs(t, err, tsvsheet.ErrDocTooLarge)

	_, _, err = st.Put(context.Background(), "ok.tsvt", []byte("5\n"), document.ExpectAbsent())
	require.NoError(t, err)
}
