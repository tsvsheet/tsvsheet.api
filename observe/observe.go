// Package observe is the ecosystem's request observability: one structured
// record and one metric observation per served request, applied as a decorator
// so every serving surface gets the same fields, the same level grading and
// the same counters rather than each one inventing its own (R17, R18).
//
// It is a decorator rather than a handler feature because the obligation to
// log belongs to the *server* — the composition root that binds a port — while
// the handler's job is to route. The destination is always injected: this
// package owns the observations, never where they go, so a consumer picks
// slog's handler and any metrics backend without this package depending on
// either.
package observe

import (
	"expvar"
	"log/slog"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// RequestMethod is an HTTP method as observed.
type RequestMethod string

// StatusCode is the HTTP status a request was answered with.
type StatusCode int

// ByteCount is a response body size in bytes.
type ByteCount int64

// Observation is what one served request contributes to metrics: the
// request's shape and its cost, and nothing drawn from the document itself —
// an observability surface that carried sheet data would be an exfiltration
// surface. TestEveryRequestIsLoggedAndMeasured states that by serving a
// document whose contents it then looks for, and fails on finding.
type Observation struct {
	Method   RequestMethod
	Status   StatusCode
	Duration time.Duration
	Bytes    ByteCount
}

// Metrics receives one Observation per served request. Implementations are
// called from every request goroutine and own their synchronisation; a nil
// Metrics collects nothing.
type Metrics interface {
	RequestServed(observed Observation)
}

// recorder counts what a handler wrote, so the request log and the metrics can
// report a status and a size without any handler knowing they exist.
//
// It forwards Flush deliberately: the event feed type-asserts its writer for
// http.Flusher and simply stops flushing when the assertion fails, so a
// wrapper omitting the method would break SSE streaming with no error raised
// anywhere. TestRecorderForwardsFlushSoTheFeedKeepsStreaming states that.
type recorder struct {
	http.ResponseWriter
	// The counts are held behind a pointer rather than written through a
	// pointer receiver: a ResponseWriter is passed by value into the wrapped
	// handler, so the sharing has to be explicit in the type for the
	// decorator to read back what the handler wrote.
	recorded *counts
}

// counts is the mutable half of a recorder — what the wrapped handler wrote.
type counts struct {
	status StatusCode
	bytes  ByteCount
}

// newRecorder wraps w, defaulting the status to 200 for a handler that writes
// a body without ever calling WriteHeader.
func newRecorder(w http.ResponseWriter) recorder {
	return recorder{ResponseWriter: w, recorded: &counts{status: http.StatusOK}}
}

// WriteHeader records the status on its way through.
func (rec recorder) WriteHeader(status int) {
	rec.recorded.status = StatusCode(status)
	rec.ResponseWriter.WriteHeader(status)
}

// Write records the bytes on their way through.
func (rec recorder) Write(p []byte) (int, error) {
	n, err := rec.ResponseWriter.Write(p)
	rec.recorded.bytes += ByteCount(n)
	return n, err
}

// Flush forwards to the wrapped writer when it can flush, keeping the event
// feed streaming through the wrapper.
func (rec recorder) Flush() {
	if flusher, ok := rec.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

// Unwrap exposes the wrapped writer to http.ResponseController.
func (rec recorder) Unwrap() http.ResponseWriter { return rec.ResponseWriter }

// Handler wraps inner so every request it serves emits one structured record
// and one metric observation. A nil logger or nil metrics disables that half;
// wrapping with neither is a no-op decorator, which keeps the seam usable in
// tests without pretending to observe.
func Handler(inner http.Handler, logger *slog.Logger, metrics Metrics) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := newRecorder(w)
		started := time.Now()
		inner.ServeHTTP(rec, r)
		observe(rec, r, time.Since(started), logger, metrics)
	})
}

// observe emits the request record and the metric observation for one served
// request.
func observe(rec recorder, r *http.Request, took time.Duration, logger *slog.Logger, metrics Metrics) {
	observed := Observation{
		Method:   RequestMethod(r.Method),
		Status:   rec.recorded.status,
		Duration: took,
		Bytes:    rec.recorded.bytes,
	}
	if metrics != nil {
		metrics.RequestServed(observed)
	}
	if logger == nil {
		return
	}
	logger.LogAttrs(r.Context(), levelFor(rec.recorded.status), "request served",
		slog.String("method", r.Method),
		slog.String("path", r.URL.Path),
		slog.Int("status", int(rec.recorded.status)),
		slog.Duration("duration", took),
		slog.Int64("bytes", int64(rec.recorded.bytes)),
	)
}

// levelFor grades a response: a server fault is an error, a refused request is
// a warning worth reading, everything else is routine.
func levelFor(status StatusCode) slog.Level {
	switch {
	case status >= http.StatusInternalServerError:
		return slog.LevelError
	case status >= http.StatusBadRequest:
		return slog.LevelWarn
	default:
		return slog.LevelInfo
	}
}

// MetricsPrefix names an ExpvarMetrics' published variables, so two servers in
// one process do not share counters.
type MetricsPrefix string

// ExpvarMetrics is the zero-dependency Metrics: request counts and total
// latency per method and status, published through the standard library's
// expvar registry and readable wherever the process serves /debug/vars.
//
// It is the default rather than a Prometheus client because this package is a
// library on a public module path: an interface plus a stdlib implementation
// gives a real, queryable answer today and leaves any backend a consumer
// prefers a twenty-line adapter away, with nothing added to go.sum.
type ExpvarMetrics struct {
	requests   *expvar.Map
	durationMS *expvar.Map
}

// NewExpvarMetrics publishes (or adopts, when a prefix repeats) the two maps.
// The value is copyable: both fields are already pointers into the expvar
// registry, so every copy counts into the same maps.
func NewExpvarMetrics(prefix MetricsPrefix) ExpvarMetrics {
	return ExpvarMetrics{
		requests:   publishedMap(varName(prefix) + "_requests"),
		durationMS: publishedMap(varName(prefix) + "_duration_ms"),
	}
}

// varName is a published expvar variable's name.
type varName string

// registry serialises get-or-create against the expvar registry. expvar's own
// state is mutex-protected, but a Get followed by a NewMap is two operations:
// two servers constructed concurrently under one prefix both saw no map and
// both published, and the loser panicked. Two `tsv data` servers in one
// process is an ordinary thing to want, and it is how the CLI's parallel
// tests found this.
var registry sync.Mutex

// publishedMap returns the named expvar map, adopting one already published
// under that name — expvar panics on a duplicate, and a second server or a
// second test in one process is an ordinary thing to want.
func publishedMap(name varName) *expvar.Map {
	registry.Lock()
	defer registry.Unlock()
	if published, ok := expvar.Get(string(name)).(*expvar.Map); ok {
		return published
	}
	return expvar.NewMap(string(name))
}

// RequestServed counts the request and adds its latency, keyed by method and
// status so a reader can see both the shape of the traffic and where the time
// goes.
func (m ExpvarMetrics) RequestServed(observed Observation) {
	key := string(observed.Method) + " " + strconv.Itoa(int(observed.Status))
	m.requests.Add(key, 1)
	m.durationMS.Add(key, observed.Duration.Milliseconds())
}
