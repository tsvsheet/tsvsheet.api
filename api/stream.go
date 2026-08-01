// Server-Sent Events framing: turning one journal event into one SSE frame,
// and forwarding a live subscription until it ends.
package api

import (
	"fmt"
	"net/http"
	"strconv"

	"github.com/tsvsheet/tsvsheet.api/document"
)

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
