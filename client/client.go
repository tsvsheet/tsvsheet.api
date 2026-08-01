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
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"

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
	doer Doer
	base BaseURL
}

// New builds a client against base. A nil doer uses http.DefaultClient.
func New(base BaseURL, doer Doer) Client {
	if doer == nil {
		return Client{base: base, doer: http.DefaultClient}
	}
	return Client{base: base, doer: doer}
}

// maxBodyBytes bounds a response body, so a misbehaving server cannot exhaust
// the client the way an unbounded read would.
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
	after, err := c.Get(ctx, doc)
	if err != nil {
		return document.Applied{}, err
	}
	return document.Applied{Old: before, New: after}, nil
}

// Put replaces or creates the document from a whole body.
func (c Client) Put(
	ctx context.Context,
	doc document.DocPath,
	body []byte,
	expect document.Expect,
) (document.Snapshot, document.Created, error) {
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

// header is one request header the caller sets.
type header struct {
	name  string
	value string
}

// conditional is the If-Match header for a revision-conditioned mutation.
func conditional(expect tsvsheet.RevisionHex) []header {
	return []header{{name: "If-Match", value: `"` + string(expect) + `"`}}
}

// precondition is the header a write's precondition implies: If-None-Match for
// a create, If-Match for a replace.
func precondition(expect document.Expect) []header {
	rev, isAbsent := expect.Revision()
	if isAbsent {
		return []header{{name: "If-None-Match", value: "*"}}
	}
	return conditional(rev)
}

// answer is one completed exchange: the status, the entity tag, and the body,
// read and closed. The response itself never escapes roundTrip, so no caller
// can leak a connection by forgetting to close one.
type answer struct {
	etag   tsvsheet.RevisionHex
	body   []byte
	status int
}

// roundTrip performs one request and reads its bounded body.
func (c Client) roundTrip(
	ctx context.Context,
	method string,
	doc document.DocPath,
	mediaType string,
	body []byte,
	headers []header,
) (answer, error) {
	req, err := c.request(ctx, method, doc, mediaType, body, headers)
	if err != nil {
		return answer{}, err
	}
	resp, err := c.doer.Do(req)
	if err != nil {
		return answer{}, document.ErrUnavailable.With(err, "doc", string(doc))
	}
	defer func() { _ = resp.Body.Close() }()
	read, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
	if err != nil {
		return answer{}, document.ErrUnavailable.With(err, "doc", string(doc))
	}
	return answer{status: resp.StatusCode, etag: revisionOf(EntityTag(resp.Header.Get("ETag"))), body: read}, nil
}

// request builds one request against the document's URL.
func (c Client) request(
	ctx context.Context,
	method string,
	doc document.DocPath,
	mediaType string,
	body []byte,
	headers []header,
) (*http.Request, error) {
	// A base URL that will not join is a misconfigured port, not a statement
	// about the document — a caller must not read it as "no such sheet".
	target, err := url.JoinPath(string(c.base), string(doc))
	if err != nil {
		return nil, document.ErrUnavailable.With(err, "base", string(c.base), "doc", string(doc))
	}
	req, err := http.NewRequestWithContext(ctx, method, target, bytes.NewReader(body))
	if err != nil {
		return nil, document.ErrUnavailable.With(err, "doc", string(doc))
	}
	if mediaType != "" {
		req.Header.Set("Accept", mediaType)
		if body != nil {
			req.Header.Set("Content-Type", mediaType)
		}
	}
	for _, h := range headers {
		req.Header.Set(h.name, h.value)
	}
	return req, nil
}

// EntityTag is an ETag header value, quotes and all.
type EntityTag string

// revisionOf reads an entity tag as a revision.
func revisionOf(tag EntityTag) tsvsheet.RevisionHex {
	unquoted := strings.TrimPrefix(string(tag), `"`)
	return tsvsheet.RevisionHex(strings.TrimSuffix(unquoted, `"`))
}

// snapshotOf parses served source into a snapshot, verifying that the
// revision the server named is the address of the bytes it sent — a server
// whose header and body disagree is unusable, not merely wrong.
func snapshotOf(body []byte, rev tsvsheet.RevisionHex) (document.Snapshot, error) {
	doc, err := tsvsheet.ParseDocument(body)
	if err != nil {
		return document.Snapshot{}, document.ErrUnavailable.With(err, "rev", string(rev))
	}
	computed := tsvsheet.Revision(doc)
	if rev != "" && computed != rev {
		return document.Snapshot{}, document.ErrUnavailable.With(nil, "etag", string(rev), "body", string(computed))
	}
	return document.Snapshot{Doc: doc, Rev: computed, Text: doc.Text()}, nil
}

// problemBody is the RFC 9457 document a refusal carries.
type problemBody struct {
	Type   string `json:"type"`
	Detail string `json:"detail"`
}

// problemError reconstructs the document plane's sentinel from a refusal, so a
// caller matches the same errors it would from the embedded port. An
// unrecognized refusal is ErrUnavailable rather than a guess: inventing a
// sentinel would make the two adapters disagree silently.
func problemError(got answer) error {
	if sentinel, ok := sentinels[slugOf(got.body)]; ok {
		return sentinel.With(nil, "status", got.status, "detail", detailOf(got.body))
	}
	return document.ErrUnavailable.With(nil, "status", got.status, "detail", detailOf(got.body))
}

// sentinels maps each problem slug to the error the embedded port returns for
// the same cause.
var sentinels = map[api.ProblemSlug]interface{ With(error, ...any) error }{
	api.SlugNotFound:     document.ErrMissing,
	api.SlugPrecondition: document.ErrPrecondition,
	api.SlugConflict:     document.ErrExists,
	api.SlugBadDocument:  document.ErrSyntax,
	api.SlugStaleBase:    tsvsheet.ErrEditsBase,
	api.SlugRefusedEdits: tsvsheet.ErrEditsApply,
	api.SlugBadEdits:     tsvsheet.ErrEditsOp,
}

// slugOf reads the problem type's trailing slug.
func slugOf(body []byte) api.ProblemSlug {
	var problem problemBody
	if err := json.Unmarshal(body, &problem); err != nil {
		return ""
	}
	return api.ProblemSlug(strings.TrimPrefix(problem.Type, api.ProblemBase))
}

// detailOf reads the problem's human-readable detail, for the error message.
func detailOf(body []byte) string {
	var problem problemBody
	if err := json.Unmarshal(body, &problem); err != nil {
		return ""
	}
	return problem.Detail
}
