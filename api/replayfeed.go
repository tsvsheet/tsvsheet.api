// Replay over the change feed: what a reconnecting subscriber is owed from the
// journal, and what it is told when its resume point is gone.
package api

import "github.com/tsvsheet/tsvsheet.api/document"

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
