// Command tsvsheet-api is the sheet API server: the tsvsheet edit language
// over HTTP. It serves a document plane (versioned .tsvt storage, edited by
// op batches, never evaluating a formula), a change feed (Server-Sent Events
// carrying the same batches), and — unless disabled — a compute plane serving
// computed values in the SPECIFICATION §9 vendor media types, which makes
// every served sheet IMPORT*-able by any other sheet. One engine everywhere:
// github.com/tsvsheet/go-tsvsheet.
package main

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"time"

	tsvsheet "github.com/tsvsheet/go-tsvsheet"
	"github.com/urfave/cli/v3"

	"github.com/tsvsheet/tsvsheet.api/internal/api"
	"github.com/tsvsheet/tsvsheet.api/internal/constants"
	"github.com/tsvsheet/tsvsheet.api/internal/store"
)

const (
	name  = `tsvsheet-api`
	usage = `Serve the sheet API: the tsvsheet edit language over HTTP.`

	description = `tsvsheet-api serves the documents under --root as the sheet API:

  GET    /{doc}            the source document (application/vnd.tsvsheet.doc+tsv), ETag = content revision
  POST   /{doc}            apply an edits batch (application/vnd.tsvsheet.edits+tsv, If-Match required)
  PUT    /{doc}            replace or create the whole document
  DELETE /{doc}            delete (If-Match required)
  GET    /{doc}            Accept: text/event-stream — the change feed
  GET    /{doc}!B7         computed reads in the SPECIFICATION §9 vendor types (compute plane)

The bind address must be loopback: this server carries no TLS and no
authentication of its own, so a wider exposure is refused rather than served
insecurely.`
)

// version is the application version. Set via ldflags: -X main.version=1.0.0
var version = "dev"

// The flag names.
const (
	flagRoot      = "root"
	flagAddr      = "addr"
	flagMaxCells  = "max-cells"
	flagNoCompute = "no-compute"
)

// The run path's seams, indirected so it stays testable without binding a
// real socket: osExit observes the exit code, serveHTTP performs the final
// blocking serve, and clock feeds the compute plane.
var (
	osExit              = os.Exit
	serveHTTP           = func(server *http.Server) error { return server.ListenAndServe() }
	clock     api.Clock = time.Now
)

func main() { osExit(run(os.Args)) }

// run builds and executes the CLI, returning the process exit code. A refusal
// is reported on the error stream before exiting: a bare non-zero status
// leaves an operator guessing whether the bind, the root, or the flags were
// the problem.
func run(args []string) int {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	if err := createApp().Run(ctx, args); err != nil {
		_, _ = fmt.Fprintln(stderr, name+": "+err.Error())
		return 1
	}
	return 0
}

// stderr is the diagnostics stream, indirected so a test can read what an
// operator would see.
var stderr io.Writer = os.Stderr

// createApp constructs the CLI: a single root command that serves.
func createApp() *cli.Command {
	return &cli.Command{
		Name:                  name,
		Usage:                 usage,
		Description:           description,
		Version:               version,
		EnableShellCompletion: true,
		Flags: []cli.Flag{
			&cli.StringFlag{Name: flagRoot, Usage: "directory of .tsvt documents to serve", Required: true},
			&cli.StringFlag{Name: flagAddr, Usage: "loopback address to bind", Value: "127.0.0.1:8787"},
			&cli.IntFlag{
				Name:  flagMaxCells,
				Usage: "cap on cells, grid dimension, and bytes per result (0 = built-in default)",
				Value: 0,
			},
			&cli.BoolFlag{
				Name:  flagNoCompute,
				Usage: "serve the document plane only: no computed reads, no computed events",
			},
		},
		Action: serve,
	}
}

// serve builds the server from the parsed flags and runs it.
func serve(_ context.Context, c *cli.Command) error {
	server, err := buildServer(c)
	if err != nil {
		return err
	}
	return serveHTTP(server)
}

// buildServer assembles the http.Server: a confined store, the API handler,
// and a loopback-verified bind address.
func buildServer(c *cli.Command) (*http.Server, error) {
	addr := c.String(flagAddr)
	if err := requireLoopback(bindAddr(addr)); err != nil {
		return nil, err
	}
	limits := limitsOf(cellCap(c.Int(flagMaxCells)))
	st, err := store.Open(store.RootDir(c.String(flagRoot)), limits)
	if err != nil {
		return nil, err
	}
	handler := api.NewHandler(api.Config{
		Store:          st,
		Limits:         limits,
		ComputeEnabled: api.ComputePlane(!c.Bool(flagNoCompute)),
		Clock:          clock,
	})
	return &http.Server{Addr: addr, Handler: handler, ReadHeaderTimeout: headerTimeout}, nil
}

// headerTimeout bounds how long a client may dawdle over its request head.
const headerTimeout = 10 * time.Second

// cellCap is the --max-cells resource cap.
type cellCap int

// limitsOf resolves the --max-cells cap: a positive cap bounds everything a
// formula result or grid may reach; zero keeps the engine's defaults.
func limitsOf(ceiling cellCap) tsvsheet.Limits {
	if ceiling > 0 {
		return tsvsheet.Limits{ResultCells: int(ceiling), GridDim: int(ceiling), ResultBytes: int(ceiling)}
	}
	return tsvsheet.DefaultLimits()
}

// bindAddr is a host:port bind address.
type bindAddr string

// requireLoopback refuses any bind address that is not loopback: the server
// carries no TLS and no authentication, so exposure is an error, not a
// warning.
func requireLoopback(addr bindAddr) error {
	host, _, err := net.SplitHostPort(string(addr))
	if err != nil {
		return constants.ErrListen.With(err, "addr", addr)
	}
	if host == "localhost" {
		return nil
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return constants.ErrNotLoopback.With(nil, "addr", addr)
	}
	return nil
}
