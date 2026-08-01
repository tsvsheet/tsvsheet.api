# tsvsheet.api

> **A spreadsheet in plain text.** The sheet API server: the [tsvsheet edit language](https://github.com/tsvsheet/.projects/blob/main/specs/tsvsheet/ideas/sheet-edit-language.md) carried over HTTP (work order 011). One URL per document; method × media type selects the action.

## Architecture

- [cmd/tsvsheet-api](cmd/tsvsheet-api/) — the urfave/cli v3 entry point: `--root` (served directory), `--addr` (loopback only — the server carries no TLS and no auth, so a wider bind is refused, never served), `--max-cells`, `--no-compute`.
- [internal/store](internal/store/) — the confined document store: `os.Root` containment, canonical bytes only (`Document.Text()`), content-addressed revisions (SHA-256), atomic temp+rename writes, every mutation revision-checked under one lock. Evaluates nothing.
- [internal/api](internal/api/) — the HTTP surface: document plane (GET/HEAD/PUT/POST/DELETE with strong `ETag`/`If-Match`, RFC 9457 problems, `Idempotency-Key` replay), the SSE change feed (`changed` batches with `#.base`/`#.rev`, `computed` cell deltas, `reset`, `deleted`; `Last-Event-ID` replay from a bounded in-memory ring), and the compute plane (SPECIFICATION §9 vendor types; `GET /{doc}!B7` reference reads).

## Non-negotiables

- **The source/computed rule is a hard safety rule.** `application/vnd.tsvsheet+tsv`, `text/tab-separated-values`, and `text/csv` mean _computed values_ (§9 importers ingest them values-only); source bytes are only ever served as `application/vnd.tsvsheet.doc+tsv`. A values `Accept` without the compute plane is `406`, never source.
- **One engine.** Every semantic act — canonicalization, op application, computation — is [go-tsvsheet](https://github.com/tsvsheet/go-tsvsheet). No parsing or evaluation is re-implemented here.
- **The document plane never evaluates an expression** — op application parses formulas for AST rewriting only. Compute is an additive, advertised capability (`Tsvsheet-Capabilities`).
- **The full gomatic Go gate applies:** `make check` green — 100% aggregate coverage, gocognit ≤ 7, `errs.Const` sentinels in [internal/constants](internal/constants/), no `fmt.Errorf`/`errors.New` in production code, value receivers except the documented stateful types (store, hub, handler, replay cache).
