// Package idempotency hosts the X-Command-Id idempotency surface per
// `messaging.md` "Command idempotency" + Stripe canon (Idempotency-Key
// header).
//
// Two pieces:
//
//   - [Store] — the storage port. `InMemoryStore` ships today;
//     `PostgresStore` lands when the first endpoint needs cross-
//     restart persistence (A.3 endpoint sprint).
//   - [Middleware] (in middleware.go) — the HTTP wrapper that wires
//     [Store] around mutating handlers. Reads `X-Command-Id`; on
//     match-body replay returns the cached response with
//     `X-Idempotent-Replay: true`; on mismatch-body replay returns
//     422 Idempotency.KeyReuse.
//
// Per `messaging.md`:
//
//   - Absent X-Command-Id header → no idempotency (opt-in).
//   - Malformed UUID → 400.
//   - Default TTL: 24 hours.
//
// Industry alignment: Stripe's Idempotency-Key (canonical 2017+),
// GitHub API request IDs, AWS API Gateway X-Amzn-Trace-Id semantics.
// RFC 9110 §17 (HTTP idempotent methods) describes the spec; Stripe's
// blog post "Designing robust + predictable APIs with idempotency"
// (2018) is the load-bearing reference for the body-mismatch detection.
package idempotency

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
)

// ErrInvalid is returned for validation failures (bad UUID, etc).
var ErrInvalid = errors.New("idempotency: invalid")

// ErrBodyMismatch is returned by [Store.Get] when the supplied body
// hash differs from the hash stored alongside the original response —
// the same X-Command-Id was reused with different request body, which
// is a programmer error. Surfaces as HTTP 422 Idempotency.KeyReuse
// per the middleware contract.
var ErrBodyMismatch = errors.New("idempotency: command id reused with different body")

// Record is the persisted output of an idempotent command. The
// response body / status / content-type are captured on the first
// call's success and replayed verbatim on subsequent matches.
type Record struct {
	Key             uuid.UUID
	BodyHash        [32]byte // SHA-256 of the request body
	ResponseStatus  int
	ResponseBody    []byte
	ResponseHeaders map[string]string // typically just Content-Type
	CreatedAt       time.Time
	ExpiresAt       time.Time
}

// Store is the storage port. Implementations MUST be safe for
// concurrent use — production callers invoke from request handlers
// under load.
type Store interface {
	// Get returns the previously-stored Record for key.
	//
	// Returns:
	//   - (record, nil)               — match: replay this response
	//   - (zero, ErrBodyMismatch)     — same key, different body hash
	//   - (zero, nil)                 — no record (first request OR expired)
	//   - (zero, other err)           — store failure (transient)
	Get(ctx context.Context, key uuid.UUID, bodyHash [32]byte) (Record, error)

	// Put stores a record. Overwrites silently if a stale (expired)
	// record sits at the same key (avoids leaking past TTL via
	// non-key-aware probing).
	Put(ctx context.Context, r Record) error

	// Purge drops expired records. Implementations may also expire
	// lazily inside Get; Purge is a forced sweep for ops tooling.
	Purge(ctx context.Context, now time.Time) (int, error)
}

// ----- InMemoryStore -------------------------------------------------

// InMemoryStore is a sync.RWMutex-guarded map[uuid.UUID]Record. Used
// in tests + single-instance dev. NOT durable — process restart loses
// all idempotency state.
//
// Production wires PostgresStore (deferred to A.3 endpoint sprint —
// needs `app.command_idempotency` migration).
type InMemoryStore struct {
	mu      sync.RWMutex
	records map[uuid.UUID]Record
	now     func() time.Time // injectable for tests
}

// NewInMemoryStore constructs an InMemoryStore. Pass nil for `now` to
// use time.Now (tests can substitute clock.Now via the package's
// test-clock pattern).
func NewInMemoryStore(now func() time.Time) *InMemoryStore {
	if now == nil {
		now = time.Now
	}
	return &InMemoryStore{
		records: make(map[uuid.UUID]Record),
		now:     now,
	}
}

// Get implements [Store.Get].
func (s *InMemoryStore) Get(_ context.Context, key uuid.UUID, bodyHash [32]byte) (Record, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	r, ok := s.records[key]
	if !ok {
		return Record{}, nil
	}
	// Lazy expiry: act as though the record doesn't exist past TTL.
	// Keeps the call site from having to call Purge before Get on
	// every request.
	if !s.now().Before(r.ExpiresAt) {
		return Record{}, nil
	}
	if r.BodyHash != bodyHash {
		return Record{}, fmt.Errorf("%w: key %s", ErrBodyMismatch, key)
	}
	return r, nil
}

// Put implements [Store.Put].
func (s *InMemoryStore) Put(_ context.Context, r Record) error {
	if r.Key == uuid.Nil {
		return fmt.Errorf("%w: nil key", ErrInvalid)
	}
	if r.ExpiresAt.IsZero() {
		return fmt.Errorf("%w: ExpiresAt required", ErrInvalid)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.records[r.Key] = r
	return nil
}

// Purge implements [Store.Purge].
func (s *InMemoryStore) Purge(_ context.Context, now time.Time) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	purged := 0
	for k, r := range s.records {
		if !now.Before(r.ExpiresAt) {
			delete(s.records, k)
			purged++
		}
	}
	return purged, nil
}

// Len returns the current entry count — useful for tests + /health
// reporting. NOT part of the [Store] interface (impl-specific).
func (s *InMemoryStore) Len() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.records)
}
