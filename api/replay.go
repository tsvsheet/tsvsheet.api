// Idempotency-Key replay: a retried batch returns its original result instead
// of re-applying or failing a now-stale precondition.
package api

import (
	"crypto/sha256"
	"net/http"
	"sync"

	tsvsheet "github.com/tsvsheet/go-tsvsheet"

	"github.com/tsvsheet/tsvsheet.api/document"
)

// replayOutcome is what one Idempotency-Key produced: the revision it left
// behind and a digest of the batch it was used for.
type replayOutcome struct {
	rev  tsvsheet.RevisionHex
	body [sha256.Size]byte
}

// replayCache remembers each Idempotency-Key's outcome so a retried POST
// returns its original result instead of re-applying (or failing a stale
// precondition). Bounded: when full, the cache resets — a replay after that
// simply re-runs the normal precondition path.
type replayCache struct {
	seen map[string]replayOutcome
	mu   sync.Mutex
}

// replayCap bounds the cache.
const replayCap = 1024

// newReplayCache builds an empty cache.
func newReplayCache() *replayCache { return &replayCache{seen: map[string]replayOutcome{}} }

// replayed answers a remembered key with its original outcome, or refuses the
// key when it was first used for a different batch. A replay must be a repeat
// of the same request: answering a *new* batch with an old result would drop
// the edit and report success the client could not detect —
// TestIdempotencyKeyRefusesADifferentBatch asserts the refusal.
func (handler Handler) replayed(w http.ResponseWriter, r *http.Request, doc document.DocPath, body []byte) bool {
	key := r.Header.Get("Idempotency-Key")
	if key == "" {
		return false
	}
	handler.replay.mu.Lock()
	prior, ok := handler.replay.seen[replayKey(doc, idempotencyKey(key))]
	handler.replay.mu.Unlock()
	if !ok {
		return false
	}
	if prior.body != bodyDigest(body) {
		writeProblem(
			w,
			http.StatusUnprocessableEntity,
			problemBadRequest,
			"idempotency key reused for a different batch",
		)
		return true
	}
	w.Header().Set("ETag", quote(prior.rev))
	w.WriteHeader(http.StatusNoContent)
	return true
}

// idempotencyKey is a client-supplied retry key.
type idempotencyKey string

// replayKey scopes a key to its document, so one client's key on one sheet
// does not answer a request against another —
// TestIdempotencyKeyIsScopedToItsDocument asserts it.
func replayKey(doc document.DocPath, key idempotencyKey) string {
	return string(doc) + "\x00" + string(key)
}

// bodyDigest content-addresses a request body, so a reused key is checked
// against what it was first used for without retaining the batch.
func bodyDigest(body []byte) [sha256.Size]byte { return sha256.Sum256(body) }

// remember records a successful application under its Idempotency-Key.
func (handler Handler) remember(r *http.Request, doc document.DocPath, rev tsvsheet.RevisionHex, body []byte) {
	key := r.Header.Get("Idempotency-Key")
	if key == "" {
		return
	}
	handler.replay.mu.Lock()
	defer handler.replay.mu.Unlock()
	if len(handler.replay.seen) >= replayCap {
		handler.replay.seen = map[string]replayOutcome{}
	}
	handler.replay.seen[replayKey(doc, idempotencyKey(key))] = replayOutcome{rev: rev, body: bodyDigest(body)}
}
