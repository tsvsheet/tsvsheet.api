// White-box tests for the feed hub's saturation behaviour, the SSE plumbing's
// degenerate writers, and the replay cache's bound — edges a black-box HTTP
// client cannot reach deterministically.
package api

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestParseAcceptDropsEmptyExpressions(t *testing.T) {
	set := parseAccept(" , text/csv;q=0.8 ,")
	assert.Equal(t, acceptSet{"text/csv"}, set)
	assert.True(t, set.has("text/csv"))
	assert.False(t, set.wildcard())
}
