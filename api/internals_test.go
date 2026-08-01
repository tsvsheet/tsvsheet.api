// White-box tests for the feed hub's saturation behaviour, the SSE plumbing's
// degenerate writers, and the replay cache's bound — edges a black-box HTTP
// client cannot reach deterministically.
package api

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	tsvsheet "github.com/tsvsheet/go-tsvsheet"
)

func TestHubRingOverflowForcesReset(t *testing.T) {
	h := newHub()
	for i := range ringCap + 10 {
		h.broadcast("doc", eventChanged, "n"+strconv.Itoa(i))
	}
	// A resume point below the retained window resets.
	sub := h.subscribe("doc", 1)
	defer sub.cancel()
	assert.True(t, sub.isReset)
	// A resume point inside the window replays exactly the tail.
	tail := h.subscribe("doc", int64(ringCap+5))
	defer tail.cancel()
	assert.False(t, tail.isReset)
	assert.Len(t, tail.replay, 5)
}

// TestHubFreshSubscriberGetsNoBacklog pins that a subscriber with no resume
// point receives only what happens next: it has just fetched the document, so
// replaying the retained journal would hand it history it already has.
func TestHubFreshSubscriberGetsNoBacklog(t *testing.T) {
	h := newHub()
	for i := range 5 {
		h.broadcast("doc", eventChanged, "n"+strconv.Itoa(i))
	}
	sub := h.subscribe("doc", 0)
	defer sub.cancel()
	assert.Empty(t, sub.replay)
	assert.False(t, sub.isReset)
}

// TestHubJournalIsBoundedByBytes pins the size bound: an event carries the
// request body it came from, so a count-only bound would let a few large
// batches pin the journal and flood the next subscriber.
func TestHubJournalIsBoundedByBytes(t *testing.T) {
	h := newHub()
	big := strings.Repeat("x", int(ringBytes)/2)
	for range 10 {
		h.broadcast("doc", eventChanged, big)
	}
	h.mu.Lock()
	retained, events := h.docs["doc"].bytes, len(h.docs["doc"].ring)
	h.mu.Unlock()
	assert.LessOrEqual(t, int(retained), int(ringBytes), "the journal stays within its byte bound")
	assert.Less(t, events, 10, "the oldest events were dropped")
}

func TestHubAlwaysRetainsTheNewestEvent(t *testing.T) {
	h := newHub()
	h.broadcast("doc", eventChanged, strings.Repeat("x", int(ringBytes)*2))
	h.mu.Lock()
	events := len(h.docs["doc"].ring)
	h.mu.Unlock()
	assert.Equal(t, 1, events, "an oversize event is still the one a live subscriber needs")
}

func TestHubResumeBeyondSequenceForcesReset(t *testing.T) {
	h := newHub()
	h.broadcast("doc", eventChanged, "x")
	sub := h.subscribe("doc", 99)
	defer sub.cancel()
	assert.True(t, sub.isReset)
}

func TestHubDropsSaturatedSubscriber(t *testing.T) {
	h := newHub()
	sub := h.subscribe("doc", 0)
	for i := range subCap + 1 {
		h.broadcast("doc", eventChanged, "n"+strconv.Itoa(i))
	}
	// The channel was closed on saturation: draining ends with a closed read.
	seen := 0
	for range sub.live {
		seen++
	}
	assert.Equal(t, subCap, seen)
	// A cancel after the drop is a no-op, and later broadcasts don't panic.
	sub.cancel()
	h.broadcast("doc", eventChanged, "after")
}

// plainWriter is a ResponseWriter with no Flush support.
type plainWriter struct{}

func (plainWriter) Header() http.Header         { return http.Header{} }
func (plainWriter) Write(b []byte) (int, error) { return len(b), nil }
func (plainWriter) WriteHeader(int)             {}

func TestFlusherOfPlainWriterIsNoOp(t *testing.T) {
	t.Parallel()
	flusherOf(plainWriter{})()
}

func TestStreamLiveEndsOnClosedChannel(t *testing.T) {
	t.Parallel()
	ch := make(chan feedEvent)
	close(ch)
	streamLive(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/x", nil), ch, func() {})
}

func TestSplitDataLinesKeepsUnterminatedTail(t *testing.T) {
	assert.Equal(t, []string{"a", "b"}, splitDataLines(feedData("a\nb")))
	assert.Equal(t, []string{"a", "b"}, splitDataLines(feedData("a\nb\n")))
	assert.Empty(t, splitDataLines(feedData("")))
}

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

// failingBody errors on read without being a MaxBytesError.
type failingBody struct{}

func (failingBody) Read([]byte) (int, error) { return 0, errors.New("torn") }
func (failingBody) Close() error             { return nil }

func TestReadBodyPlainFailureIs400(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/doc", nil)
	req.Body = failingBody{}
	rec := httptest.NewRecorder()
	_, ok := readBody(rec, req)
	require.False(t, ok)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "unreadable")
}

func TestParseAcceptDropsEmptyExpressions(t *testing.T) {
	set := parseAccept(" , text/csv;q=0.8 ,")
	assert.Equal(t, acceptSet{"text/csv"}, set)
	assert.True(t, set.has("text/csv"))
	assert.False(t, set.wildcard())
}

func TestCellsDeltaCoversShrinkAndGrowth(t *testing.T) {
	old := tsvsheet.Grid{{"1", "2"}, {"3"}}
	next := tsvsheet.Grid{{"1", "9", "8"}}
	delta := cellsDelta(old, next)
	assert.Contains(t, delta, "B1\t9")
	assert.Contains(t, delta, "C1\t8")
	assert.Contains(t, delta, "A2\t\n")
	assert.NotContains(t, delta, "A1")
}

func TestCellsDeltaEqualGridsIsEmpty(t *testing.T) {
	grid := tsvsheet.Grid{{"1"}}
	assert.Empty(t, cellsDelta(grid, grid))
}

func TestWriteEventOmitsZeroID(t *testing.T) {
	rec := httptest.NewRecorder()
	writeEvent(rec, feedEvent{name: eventReset, data: "x"})
	assert.False(t, strings.Contains(rec.Body.String(), "id:"))
}
