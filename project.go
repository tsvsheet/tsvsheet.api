// Package tsvsheetapi is the module root for tsvsheet-api — the sheet API
// server that carries the tsvsheet edit language over HTTP: a document plane
// (versioned .tsvt storage edited by op batches), a change feed (Server-Sent
// Events replaying the same batches), and a compute plane serving computed
// values in the SPECIFICATION §9 vendor media types, so any served sheet is
// IMPORT*-able by any other sheet. The runnable entry point is
// cmd/tsvsheet-api; the HTTP surface lives under internal/api, the confined
// document store under internal/store, and the engine itself is
// github.com/tsvsheet/go-tsvsheet.
package tsvsheetapi
