package api

import (
	"bufio"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// sseSession is one live event-stream connection.
type sseSession struct {
	resp    *http.Response
	scanner *bufio.Scanner
}

// openSSE subscribes to a.tsvt's change feed on the live server.
func openSSE(t *testing.T, base, lastID string) *sseSession {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, base+"/a.tsvt", nil)
	require.NoError(t, err)
	req.Header.Set("Accept", "text/event-stream")
	if lastID != "" {
		req.Header.Set("Last-Event-ID", lastID)
	}
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	t.Cleanup(func() { _ = resp.Body.Close() })
	require.Equal(t, http.StatusOK, resp.StatusCode)
	return &sseSession{resp: resp, scanner: bufio.NewScanner(resp.Body)}
}

// next reads one event (to its blank-line terminator) with a deadline.
func (s *sseSession) next(t *testing.T) map[string]string {
	t.Helper()
	event := map[string]string{}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for s.scanner.Scan() {
			line := s.scanner.Text()
			if line == "" {
				return
			}
			key, value, _ := strings.Cut(line, ": ")
			if prev, ok := event[key]; ok {
				value = prev + "\n" + value
			}
			event[key] = value
		}
	}()
	select {
	case <-done:
		return event
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for an SSE event")
		return nil
	}
}

func (s *sseSession) close() { _ = s.resp.Body.Close() }

func TestFeedDeliversChangedAndComputedEvents(t *testing.T) {
	h := newHandler(t, map[string]string{"a.tsvt": "1\t=A1*2\n"}, WithComputePlane)
	server := httptest.NewServer(h)
	defer server.Close()

	session := openSSE(t, server.URL, "")
	defer session.close()

	body := "setCell\tA1\t5\n"
	req, err := http.NewRequest(http.MethodPost, server.URL+"/a.tsvt", strings.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("Content-Type", TypeEdits)
	req.Header.Set("If-Match", etag(t, "1\t=A1*2\n"))
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	require.Equal(t, http.StatusNoContent, resp.StatusCode)
	_ = resp.Body.Close()

	changed := session.next(t)
	assert.Equal(t, "changed", changed["event"])
	assert.Contains(t, changed["data"], "setCell\tA1\t5")
	assert.Contains(t, changed["data"], "#.base\t"+revision(t, "1\t=A1*2\n"))
	assert.Contains(t, changed["data"], "#.rev\t"+revision(t, "5\t=A1*2\n"))

	computed := session.next(t)
	assert.Equal(t, "computed", computed["event"])
	assert.Contains(t, computed["data"], "A1\t5")
	assert.Contains(t, computed["data"], "B1\t10")
}

func TestFeedComputedDeltaOnRowDeletion(t *testing.T) {
	// Deleting a row shrinks the grid: the delta names the vacated cells with
	// their new (empty or shifted) values.
	h := newHandler(t, map[string]string{"a.tsvt": "1\t2\n3\t4\n"}, WithComputePlane)
	server := httptest.NewServer(h)
	defer server.Close()
	session := openSSE(t, server.URL, "")
	defer session.close()

	req, err := http.NewRequest(http.MethodPost, server.URL+"/a.tsvt", strings.NewReader("deleteRow\t1\n"))
	require.NoError(t, err)
	req.Header.Set("Content-Type", TypeEdits)
	req.Header.Set("If-Match", etag(t, "1\t2\n3\t4\n"))
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	require.Equal(t, http.StatusNoContent, resp.StatusCode)
	_ = resp.Body.Close()

	changed := session.next(t)
	require.Equal(t, "changed", changed["event"])
	computed := session.next(t)
	require.Equal(t, "computed", computed["event"])
	assert.Contains(t, computed["data"], "A1\t3")
	assert.Contains(t, computed["data"], "B1\t4")
	assert.Contains(t, computed["data"], "A2\t\n")
}
