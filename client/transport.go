// The client's transport mechanics: building one request, performing it, and
// reading its bounded body. The http.Response never escapes this file, so no
// caller can leak a connection by forgetting to close one.
package client

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/url"

	tsvsheet "github.com/tsvsheet/go-tsvsheet"

	"github.com/tsvsheet/tsvsheet.api/document"
)

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
	return answer{status: resp.StatusCode, etag: revisionOf(entityTag(resp.Header.Get("ETag"))), body: read}, nil
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
	// The path is checked before joining: URL joining CLEANS "..", so an
	// unvalidated traversal would silently address a different document
	// instead of being refused — and would escape the base's mount point.
	if err := document.Validate(doc); err != nil {
		return nil, err
	}
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
		if method != http.MethodGet {
			// Set on every write, including a nil body: a nil slice is the
			// natural spelling of an empty document, and omitting the type
			// would make the server refuse what the embedded port accepts.
			req.Header.Set("Content-Type", mediaType)
		}
	}
	for _, h := range headers {
		req.Header.Set(h.name, h.value)
	}
	return req, nil
}

// entityTag is an ETag header value, quotes and all.
type entityTag string
