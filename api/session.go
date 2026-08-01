// A subscriber's session over the change feed, as the handler sees it: the
// replay it is owed on connect and the live events that follow.
package api

import "github.com/tsvsheet/tsvsheet.api/document"

// subscription is one subscriber's view: events to replay first, the live
// channel, whether the requested horizon was already gone (reset), and the
// release to call when done.
type subscription struct {
	live    chan feedEvent
	cancel  func()
	replay  []feedEvent
	isReset bool
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
