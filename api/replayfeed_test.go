package api

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFeedReplaysFromLastEventID(t *testing.T) {
	h := newHandler(t, map[string]string{"a.tsvt": "1\n"}, DocumentPlaneOnly)
	server := httptest.NewServer(h)
	defer server.Close()

	first := openSSE(t, server.URL, "")
	post := func(ifMatch, cell string) {
		req, err := http.NewRequest(http.MethodPost, server.URL+"/a.tsvt", strings.NewReader("setCell\tA1\t"+cell+"\n"))
		require.NoError(t, err)
		req.Header.Set("Content-Type", TypeEdits)
		req.Header.Set("If-Match", ifMatch)
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		require.Equal(t, http.StatusNoContent, resp.StatusCode)
		_ = resp.Body.Close()
	}
	post(etag(t, "1\n"), "2")
	one := first.next(t)
	require.Equal(t, "changed", one["event"])
	firstID := one["id"]
	post(etag(t, "2\n"), "3")
	_ = first.next(t)
	first.close()

	// Reconnecting after the first event replays the second.
	second := openSSE(t, server.URL, firstID)
	defer second.close()
	replayed := second.next(t)
	assert.Equal(t, "changed", replayed["event"])
	assert.Contains(t, replayed["data"], "setCell\tA1\t3")
}

func TestFeedResetWhenHorizonGone(t *testing.T) {
	h := newHandler(t, map[string]string{"a.tsvt": "1\n"}, DocumentPlaneOnly)
	server := httptest.NewServer(h)
	defer server.Close()
	// An ID far below the retained window (which is empty) forces a reset.
	session := openSSE(t, server.URL, "999")
	defer session.close()
	event := session.next(t)
	assert.Equal(t, "reset", event["event"])
	assert.Contains(t, event["data"], "#.rev\t"+revision(t, "1\n"))
}

func TestPutBroadcastsReset(t *testing.T) {
	h := newHandler(t, map[string]string{"a.tsvt": "1\n"}, DocumentPlaneOnly)
	server := httptest.NewServer(h)
	defer server.Close()
	session := openSSE(t, server.URL, "")
	defer session.close()

	req, err := http.NewRequest(http.MethodPut, server.URL+"/a.tsvt", strings.NewReader("9\n"))
	require.NoError(t, err)
	req.Header.Set("Content-Type", TypeDoc)
	req.Header.Set("If-Match", etag(t, "1\n"))
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	_ = resp.Body.Close()

	event := session.next(t)
	assert.Equal(t, "reset", event["event"])
	assert.Contains(t, event["data"], "#.rev\t"+revision(t, "9\n"))
}

func TestDeleteBroadcastsDeletedAndEndsStream(t *testing.T) {
	h := newHandler(t, map[string]string{"a.tsvt": "1\n"}, DocumentPlaneOnly)
	server := httptest.NewServer(h)
	defer server.Close()
	session := openSSE(t, server.URL, "")
	defer session.close()

	req, err := http.NewRequest(http.MethodDelete, server.URL+"/a.tsvt", nil)
	require.NoError(t, err)
	req.Header.Set("If-Match", etag(t, "1\n"))
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	require.Equal(t, http.StatusNoContent, resp.StatusCode)
	_ = resp.Body.Close()

	event := session.next(t)
	assert.Equal(t, "deleted", event["event"])
	rest, err := io.ReadAll(session.resp.Body)
	require.NoError(t, err)
	assert.Empty(t, string(rest))
}

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

func TestHubRetainsTheNewestEvent(t *testing.T) {
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

func TestWriteEventOmitsZeroID(t *testing.T) {
	rec := httptest.NewRecorder()
	writeEvent(rec, feedEvent{name: eventReset, data: "x"})
	assert.False(t, strings.Contains(rec.Body.String(), "id:"))
}
