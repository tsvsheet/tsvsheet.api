package main

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	tsvsheet "github.com/tsvsheet/go-tsvsheet"
)

// withServeStub swaps the blocking serve for a capture, restoring it after.
func withServeStub(t *testing.T, fn func(*http.Server) error) *[]*http.Server {
	t.Helper()
	var captured []*http.Server
	prev := serveHTTP
	serveHTTP = func(server *http.Server) error {
		captured = append(captured, server)
		return fn(server)
	}
	t.Cleanup(func() { serveHTTP = prev })
	return &captured
}

// docRoot is a temp root holding one document.
func docRoot(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "a.tsvt"), []byte("1\t=A1*2\n"), 0o600))
	return dir
}

func TestRunServesTheConfiguredRoot(t *testing.T) {
	captured := withServeStub(t, func(*http.Server) error { return nil })
	code := run([]string{name, "--root", docRoot(t), "--addr", "127.0.0.1:0"})
	assert.Equal(t, 0, code)
	require.Len(t, *captured, 1)
	server := (*captured)[0]
	assert.Equal(t, "127.0.0.1:0", server.Addr)

	// The wired handler serves the document plane and the compute plane.
	rec := httptest.NewRecorder()
	server.Handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/a.tsvt", nil))
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "1\t=A1*2\n", rec.Body.String())
	assert.Equal(t, "edits, events, compute", rec.Header().Get("Tsvsheet-Capabilities"))

	computed := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/a.tsvt!B1", nil)
	server.Handler.ServeHTTP(computed, req)
	assert.Equal(t, "2\n", computed.Body.String())
}

func TestRunNoComputeServesDocumentPlaneOnly(t *testing.T) {
	captured := withServeStub(t, func(*http.Server) error { return nil })
	code := run([]string{name, "--root", docRoot(t), "--no-compute"})
	assert.Equal(t, 0, code)
	require.Len(t, *captured, 1)
	rec := httptest.NewRecorder()
	(*captured)[0].Handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/a.tsvt", nil))
	assert.Equal(t, "edits, events", rec.Header().Get("Tsvsheet-Capabilities"))
}

func TestRunMaxCellsCapsTheEngine(t *testing.T) {
	captured := withServeStub(t, func(*http.Server) error { return nil })
	code := run([]string{name, "--root", docRoot(t), "--max-cells", "2"})
	assert.Equal(t, 0, code)
	require.Len(t, *captured, 1)

	// An edit beyond the capped grid dimension is refused (422); without the
	// cap the same edit would land.
	source := "1\t=A1*2\n"
	doc, err := docRevision(source)
	require.NoError(t, err)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/a.tsvt", strings.NewReader("setCell\tE9\tfar\n"))
	req.Header.Set("Content-Type", "application/vnd.tsvsheet.edits+tsv")
	req.Header.Set("If-Match", `"`+doc+`"`)
	(*captured)[0].Handler.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusUnprocessableEntity, rec.Code)
}

// docRevision content-addresses source through the engine.
func docRevision(source string) (string, error) {
	doc, err := tsvsheet.ParseDocument([]byte(source))
	if err != nil {
		return "", err
	}
	return string(tsvsheet.Revision(doc)), nil
}

func TestRunRefusesNonLoopbackBind(t *testing.T) {
	_ = withServeStub(t, func(*http.Server) error { return nil })
	code := run([]string{name, "--root", docRoot(t), "--addr", "0.0.0.0:8787"})
	assert.Equal(t, 1, code)
}

func TestRunRefusesMalformedAddr(t *testing.T) {
	_ = withServeStub(t, func(*http.Server) error { return nil })
	code := run([]string{name, "--root", docRoot(t), "--addr", "no-port"})
	assert.Equal(t, 1, code)
}

func TestRunRefusesUnresolvableHost(t *testing.T) {
	_ = withServeStub(t, func(*http.Server) error { return nil })
	code := run([]string{name, "--root", docRoot(t), "--addr", "example.com:80"})
	assert.Equal(t, 1, code)
}

func TestLocalhostIsLoopback(t *testing.T) {
	captured := withServeStub(t, func(*http.Server) error { return nil })
	code := run([]string{name, "--root", docRoot(t), "--addr", "localhost:0"})
	assert.Equal(t, 0, code)
	assert.Len(t, *captured, 1)
}

func TestRunMissingRootDirectoryFails(t *testing.T) {
	_ = withServeStub(t, func(*http.Server) error { return nil })
	code := run([]string{name, "--root", filepath.Join(t.TempDir(), "absent")})
	assert.Equal(t, 1, code)
}

func TestRunServeFailureIsExitOne(t *testing.T) {
	_ = withServeStub(t, func(*http.Server) error { return errors.New("bind refused") })
	code := run([]string{name, "--root", docRoot(t)})
	assert.Equal(t, 1, code)
}

func TestMainUsesOSExit(t *testing.T) {
	prevExit, prevServe := osExit, serveHTTP
	t.Cleanup(func() { osExit, serveHTTP = prevExit, prevServe })
	var code int
	osExit = func(c int) { code = c }
	serveHTTP = func(*http.Server) error { return nil }
	prevArgs := os.Args
	t.Cleanup(func() { os.Args = prevArgs })
	os.Args = []string{name, "--root", docRoot(t)}
	main()
	assert.Equal(t, 0, code)
}
