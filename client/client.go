// Package client is the document plane's remote adapter: a document.Port
// implemented over the sheet API, so a frontend that can edit a local file can
// edit a document on a server without knowing the difference.
//
// Its whole job is fidelity. Every refusal the embedded port returns as a
// sentinel, this one reconstructs from the response's RFC 9457 problem type,
// so the conformance suite can assert identical behaviour against both. A
// transport fault — unreachable server, unintelligible answer — is
// document.ErrUnavailable, which is never a statement about the document
// itself.
package client

import (
	"context"
	"net/http"

	tsvsheet "github.com/tsvsheet/go-tsvsheet"

	"github.com/tsvsheet/tsvsheet.api/api"
	"github.com/tsvsheet/tsvsheet.api/document"
)

// BaseURL is the sheet API root a client addresses.
type BaseURL string

// Doer is the HTTP surface a client needs, injected so a caller supplies its
// own timeouts, transport, or test double.
type Doer interface {
	Do(req *http.Request) (*http.Response, error)
}

// Client is a document.Port over the sheet API.
type Client struct {
	doer   Doer
	base   BaseURL
	limits tsvsheet.Limits
}

// New builds a client against base. A nil doer uses http.DefaultClient. The
// engine limits bound the local fold that verifies each accepted batch.
func New(base BaseURL, doer Doer) Client { return NewWithLimits(base, doer, tsvsheet.DefaultLimits()) }

// NewWithLimits builds a client whose local verification fold is bounded by
// the given limits, which should match the server's so the two agree about
// what a batch may do.
func NewWithLimits(base BaseURL, doer Doer, limits tsvsheet.Limits) Client {
	if doer == nil {
		doer = http.DefaultClient
	}
	return Client{base: base, doer: doer, limits: limits}
}

// maxBodyBytes bounds a response body, so a misbehaving server does not
// exhaust the client the way an unbounded read would —
// TestOversizeResponseIsTruncatedNotFatal asserts the bound holds.
const maxBodyBytes = 8 << 20

// Get reads the document's canonical source and revision.
func (c Client) Get(ctx context.Context, doc document.DocPath) (document.Snapshot, error) {
	got, err := c.roundTrip(ctx, http.MethodGet, doc, api.TypeDoc, nil, nil)
	if err != nil {
		return document.Snapshot{}, err
	}
	if got.status != http.StatusOK {
		return document.Snapshot{}, problemError(got)
	}
	return snapshotOf(got.body, got.etag)
}

// Apply folds an edits batch over the document at the expected revision.
func (c Client) Apply(
	ctx context.Context,
	doc document.DocPath,
	batch tsvsheet.Edits,
	expect tsvsheet.RevisionHex,
) (document.Applied, error) {
	before, err := c.Get(ctx, doc)
	if err != nil {
		return document.Applied{}, err
	}
	got, err := c.roundTrip(ctx, http.MethodPost, doc, api.TypeEdits, batch.Text(), conditional(expect))
	if err != nil {
		return document.Applied{}, err
	}
	if got.status != http.StatusNoContent {
		return document.Applied{}, problemError(got)
	}
	return applied(before, batch, got.etag, c.limits)
}

// applied builds the result of a batch the server accepted, by folding the
// same batch locally rather than reading the document back.
//
// Reading back would be wrong, not merely slower: between the write and the
// read another writer can change the document, so the caller would be told it
// produced a state some other batch produced — or, if the document were
// deleted in between, that its successful edit had failed. Folding locally
// also verifies the server: one engine means the revision computed here must
// equal the one it named, and a disagreement means the two ran different
// semantics, which no caller should be asked to paper over.
func applied(
	before document.Snapshot,
	batch tsvsheet.Edits,
	served tsvsheet.RevisionHex,
	limits tsvsheet.Limits,
) (document.Applied, error) {
	next, err := tsvsheet.Apply(before.Doc, batch, limits)
	if err != nil {
		return document.Applied{}, document.ErrUnavailable.With(err, "served", string(served))
	}
	computed := tsvsheet.Revision(next)
	if computed != served {
		return document.Applied{}, document.ErrUnavailable.With(
			nil,
			"served",
			string(served),
			"computed",
			string(computed),
		)
	}
	return document.Applied{
		Old: before,
		New: document.Snapshot{Doc: next, Rev: computed, Text: next.Text()},
	}, nil
}

// Put replaces or creates the document from a whole body.
func (c Client) Put(
	ctx context.Context,
	doc document.DocPath,
	body []byte,
	expect document.Expect,
) (document.Snapshot, document.Created, error) {
	if expect.IsZero() {
		return document.Snapshot{}, false, document.ErrPrecondition.With(nil, "doc", string(doc), "expect", "none")
	}
	got, err := c.roundTrip(ctx, http.MethodPut, doc, api.TypeDoc, body, precondition(expect))
	if err != nil {
		return document.Snapshot{}, false, err
	}
	if got.status != http.StatusOK && got.status != http.StatusCreated {
		return document.Snapshot{}, false, problemError(got)
	}
	// The server stores the canonical form, which may differ from the body
	// sent, so the snapshot returned is what it now holds — not what was
	// posted.
	stored, err := c.Get(ctx, doc)
	if err != nil {
		return document.Snapshot{}, false, err
	}
	return stored, document.Created(got.status == http.StatusCreated), nil
}

// Delete removes the document at the expected revision.
func (c Client) Delete(ctx context.Context, doc document.DocPath, expect tsvsheet.RevisionHex) error {
	got, err := c.roundTrip(ctx, http.MethodDelete, doc, "", nil, conditional(expect))
	if err != nil {
		return err
	}
	if got.status != http.StatusNoContent {
		return problemError(got)
	}
	return nil
}
