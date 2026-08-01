package document_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	tsvsheet "github.com/tsvsheet/go-tsvsheet"

	"github.com/tsvsheet/tsvsheet.api/document"
)

// TestValidateRefusesEveryAliasAsMissing pins the plane's path rule and the
// sentinels it emits: a refusal reads as "no such document" to every caller,
// with the specific cause available to one that wants it.
func TestValidateRefusesEveryAliasAsMissing(t *testing.T) {
	t.Parallel()
	for _, refused := range []document.DocPath{
		"", ".", "..", "./a.tsvt", "../a.tsvt", "/a.tsvt", "x/../a.tsvt", "a//b.tsvt", "a!b.tsvt",
	} {
		err := document.Validate(refused)
		require.Error(t, err, refused)
		assert.ErrorIs(t, err, document.ErrMissing, refused)
		assert.ErrorIs(t, err, document.ErrPath, refused)
	}
}

func TestValidateAdmitsOrdinaryPaths(t *testing.T) {
	t.Parallel()
	for _, ok := range []document.DocPath{"a.tsvt", "lib/a.tsvt", "a b.tsvt", "modèle.tsvt", "a.b.tsvt"} {
		assert.NoError(t, document.Validate(ok), ok)
	}
}

// TestExpectStatesOneRequirement pins the precondition vocabulary: a
// value built by neither constructor states no expectation at all, which no
// mutation may act on.
func TestExpectStatesOneRequirement(t *testing.T) {
	t.Parallel()
	rev, isAbsent := document.ExpectRev("abc").Revision()
	assert.Equal(t, tsvsheet.RevisionHex("abc"), rev)
	assert.False(t, bool(isAbsent))
	assert.False(t, document.ExpectRev("abc").IsZero())

	rev, isAbsent = document.ExpectAbsent().Revision()
	assert.Empty(t, string(rev))
	assert.True(t, bool(isAbsent))
	assert.False(t, document.ExpectAbsent().IsZero())

	assert.True(t, document.Expect{}.IsZero(), "a value built by neither constructor expects nothing")
}

// TestSentinelsAreDistinct pins that the plane's errors do not collide: two
// conditions sharing an error would make a caller unable to tell a conflict
// from a missing document.
func TestSentinelsAreDistinct(t *testing.T) {
	t.Parallel()
	all := []error{
		document.ErrExists, document.ErrMissing, document.ErrPath,
		document.ErrPrecondition, document.ErrSyntax, document.ErrUnavailable,
	}
	for i, a := range all {
		for j, b := range all {
			if i == j {
				continue
			}
			assert.NotErrorIs(t, a, b, "%v and %v must be distinguishable", a, b)
		}
	}
}
