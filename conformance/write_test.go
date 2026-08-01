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
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tsvsheet/tsvsheet.api/conformance"
	"github.com/tsvsheet/tsvsheet.api/document"
)

func TestConformancePutCreates(t *testing.T) {
	conform(t, "put-create", oneDoc(), func(t *testing.T, port document.Port) {
		snap, created, err := port.Put(context.Background(), "new.tsvt", []byte("7\t8"), document.ExpectAbsent())
		require.NoError(t, err)
		assert.True(t, bool(created))
		assert.Equal(t, revisionOf(t, "7\t8\n"), snap.Rev, "the stored form is canonical")
		assert.Equal(t, "7\t8\n", string(snap.Text))
	})
}

func TestConformancePutCreateOverExistingRefuses(t *testing.T) {
	conform(t, "put-exists", oneDoc(), func(t *testing.T, port document.Port) {
		_, _, err := port.Put(context.Background(), "a.tsvt", []byte("9\n"), document.ExpectAbsent())
		require.Error(t, err)
		assert.ErrorIs(t, err, document.ErrExists)
	})
}

func TestConformancePutReplaces(t *testing.T) {
	conform(t, "put-replace", oneDoc(), func(t *testing.T, port document.Port) {
		snap, created, err := port.Put(
			context.Background(), "a.tsvt", []byte("9\n"), document.ExpectRev(revisionOf(t, seeded)),
		)
		require.NoError(t, err)
		assert.False(t, bool(created))
		assert.Equal(t, revisionOf(t, "9\n"), snap.Rev)
	})
}

func TestConformancePutStaleRevisionRefuses(t *testing.T) {
	conform(t, "put-stale", oneDoc(), func(t *testing.T, port document.Port) {
		_, _, err := port.Put(
			context.Background(), "a.tsvt", []byte("9\n"), document.ExpectRev(revisionOf(t, "other\n")),
		)
		require.Error(t, err)
		assert.ErrorIs(t, err, document.ErrPrecondition)
	})
}

func TestConformancePutUnparsableBodyRefuses(t *testing.T) {
	conform(t, "put-syntax", oneDoc(), func(t *testing.T, port document.Port) {
		_, _, err := port.Put(context.Background(), "bad.tsvt", []byte("=(\n"), document.ExpectAbsent())
		require.Error(t, err)
		assert.ErrorIs(t, err, document.ErrSyntax)
	})
}

func TestConformanceDeleteRemoves(t *testing.T) {
	conform(t, "delete", oneDoc(), func(t *testing.T, port document.Port) {
		require.NoError(t, port.Delete(context.Background(), "a.tsvt", revisionOf(t, seeded)))
		_, err := port.Get(context.Background(), "a.tsvt")
		assert.ErrorIs(t, err, document.ErrMissing)
	})
}

func TestConformanceDeleteStaleRevisionRefuses(t *testing.T) {
	conform(t, "delete-stale", oneDoc(), func(t *testing.T, port document.Port) {
		err := port.Delete(context.Background(), "a.tsvt", revisionOf(t, "other\n"))
		require.Error(t, err)
		assert.ErrorIs(t, err, document.ErrPrecondition)
		_, getErr := port.Get(context.Background(), "a.tsvt")
		assert.NoError(t, getErr, "a refused delete leaves the document in place")
	})
}

func TestConformanceDeleteMissingRefuses(t *testing.T) {
	conform(t, "delete-missing", oneDoc(), func(t *testing.T, port document.Port) {
		err := port.Delete(context.Background(), "absent.tsvt", revisionOf(t, seeded))
		require.Error(t, err)
		assert.ErrorIs(t, err, document.ErrMissing)
	})
}

func TestConformanceCancelledContextIsHonoured(t *testing.T) {
	// The embedded adapter cannot block, so this asserts only that a cancelled
	// context never produces a *wrong answer*: either the read succeeds
	// (nothing to interrupt) or it reports unavailability — never a document.
	conform(t, "cancelled", oneDoc(), func(t *testing.T, port document.Port) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		snap, err := port.Get(ctx, "a.tsvt")
		if err != nil {
			assert.ErrorIs(t, err, document.ErrUnavailable)
			return
		}
		assert.Equal(t, seeded, string(snap.Text))
	})
}

// TestEmbeddedPortBindsNoListener pins the owner's constraint that a frontend
// holding the embedded port needs no server: the store package must not reach
// HTTP at all. Import-level, because a runtime assertion would only prove that
// one code path stayed quiet.
func TestEmbeddedPortBindsNoListener(t *testing.T) {
	for _, pkg := range []string{"../document", "../store"} {
		source := readPackage(t, pkg)
		assert.NotContains(t, source, `"net/http"`, pkg+" must not import net/http")
		assert.NotContains(t, source, `"net"`+"\n", pkg+" must not import net")
	}
}

// readPackage concatenates a package's non-test Go sources.
func readPackage(t *testing.T, pkg string) string {
	t.Helper()
	entries, err := os.ReadDir(pkg)
	require.NoError(t, err)
	var all strings.Builder
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".go") || strings.HasSuffix(entry.Name(), "_test.go") {
			continue
		}
		content, readErr := os.ReadFile(filepath.Join(pkg, entry.Name()))
		require.NoError(t, readErr)
		_, _ = all.Write(content)
	}
	return all.String()
}

// TestConformanceRefusedPathsAreRefusedForRealNotByAbsence pins the fix for a
// defect an adversarial pass reproduced: the previous refused-path check
// passed only because no file happened to sit at the traversal target. With
// one there, an unvalidated client rewrote the path and served it.
func TestConformanceRefusedPathsAreRefusedForRealNotByAbsence(t *testing.T) {
	for _, refused := range []document.DocPath{
		"../escape.tsvt", "./a.tsvt", "/a.tsvt", "x/../a.tsvt", "a!b.tsvt", "..", ".", "",
	} {
		conform(t, "refused/"+string(refused), oneDoc(), func(t *testing.T, port document.Port) {
			_, err := port.Get(context.Background(), refused)
			require.Error(t, err)
			assert.ErrorIs(t, err, document.ErrMissing)
			assert.ErrorIs(t, err, document.ErrPath, "the specific cause travels with the refusal")
		})
	}
}

// TestConformanceRefusedPathsCannotBeWritten pins the P0: a create-only write
// to an aliased path used to destroy the aliased document and report 201.
func TestConformanceRefusedPathsCannotBeWritten(t *testing.T) {
	for _, refused := range []document.DocPath{"./a.tsvt", "../escape.tsvt", "/a.tsvt", "a!b.tsvt"} {
		conform(t, "no-write/"+string(refused), oneDoc(), func(t *testing.T, port document.Port) {
			_, _, err := port.Put(context.Background(), refused, []byte("HIJACKED\n"), document.ExpectAbsent())
			require.Error(t, err)
			assert.ErrorIs(t, err, document.ErrMissing)
			snap, getErr := port.Get(context.Background(), "a.tsvt")
			require.NoError(t, getErr)
			assert.Equal(t, seeded, string(snap.Text), "the aliased document is untouched")
		})
	}
}

func TestConformanceRefusedPathsCannotBeAppliedToOrDeleted(t *testing.T) {
	conform(t, "no-mutate", oneDoc(), func(t *testing.T, port document.Port) {
		_, err := port.Apply(context.Background(), "./a.tsvt", editsOf(t, "setCell\tA1\tx\n"), revisionOf(t, seeded))
		assert.ErrorIs(t, err, document.ErrMissing)
		assert.ErrorIs(t, port.Delete(context.Background(), "./a.tsvt", revisionOf(t, seeded)), document.ErrMissing)
		snap, getErr := port.Get(context.Background(), "a.tsvt")
		require.NoError(t, getErr)
		assert.Equal(t, seeded, string(snap.Text))
	})
}

// TestConformanceZeroPreconditionIsRefused pins that a write with no stated
// expectation is refused rather than performed: an unconditioned write is
// exactly the silent overwrite the plane exists to prevent.
func TestConformanceZeroPreconditionIsRefused(t *testing.T) {
	conform(t, "zero-expect", oneDoc(), func(t *testing.T, port document.Port) {
		_, _, err := port.Put(context.Background(), "a.tsvt", []byte("9\n"), document.Expect{})
		require.Error(t, err)
		assert.ErrorIs(t, err, document.ErrPrecondition)
		snap, getErr := port.Get(context.Background(), "a.tsvt")
		require.NoError(t, getErr)
		assert.Equal(t, seeded, string(snap.Text))
	})
}

// TestConformanceNilBodyIsAnEmptyDocument pins that the natural spelling of an
// empty document behaves the same through both adapters.
func TestConformanceNilBodyIsAnEmptyDocument(t *testing.T) {
	conform(t, "nil-body", oneDoc(), func(t *testing.T, port document.Port) {
		snap, created, err := port.Put(context.Background(), "empty.tsvt", nil, document.ExpectAbsent())
		require.NoError(t, err)
		assert.True(t, bool(created))
		assert.Empty(t, string(snap.Text))
	})
}

// TestConformanceOnlySheetsAreServed pins the extension rule: a port is
// pointed at a directory, and without this every file under it would be
// readable and writable through the same requests — an operator pointing one
// at a project checkout would be serving its dotfiles, not its spreadsheets.
func TestConformanceOnlySheetsAreServed(t *testing.T) {
	fixtures := map[string]string{"a.tsvt": conformance.Seed, ".env": "SECRET=hunter2\n", "notes.txt": "hello\n"}
	for _, refused := range []document.DocPath{".env", "notes.txt", "a.tsv", "a.TSVT", ".tsvt", "a.tsvt.bak"} {
		conform(t, "not-a-sheet/"+string(refused), fixtures, func(t *testing.T, port document.Port) {
			_, err := port.Get(context.Background(), refused)
			require.Error(t, err)
			assert.ErrorIs(t, err, document.ErrMissing)

			_, _, writeErr := port.Put(context.Background(), refused, []byte("PWNED\n"), document.ExpectAbsent())
			require.Error(t, writeErr)
			assert.ErrorIs(t, writeErr, document.ErrMissing)
		})
	}
}

// TestConformanceARefusedWritePathIsMissingNotAServerFault pins that a path
// the filesystem will not accept reads as "not found" on the write side too,
// rather than blaming the server and disclosing that the escaping name exists.
func TestConformanceARefusedWritePathIsMissingNotAServerFault(t *testing.T) {
	conform(t, "refused-write", map[string]string{"a.tsvt": conformance.Seed}, func(t *testing.T, port document.Port) {
		_, _, err := port.Put(context.Background(), "a.tsvt/nested.tsvt", []byte("1\n"), document.ExpectAbsent())
		require.Error(t, err)
		assert.ErrorIs(t, err, document.ErrMissing)
		assert.NotContains(t, err.Error(), "mkdirat", "no syscall text reaches a caller")
	})
}
