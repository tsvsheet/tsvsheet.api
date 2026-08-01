package api

import (
	"fmt"
	"net/http"
	"strconv"
	"sync"

	"github.com/tsvsheet/tsvsheet.api/document"
)

// feedEvent is one change-feed message: a journal sequence, an event name,
// and its payload (an edits document or a cells delta — the same TSV formats
// everything else uses).
type feedEvent struct {
	name string
	data string
	id   int64
}

// ringCap bounds how many events a document's in-memory journal retains for
// Last-Event-ID replay; a reconnect beyond the window is answered with a
// reset event instead.
const ringCap = 256

// ringBytes bounds the same journal by payload size. A count alone is not a
// bound: an event carries the request body it came from, so 256 batches at the
// body limit would pin a gigabyte per document and hand all of it to the next
// subscriber. The oldest events are dropped until the retained payload fits,
// and a client whose resume point went with them gets a reset.
const ringBytes journalBytes = 1 << 20

// journalBytes is a retained-payload total.
type journalBytes int

// subCap bounds a subscriber's buffer; a subscriber that cannot drain is
// dropped (its channel closed) and recovers by reconnecting with replay.
const subCap = 16

// docFeed is one document's live feed state.
type docFeed struct {
	subs  map[chan feedEvent]struct{}
	ring  []feedEvent
	seq   int64
	bytes journalBytes
}

// trimJournal returns the journal reduced to within both bounds, with the
// payload total it now carries. The newest event is always retained, however
// large, so a live subscriber still sees the change that just happened.
func trimJournal(ring []feedEvent, bytes journalBytes) ([]feedEvent, journalBytes) {
	for len(ring) > 1 && (len(ring) > ringCap || bytes > ringBytes) {
		bytes -= journalBytes(len(ring[0].data))
		ring = ring[1:]
	}
	return ring, bytes
}

// hub fans applied changes out to event-stream subscribers, retaining a
// bounded per-document replay window.
//
// Pointer receivers are necessary: the hub owns a mutex and shared maps that
// may not be copied.
type hub struct {
	docs map[document.DocPath]*docFeed
	mu   sync.Mutex
}

// newHub builds an empty hub.
func newHub() *hub { return &hub{docs: map[document.DocPath]*docFeed{}} }

// feedOf returns doc's feed state, creating it, under the held lock.
func (h *hub) feedOf(doc document.DocPath) *docFeed {
	feed, ok := h.docs[doc]
	if !ok {
		feed = &docFeed{subs: map[chan feedEvent]struct{}{}}
		h.docs[doc] = feed
	}
	return feed
}

// broadcast appends one event to doc's journal and fans it out. A subscriber
// whose buffer is full is closed and dropped — it reconnects with replay.
func (h *hub) broadcast(doc document.DocPath, name, data string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	feed := h.feedOf(doc)
	feed.seq++
	event := feedEvent{id: feed.seq, name: name, data: data}
	feed.ring, feed.bytes = trimJournal(append(feed.ring, event), feed.bytes+journalBytes(len(event.data)))
	for ch := range feed.subs {
		select {
		case ch <- event:
		default:
			close(ch)
			delete(feed.subs, ch)
		}
	}
}

// subscription is one subscriber's view: events to replay first, the live
// channel, whether the requested horizon was already gone (reset), and the
// release to call when done.
type subscription struct {
	live    chan feedEvent
	cancel  func()
	replay  []feedEvent
	isReset bool
}

// subscribe registers a subscriber resuming after lastID (0 = live only).
func (h *hub) subscribe(doc document.DocPath, lastID int64) subscription {
	h.mu.Lock()
	defer h.mu.Unlock()
	feed := h.feedOf(doc)
	ch := make(chan feedEvent, subCap)
	feed.subs[ch] = struct{}{}
	cancel := func() { h.unsubscribe(doc, ch) }
	// No resume point means a fresh subscriber: it has just fetched the
	// document, so replaying the backlog would hand it a history it already
	// has, at the journal's full retained size.
	if lastID == 0 {
		return subscription{live: ch, cancel: cancel}
	}
	// A resume point below the retained window or beyond the current sequence
	// (a restarted server) cannot be replayed — the client must refetch.
	floor := feed.seq - int64(len(feed.ring))
	if lastID < floor || lastID > feed.seq {
		return subscription{live: ch, cancel: cancel, isReset: true}
	}
	var replay []feedEvent
	for _, event := range feed.ring {
		if event.id > lastID {
			replay = append(replay, event)
		}
	}
	return subscription{replay: replay, live: ch, cancel: cancel}
}

// unsubscribe removes a subscriber if still registered.
func (h *hub) unsubscribe(doc document.DocPath, ch chan feedEvent) {
	h.mu.Lock()
	defer h.mu.Unlock()
	feed := h.feedOf(doc)
	if _, ok := feed.subs[ch]; ok {
		close(ch)
		delete(feed.subs, ch)
	}
}

// serveFeed streams doc's events as Server-Sent Events until the client goes
// away or the document is deleted.
func (handler Handler) serveFeed(w http.ResponseWriter, r *http.Request, doc document.DocPath, headRev string) {
	sub := handler.hub.subscribe(doc, lastEventID(r))
	defer sub.cancel()
	w.Header().Set("Content-Type", TypeStream)
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	flush := flusherOf(w)
	flush()
	if sub.isReset {
		writeEvent(w, feedEvent{name: eventReset, data: metaRev + "\t" + headRev})
		flush()
	}
	for _, event := range sub.replay {
		writeEvent(w, event)
		flush()
	}
	streamLive(w, r, sub.live, flush)
}

// streamLive forwards live events until the subscription closes or the
// client disconnects. A deleted event ends the stream.
func streamLive(w http.ResponseWriter, r *http.Request, live <-chan feedEvent, flush func()) {
	for {
		select {
		case <-r.Context().Done():
			return
		case event, isOpen := <-live:
			if !forwardEvent(w, event, streamOpen(isOpen), flush) {
				return
			}
		}
	}
}

// streamOpen reports whether the live channel was still open at receipt.
type streamOpen bool

// forwardEvent writes one live event, reporting whether the stream continues:
// a closed channel or a deleted document ends it.
func forwardEvent(w http.ResponseWriter, event feedEvent, isOpen streamOpen, flush func()) bool {
	if !isOpen {
		return false
	}
	writeEvent(w, event)
	flush()
	return event.name != eventDeleted
}

// writeEvent emits one SSE frame; multi-line payloads become repeated data
// lines, as the SSE format defines.
func writeEvent(w http.ResponseWriter, event feedEvent) {
	if event.id > 0 {
		_, _ = fmt.Fprintf(w, "id: %d\n", event.id)
	}
	_, _ = fmt.Fprintf(w, "event: %s\n", event.name)
	for _, line := range splitDataLines(feedData(event.data)) {
		_, _ = fmt.Fprintf(w, "data: %s\n", line)
	}
	_, _ = fmt.Fprint(w, "\n")
}

// feedData is one event payload.
type feedData string

// splitDataLines splits a payload for SSE data framing, dropping a trailing
// newline's empty tail.
func splitDataLines(data feedData) []string {
	lines := []string{}
	start := 0
	for i := 0; i < len(data); i++ {
		if data[i] == '\n' {
			lines = append(lines, string(data[start:i]))
			start = i + 1
		}
	}
	if start < len(data) {
		lines = append(lines, string(data[start:]))
	}
	return lines
}

// flusherOf returns the writer's flush, or a no-op when the writer cannot
// stream.
func flusherOf(w http.ResponseWriter) func() {
	if f, ok := w.(http.Flusher); ok {
		return f.Flush
	}
	return func() {}
}

// lastEventID reads the SSE resume header (0 when absent or malformed).
func lastEventID(r *http.Request) int64 {
	id, err := strconv.ParseInt(r.Header.Get("Last-Event-ID"), 10, 64)
	if err != nil || id < 0 {
		return 0
	}
	return id
}

// The feed's event names and metadata keys.
const (
	eventChanged  = "changed"
	eventComputed = "computed"
	eventReset    = "reset"
	eventDeleted  = "deleted"
	metaBase      = "#.base"
	metaRev       = "#.rev"
)
