package api

import (
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

// subCap bounds a subscriber's buffer; a subscriber that does not drain is
// dropped (its channel closed) and recovers by reconnecting with replay —
// TestHubDropsSaturatedSubscriber asserts the drop and that later broadcasts
// survive it.
const subCap = 16

// docFeed is one document's live feed state.
type docFeed struct {
	subs  map[chan feedEvent]struct{}
	ring  []feedEvent
	seq   int64
	bytes journalBytes
}

// trimJournal returns the journal reduced to within both bounds, with the
// payload total it now carries. The newest event is retained however large, so
// a live subscriber still sees the change that just happened. See
// TestHubRetainsTheNewestEvent and TestHubJournalIsBoundedByBytes.
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
