// White-box failure-path tests: the write/rename/remove stubs are
// process-global, so every test here runs serially and restores what it swaps.
package store

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	tsvsheet "github.com/tsvsheet/go-tsvsheet"

	"github.com/tsvsheet/tsvsheet.api/document"
)

// seeded returns a store whose root holds a.tsvt with "1\n".
func seeded(t *testing.T) (*Store, string) {
	t.Helper()
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "a.tsvt"), []byte("1\n"), 0o600))
	st, err := Open(RootDir(dir), tsvsheet.DefaultLimits())
	require.NoError(t, err)
	return st, dir
}

// revOf content-addresses src.
func revOf(t *testing.T, src string) tsvsheet.RevisionHex {
	t.Helper()
	doc, err := tsvsheet.ParseDocument([]byte(src))
	require.NoError(t, err)
	return tsvsheet.Revision(doc)
}

// batchOf parses one edits document.
func batchOf(t *testing.T, src string) tsvsheet.Edits {
	t.Helper()
	batch, err := tsvsheet.ParseEdits([]byte(src))
	require.NoError(t, err)
	return batch
}

func TestWriteFailureLeavesDocumentIntact(t *testing.T) {
	st, dir := seeded(t)
	prev := writeFileIn
	writeFileIn = func(*os.Root, string, []byte, os.FileMode) error { return errors.New("disk full") }
	t.Cleanup(func() { writeFileIn = prev })
	_, err := st.Apply(t.Context(), "a.tsvt", batchOf(t, "setCell\tA1\t2\n"), revOf(t, "1\n"))
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrWrite)
	saved, readErr := os.ReadFile(filepath.Join(dir, "a.tsvt"))
	require.NoError(t, readErr)
	assert.Equal(t, "1\n", string(saved))
}

func TestRenameFailureRemovesStagingAndLeavesDocument(t *testing.T) {
	st, dir := seeded(t)
	prev := renameIn
	renameIn = func(*os.Root, string, string) error { return errors.New("cross-device") }
	t.Cleanup(func() { renameIn = prev })
	_, _, err := st.Put(t.Context(), "a.tsvt", []byte("2\n"), document.ExpectRev(revOf(t, "1\n")))
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrWrite)
	saved, readErr := os.ReadFile(filepath.Join(dir, "a.tsvt"))
	require.NoError(t, readErr)
	assert.Equal(t, "1\n", string(saved))
	entries, readDirErr := os.ReadDir(dir)
	require.NoError(t, readDirErr)
	assert.Len(t, entries, 1, "no staging file survives a failed rename")
}

func TestRemoveFailureSurfacesAsWriteError(t *testing.T) {
	st, _ := seeded(t)
	prev := removeIn
	removeIn = func(*os.Root, string) error { return errors.New("busy") }
	t.Cleanup(func() { removeIn = prev })
	err := st.Delete(t.Context(), "a.tsvt", revOf(t, "1\n"))
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrWrite)
}

func TestPathThroughAFileIsMissingBothWays(t *testing.T) {
	// "sub" is a file, so nothing resolves at sub/x.tsvt: the read is a missing
	// document, and so is the write — a client-named path that cannot exist is
	// not a server fault.
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "sub"), []byte("a file"), 0o600))
	st, err := Open(RootDir(dir), tsvsheet.DefaultLimits())
	require.NoError(t, err)
	_, getErr := st.Get(t.Context(), "sub/x.tsvt")
	require.Error(t, getErr)
	assert.ErrorIs(t, getErr, document.ErrMissing)
	_, _, putErr := st.Put(t.Context(), "sub/x.tsvt", []byte("1\n"), document.ExpectAbsent())
	require.Error(t, putErr)
	assert.ErrorIs(t, putErr, document.ErrMissing)
}

// TestEscapingSymlinkIsMissingNotAServerFault pins that a client-named path
// os.Root refuses reads as "not found": the content is withheld either way,
// but a 500 would blame the server and disclose that the escaping name exists.
func TestEscapingSymlinkIsMissingNotAServerFault(t *testing.T) {
	outside := filepath.Join(t.TempDir(), "secret.tsvt")
	require.NoError(t, os.WriteFile(outside, []byte("secret\n"), 0o600))
	dir := t.TempDir()
	require.NoError(t, os.Symlink(outside, filepath.Join(dir, "link.tsvt")))
	st, err := Open(RootDir(dir), tsvsheet.DefaultLimits())
	require.NoError(t, err)
	_, getErr := st.Get(t.Context(), "link.tsvt")
	require.Error(t, getErr)
	assert.ErrorIs(t, getErr, document.ErrMissing)
	assert.NotContains(t, errText(getErr), "secret")
}

func TestInvalidNameIsMissing(t *testing.T) {
	st, _ := seeded(t)
	_, err := st.Get(t.Context(), DocPath("a.tsvt\x00.png"))
	require.Error(t, err)
	assert.ErrorIs(t, err, document.ErrMissing)
}

func TestErrTextOfNilIsEmpty(t *testing.T) { assert.Empty(t, errText(nil)) }

func TestMkdirFailureSurfacesAsWriteError(t *testing.T) {
	dir := t.TempDir()
	st, err := Open(RootDir(dir), tsvsheet.DefaultLimits())
	require.NoError(t, err)
	prev := mkdirIn
	mkdirIn = func(*os.Root, string, os.FileMode) error { return errors.New("read-only fs") }
	t.Cleanup(func() { mkdirIn = prev })
	_, _, putErr := st.Put(t.Context(), "sub/x.tsvt", []byte("1\n"), document.ExpectAbsent())
	require.Error(t, putErr)
	assert.ErrorIs(t, putErr, ErrWrite)
}

func TestReadFailureOnDirectoryDocument(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(dir, "d.tsvt"), 0o700))
	st, err := Open(RootDir(dir), tsvsheet.DefaultLimits())
	require.NoError(t, err)
	_, getErr := st.Get(t.Context(), "d.tsvt")
	require.Error(t, getErr)
	assert.ErrorIs(t, getErr, ErrRead)
}

func TestPutOverUnparsableExistingPropagatesParseError(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "bad.tsvt"), []byte("=(\n"), 0o600))
	st, err := Open(RootDir(dir), tsvsheet.DefaultLimits())
	require.NoError(t, err)
	_, _, putErr := st.Put(t.Context(), "bad.tsvt", []byte("1\n"), document.ExpectAbsent())
	require.Error(t, putErr)
	assert.ErrorIs(t, putErr, ErrParse)
}

// TestRefusedNamesCarryTheirSyscallCause pins the two OS refusals this file
// classifies as "not found": an escape and a name the kernel rejects. Both are
// asserted through the syscall errors themselves, since neither is expressible
// as a sentinel this package declares.
func TestRefusedNamesCarryTheirSyscallCause(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "sub"), []byte("a file"), 0o600))
	st, err := Open(RootDir(dir), tsvsheet.DefaultLimits())
	require.NoError(t, err)

	_, notDir := st.Get(t.Context(), "sub/x.tsvt")
	require.Error(t, notDir)
	assert.ErrorIs(t, notDir, syscall.ENOTDIR, "a path through a file is ENOTDIR underneath")
	assert.ErrorIs(t, notDir, document.ErrMissing, "and reads as missing to a caller")

	_, invalid := st.Get(t.Context(), DocPath("a\x00b.tsvt"))
	require.Error(t, invalid)
	assert.ErrorIs(t, invalid, syscall.EINVAL, "a NUL in a name is EINVAL underneath")
	assert.ErrorIs(t, invalid, document.ErrMissing)
}

// TestParseStored_MapsBothRefusals pins the stored-parse mapping directly —
// the TOCTOU half of the budget contract (a file that grew between the
// pre-flight and the read still refuses as the sentinel) and the corrupt
// half (a scan-refused document is ErrParse naming the path).
func TestParseStored_MapsBothRefusals(t *testing.T) {
	t.Parallel()

	tight := tsvsheet.DefaultLimits()
	tight.ResidentCells = 1
	_, err := parseStored([]byte("1\t2\n3\t4\n"), tight, "grew.tsvt")
	require.ErrorIs(t, err, tsvsheet.ErrDocTooLarge, "grown-past-the-vet bytes refuse as the sentinel")

	long := append(bytes.Repeat([]byte{'a'}, (1<<20)+2), '\n')
	_, err = parseStored(long, tsvsheet.DefaultLimits(), "corrupt.tsvt")
	require.ErrorIs(t, err, ErrParse)
	assert.Contains(t, err.Error(), "corrupt.tsvt")

	doc, err := parseStored([]byte("ok\n"), tsvsheet.DefaultLimits(), "ok.tsvt")
	require.NoError(t, err)
	assert.Equal(t, []byte("ok\n"), doc.Text())
}

// TestStore_ScanRefusedFileFallsToTheReadPath pins vetFile's fall-through: a
// stored file the census scanner itself refuses (a line past the ceiling) is
// not the pre-flight's to report — the read path answers ErrParse, its
// single voice for corrupt stores.
func TestStore_ScanRefusedFileFallsToTheReadPath(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	long := append(bytes.Repeat([]byte{'a'}, (1<<20)+2), '\n')
	require.NoError(t, os.WriteFile(filepath.Join(dir, "longline.tsvt"), long, 0o600))
	st, err := Open(RootDir(dir), tsvsheet.DefaultLimits())
	require.NoError(t, err)
	_, err = st.Get(context.Background(), "longline.tsvt")
	require.ErrorIs(t, err, ErrParse)
}
