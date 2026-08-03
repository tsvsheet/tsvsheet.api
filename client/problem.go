// Reconstructing the document plane's errors from the wire: an entity tag
// becomes a revision, a body becomes a verified snapshot, and an RFC 9457
// problem becomes the same sentinel the embedded port would have returned.
package client

import (
	"encoding/json"
	"strings"

	tsvsheet "github.com/tsvsheet/go-tsvsheet"

	"github.com/tsvsheet/tsvsheet.api/api"
	"github.com/tsvsheet/tsvsheet.api/document"
)

// revisionOf reads an entity tag as a revision.
func revisionOf(tag entityTag) tsvsheet.RevisionHex {
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
	if rev == "" {
		// Without an entity tag there is nothing to check the body against,
		// and a silently truncated response would parse cleanly. An
		// unverifiable answer is an unusable one.
		return document.Snapshot{}, document.ErrUnavailable.With(nil, "etag", "absent")
	}
	if computed != rev {
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
	api.SlugDocTooLarge:  tsvsheet.ErrDocTooLarge,
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
