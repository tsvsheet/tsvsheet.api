// White-box tests for the feed hub's saturation behaviour, the SSE plumbing's
// degenerate writers, and the replay cache's bound — edges a black-box HTTP
// client cannot reach deterministically.
package api

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	tsvsheet "github.com/tsvsheet/go-tsvsheet"
)

func TestReplayCacheResetsWhenFull(t *testing.T) {
	handler := NewHandler(Config{})
	for i := range replayCap {
		handler.replay.seen["doc\x00k"+strconv.Itoa(i)] = replayOutcome{rev: "r"}
	}
	req := httptest.NewRequest(http.MethodPost, "/doc", nil)
	req.Header.Set("Idempotency-Key", "fresh")
	handler.remember(req, "doc", tsvsheet.RevisionHex("new"), []byte("body"))
	assert.Len(t, handler.replay.seen, 1)
}

func TestRememberWithoutKeyIsNoOp(t *testing.T) {
	handler := NewHandler(Config{})
	handler.remember(httptest.NewRequest(http.MethodPost, "/doc", nil), "doc", "r", []byte("body"))
	assert.Empty(t, handler.replay.seen)
}
