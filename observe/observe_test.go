package observe

import (
	"bytes"
	"context"
	"encoding/json"
	"expvar"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	tsvsheet "github.com/tsvsheet/go-tsvsheet"

	"github.com/tsvsheet/tsvsheet.api/api"
	"github.com/tsvsheet/tsvsheet.api/store"
)

// capturedMetrics records every observation for assertion.
type capturedMetrics struct {
	seen []Observation
	mu   sync.Mutex
}

func (c *capturedMetrics) RequestServed(observed Observation) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.seen = append(c.seen, observed)
}

func (c *capturedMetrics) all() []Observation {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]Observation(nil), c.seen...)
}

// seededStore confines a store to a fresh root holding files.
func seededStore(t *testing.T, files map[string]string) *store.Store {
	t.Helper()
	dir := t.TempDir()
	for name, content := range files {
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600))
	}
	st, err := store.Open(store.RootDir(dir), tsvsheet.DefaultLimits())
	require.NoError(t, err)
	return st
}

// observed builds a server whose logs and metrics the test can read.
func observed(t *testing.T, files map[string]string) (*httptest.Server, *bytes.Buffer, *capturedMetrics) {
	t.Helper()
	logs := &bytes.Buffer{}
	metrics := &capturedMetrics{}
	handler := api.NewHandler(api.Config{
		Port:           seededStore(t, files),
		Limits:         tsvsheet.DefaultLimits(),
		ComputeEnabled: api.WithComputePlane,
		Clock:          func() time.Time { return time.Unix(1_700_000_000, 0).UTC() },
	})
	logger := slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	server := httptest.NewServer(Handler(handler, logger, metrics))
	t.Cleanup(server.Close)
	return server, logs, metrics
}

// TestEveryRequestIsLoggedAndMeasured pins R17 at the surface: a served
// request emits one structured record carrying method, path, status, duration
// and size, and one metric observation agreeing with it — and the record
// never carries the document's contents.
func TestEveryRequestIsLoggedAndMeasured(t *testing.T) {
	server, logs, metrics := observed(t, map[string]string{"a.tsvt": "secretvalue\t2\n"})

	resp, err := server.Client().Get(server.URL + "/a.tsvt")
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	require.Equal(t, http.StatusOK, resp.StatusCode)

	var record map[string]any
	require.NoError(t, json.Unmarshal([]byte(strings.TrimSpace(logs.String())), &record))
	assert.Equal(t, "request served", record["msg"])
	assert.Equal(t, "GET", record["method"])
	assert.Equal(t, "/a.tsvt", record["path"])
	assert.InDelta(t, 200, record["status"], 0)
	assert.Contains(t, record, "duration")
	assert.Contains(t, record, "bytes")
	assert.NotContains(t, logs.String(), "secretvalue",
		"a log line must not carry the document it served")

	seen := metrics.all()
	require.Len(t, seen, 1)
	assert.Equal(t, RequestMethod("GET"), seen[0].Method)
	assert.Equal(t, StatusCode(200), seen[0].Status)
	assert.Positive(t, seen[0].Bytes, "the observation carries what was written")
}

// TestRefusalsAreLoggedAtAReadableLevel pins the grading: a refused request is
// a warning a reader should see, a server fault would be an error, and a
// routine answer stays info — so a log at warn+ is exactly the set worth
// alerting on.
func TestRefusalsAreLoggedAtAReadableLevel(t *testing.T) {
	server, logs, metrics := observed(t, map[string]string{"a.tsvt": "1\n"})

	resp, err := server.Client().Get(server.URL + "/missing.tsvt")
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	require.Equal(t, http.StatusNotFound, resp.StatusCode)

	var record map[string]any
	require.NoError(t, json.Unmarshal([]byte(strings.TrimSpace(logs.String())), &record))
	assert.Equal(t, "WARN", record["level"], "a refusal is worth reading")
	assert.InDelta(t, 404, record["status"], 0)
	require.Len(t, metrics.all(), 1)
	assert.Equal(t, StatusCode(404), metrics.all()[0].Status)
}

// flushCounter is a ResponseWriter that counts flushes.
type flushCounter struct {
	http.ResponseWriter
	flushes int
}

func (f *flushCounter) Flush() { f.flushes++ }

// TestRecorderForwardsFlush pins the wrapper's obligation directly: the event
// feed type-asserts http.Flusher and falls back to a no-op when the assertion
// fails, so a recorder without Flush would stop SSE from streaming with no
// error raised anywhere.
func TestRecorderForwardsFlush(t *testing.T) {
	t.Parallel()

	inner := &flushCounter{ResponseWriter: httptest.NewRecorder()}
	rec := newRecorder(inner)

	var asFlusher http.Flusher = rec
	asFlusher.Flush()
	assert.Equal(t, 1, inner.flushes, "the flush must reach the wrapped writer")
	assert.Same(t, inner, rec.Unwrap(), "ResponseController can still reach through")
}

// TestFeedStreamsThroughTheRecorder is the end-to-end half: with the stream
// open, an edit made by another client must arrive on it. Unflushed, those
// bytes would sit in the response buffer until the handler returned — which,
// for a feed, is never.
func TestFeedStreamsThroughTheRecorder(t *testing.T) {
	server, _, _ := observed(t, map[string]string{"a.tsvt": "1\n"})

	head, err := server.Client().Get(server.URL + "/a.tsvt")
	require.NoError(t, err)
	rev := strings.Trim(head.Header.Get("ETag"), `"`)
	require.NoError(t, head.Body.Close())
	require.NotEmpty(t, rev)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	sub, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL+"/a.tsvt", nil)
	require.NoError(t, err)
	sub.Header.Set("Accept", "text/event-stream")
	stream, err := server.Client().Do(sub)
	require.NoError(t, err)
	defer func() { _ = stream.Body.Close() }()
	require.Equal(t, http.StatusOK, stream.StatusCode)

	arrived := make(chan int, 1)
	go func() {
		buf := make([]byte, 64)
		n, _ := stream.Body.Read(buf)
		arrived <- n
	}()

	edit, err := http.NewRequestWithContext(ctx, http.MethodPut, server.URL+"/a.tsvt", strings.NewReader("2\n"))
	require.NoError(t, err)
	edit.Header.Set("If-Match", `"`+rev+`"`)
	edit.Header.Set("Content-Type", "application/vnd.tsvsheet.doc+tsv")
	put, err := server.Client().Do(edit)
	require.NoError(t, err)
	require.NoError(t, put.Body.Close())
	require.Equal(t, http.StatusOK, put.StatusCode)

	select {
	case n := <-arrived:
		assert.Positive(t, n, "the change event must reach the subscriber through the recorder")
	case <-time.After(5 * time.Second):
		t.Fatal("nothing streamed through the recorder: the wrapper broke SSE")
	}
}

// TestObservabilityIsOptional pins the nil-safety the decorator promises: a
// wrap with neither destination serves exactly as the bare handler does.
func TestObservabilityIsOptional(t *testing.T) {
	handler := api.NewHandler(api.Config{
		Port:           seededStore(t, map[string]string{"a.tsvt": "1\n"}),
		Limits:         tsvsheet.DefaultLimits(),
		ComputeEnabled: api.WithComputePlane,
		Clock:          func() time.Time { return time.Unix(1_700_000_000, 0).UTC() },
	})
	server := httptest.NewServer(Handler(handler, nil, nil))
	t.Cleanup(server.Close)

	resp, err := server.Client().Get(server.URL + "/a.tsvt")
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	assert.Equal(t, http.StatusOK, resp.StatusCode)
}

// TestExpvarMetricsCountAndTime pins the zero-dependency default: counts and
// summed latency land under the given prefix, keyed by method and status, and
// a repeated prefix adopts the published maps rather than panicking — two
// servers in one process is an ordinary thing to want.
func TestExpvarMetricsCountAndTime(t *testing.T) {
	m := NewExpvarMetrics("test_alpha")
	m.RequestServed(Observation{Method: "GET", Status: 200, Duration: 7 * time.Millisecond})
	m.RequestServed(Observation{Method: "GET", Status: 200, Duration: 3 * time.Millisecond})
	m.RequestServed(Observation{Method: "PUT", Status: 412})

	requests, ok := expvar.Get("test_alpha_requests").(*expvar.Map)
	require.True(t, ok)
	assert.Equal(t, "2", requests.Get("GET 200").String())
	assert.Equal(t, "1", requests.Get("PUT 412").String())

	durations, ok := expvar.Get("test_alpha_duration_ms").(*expvar.Map)
	require.True(t, ok)
	assert.Equal(t, "10", durations.Get("GET 200").String(), "latency sums")

	again := NewExpvarMetrics("test_alpha")
	again.RequestServed(Observation{Method: "GET", Status: 200})
	assert.Equal(t, "3", requests.Get("GET 200").String(), "a repeated prefix adopts, never panics")
}

// TestLevelGradesEveryClass pins the grading table whole — a server fault is
// an error, a refusal a warning, anything else routine — because the level is
// what decides whether an operator ever sees the line.
func TestLevelGradesEveryClass(t *testing.T) {
	t.Parallel()

	for status, want := range map[StatusCode]slog.Level{
		200: slog.LevelInfo,
		204: slog.LevelInfo,
		304: slog.LevelInfo,
		400: slog.LevelWarn,
		404: slog.LevelWarn,
		412: slog.LevelWarn,
		500: slog.LevelError,
		503: slog.LevelError,
	} {
		assert.Equal(t, want, levelFor(status), "status %d", status)
	}
}

// TestConcurrentMetricsUnderOnePrefix pins the race the CLI's parallel tests
// found: constructing two servers under the same prefix at the same time must
// adopt one published map, not panic on the duplicate. Two servers in one
// process is ordinary, and a Get followed by a NewMap is two operations.
func TestConcurrentMetricsUnderOnePrefix(t *testing.T) {
	t.Parallel()

	const racers = 16
	ready := make(chan struct{})
	done := make(chan ExpvarMetrics, racers)
	for range racers {
		go func() {
			<-ready // release them together, so the Get/NewMap window overlaps
			done <- NewExpvarMetrics("test_race")
		}()
	}
	close(ready)

	for range racers {
		m := <-done
		m.RequestServed(Observation{Method: "GET", Status: 200})
	}
	counted, ok := expvar.Get("test_race_requests").(*expvar.Map)
	require.True(t, ok)
	assert.Equal(t, strconv.Itoa(racers), counted.Get("GET 200").String(),
		"every racer counted into the one published map")
}
