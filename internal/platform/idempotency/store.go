// Package idempotency hosts the X-Command-Id idempotency surface per
// `messaging.md` "Command idempotency" + Stripe canon (Idempotency-Key
// header).
//
// Two pieces:
//
//   - [Store] — the storage port. [InMemoryStore] for single-process
//     dev/tests; [PostgresStore] for production (durable, multi-replica
//     correct under SKIP-LOCKED race semantics).
//   - [Middleware] (in middleware.go) — the HTTP wrapper that wires
//     [Store] around mutating handlers. Reads `X-Command-Id`; on
//     match-body replay returns the cached response with
//     `X-Idempotent-Replay: true`; on mismatch-body replay returns
//     422 idempotency.key_reuse.
//
// Per `messaging.md`:
//
//   - Absent X-Command-Id header → no idempotency (opt-in).
//   - Malformed UUID → 400.
//   - Default TTL: 24 hours.
//
// Per-caller scoping: Stripe canon scopes idempotency keys per API key
// (= per caller). LeadKart binds CallerID to the authenticated tenant
// (or "platform:<userID>" for operator paths). Without scoping, two
// tenants picking the same X-Command-Id collide → cross-tenant data
// leak via response replay. The [Store] interface threads CallerID
// through; the middleware extracts it via a wired keyer.
//
// Industry alignment: Stripe Idempotency-Key (canonical 2017+),
// GitHub API request IDs, AWS API Gateway X-Amzn-Trace-Id semantics,
// RFC 9110 §17 (HTTP idempotent methods).
package idempotency

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrInvalid is returned for validation failures (bad UUID, etc).
var ErrInvalid = errors.New("idempotency: invalid")

// ErrBodyMismatch is returned by [Store.Get] when the supplied body
// hash differs from the hash stored alongside the original response —
// the same X-Command-Id was reused with different request body, which
// is a programmer error. Surfaces as HTTP 422 idempotency.key_reuse
// per the middleware contract.
var ErrBodyMismatch = errors.New("idempotency: command id reused with different body")

// MaxCallerIDLen mirrors the migration's CHECK constraint —
// length(caller_id) ≤ 200. Bound here so the in-memory + Postgres
// stores reject identical inputs.
const MaxCallerIDLen = 200

// Record is the persisted output of an idempotent command. The
// response body / status / content-type are captured on the first
// call's success and replayed verbatim on subsequent matches.
type Record struct {
	CallerID        string   // Per-caller scope (see package doc + migration 20260507000007)
	Key             uuid.UUID
	BodyHash        [32]byte // SHA-256 of the request body
	ResponseStatus  int
	ResponseBody    []byte
	ResponseHeaders map[string]string // Content-Type + any handler-set replay-relevant headers
	CreatedAt       time.Time
	ExpiresAt       time.Time
}

// Store is the storage port. Implementations MUST be safe for
// concurrent use — production callers invoke from request handlers
// under load.
type Store interface {
	// Get returns the previously-stored Record for (callerID, key).
	//
	// Returns:
	//   - (record, nil)               — match: replay this response
	//   - (zero, ErrBodyMismatch)     — same key, different body hash
	//   - (zero, nil)                 — no record (first request OR expired)
	//   - (zero, other err)           — store failure (transient)
	Get(ctx context.Context, callerID string, key uuid.UUID, bodyHash [32]byte) (Record, error)

	// Put stores a record. Overwrites silently if a stale (expired)
	// record sits at the same (callerID, key) (avoids leaking past TTL
	// via non-key-aware probing).
	Put(ctx context.Context, r Record) error

	// Purge drops expired records. Implementations may also expire
	// lazily inside Get; Purge is a forced sweep for ops tooling.
	Purge(ctx context.Context, now time.Time) (int, error)
}

// composite is the in-memory store's map key — caller_id + key together
// scope the record per Stripe canon.
type composite struct {
	caller string
	key    uuid.UUID
}

// ----- InMemoryStore -------------------------------------------------

// InMemoryStore is a sync.RWMutex-guarded map[composite]Record. Used
// in tests + single-instance dev. NOT durable — process restart loses
// all idempotency state. Production wires [PostgresStore].
type InMemoryStore struct {
	mu      sync.RWMutex
	records map[composite]Record
	now     func() time.Time
}

// NewInMemoryStore constructs an InMemoryStore. Pass nil for `now` to
// use time.Now (tests can substitute a fake clock).
func NewInMemoryStore(now func() time.Time) *InMemoryStore {
	if now == nil {
		now = time.Now
	}
	return &InMemoryStore{
		records: make(map[composite]Record),
		now:     now,
	}
}

// Get implements [Store.Get].
func (s *InMemoryStore) Get(_ context.Context, callerID string, key uuid.UUID, bodyHash [32]byte) (Record, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	r, ok := s.records[composite{caller: callerID, key: key}]
	if !ok {
		return Record{}, nil
	}
	if !s.now().Before(r.ExpiresAt) {
		return Record{}, nil
	}
	if r.BodyHash != bodyHash {
		return Record{}, fmt.Errorf("%w: caller %s key %s", ErrBodyMismatch, callerID, key)
	}
	return r, nil
}

// Put implements [Store.Put].
func (s *InMemoryStore) Put(_ context.Context, r Record) error {
	if err := validateRecord(r); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.records[composite{caller: r.CallerID, key: r.Key}] = r
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

// ----- PostgresStore -------------------------------------------------

// PostgresStore persists idempotency records in app.command_idempotency
// (created in migration 20260507000001 + extended for caller scoping in
// 20260507000007). Backed by pgxpool — works under [pg.Transactor]'s
// SET LOCAL semantics but does NOT require a tenant scope (the table
// lives in the app schema with no RLS).
//
// Concurrency: INSERT ... ON CONFLICT DO UPDATE handles the race where
// two requests with the same (caller, key) arrive concurrently — the
// loser silently overwrites with the winner's response. Both clients
// see the same final stored record on subsequent replays.
type PostgresStore struct {
	pool *pgxpool.Pool
}

// NewPostgresStore wires a PostgresStore against the platform pgxpool.
func NewPostgresStore(pool *pgxpool.Pool) *PostgresStore {
	if pool == nil {
		panic("idempotency: PostgresStore requires non-nil pool")
	}
	return &PostgresStore{pool: pool}
}

const pgGetIdempotencyRecord = `
SELECT body_hash, response_status, response_body, response_headers,
       created_at, expires_at
FROM   app.command_idempotency
WHERE  caller_id  = $1
  AND  command_id = $2
`

const pgPutIdempotencyRecord = `
INSERT INTO app.command_idempotency (
    caller_id, command_id, body_hash, response_status, response_body,
    response_headers, created_at, expires_at
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
ON CONFLICT (caller_id, command_id) DO UPDATE SET
    body_hash        = EXCLUDED.body_hash,
    response_status  = EXCLUDED.response_status,
    response_body    = EXCLUDED.response_body,
    response_headers = EXCLUDED.response_headers,
    created_at       = EXCLUDED.created_at,
    expires_at       = EXCLUDED.expires_at
`

const pgPurgeIdempotency = `
DELETE FROM app.command_idempotency
WHERE       expires_at <= $1
`

// Get implements [Store.Get].
func (s *PostgresStore) Get(ctx context.Context, callerID string, key uuid.UUID, bodyHash [32]byte) (Record, error) {
	if callerID == "" {
		return Record{}, fmt.Errorf("%w: caller required", ErrInvalid)
	}
	row := s.pool.QueryRow(ctx, pgGetIdempotencyRecord, callerID, key.String())
	var (
		bodyHashHex   string
		respStatus    int32
		respBody      []byte
		respHeadersJB []byte
		createdAt     time.Time
		expiresAt     time.Time
	)
	if err := row.Scan(&bodyHashHex, &respStatus, &respBody, &respHeadersJB, &createdAt, &expiresAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Record{}, nil
		}
		return Record{}, fmt.Errorf("idempotency: pg get: %w", err)
	}
	// Lazy expiry — same semantics as InMemoryStore.
	if !time.Now().UTC().Before(expiresAt) {
		return Record{}, nil
	}
	storedHash, err := decodeBodyHash(bodyHashHex)
	if err != nil {
		return Record{}, fmt.Errorf("idempotency: pg get hash decode: %w", err)
	}
	if storedHash != bodyHash {
		return Record{}, fmt.Errorf("%w: caller %s key %s", ErrBodyMismatch, callerID, key)
	}
	headers := map[string]string{}
	if len(respHeadersJB) > 0 {
		if err := json.Unmarshal(respHeadersJB, &headers); err != nil {
			return Record{}, fmt.Errorf("idempotency: pg get headers decode: %w", err)
		}
	}
	return Record{
		CallerID:        callerID,
		Key:             key,
		BodyHash:        storedHash,
		ResponseStatus:  int(respStatus),
		ResponseBody:    respBody,
		ResponseHeaders: headers,
		CreatedAt:       createdAt,
		ExpiresAt:       expiresAt,
	}, nil
}

// Put implements [Store.Put].
func (s *PostgresStore) Put(ctx context.Context, r Record) error {
	if err := validateRecord(r); err != nil {
		return err
	}
	headersJSON, err := json.Marshal(r.ResponseHeaders)
	if err != nil {
		return fmt.Errorf("idempotency: pg put headers encode: %w", err)
	}
	if _, err := s.pool.Exec(ctx, pgPutIdempotencyRecord,
		r.CallerID,
		r.Key.String(),
		hex.EncodeToString(r.BodyHash[:]),
		// validateRecord bounds ResponseStatus to [100, 599] per RFC 9110,
		// so the int→int32 cast cannot overflow.
		int32(r.ResponseStatus), //nolint:gosec // G115: bounded by validateRecord
		r.ResponseBody,
		headersJSON,
		r.CreatedAt,
		r.ExpiresAt,
	); err != nil {
		return fmt.Errorf("idempotency: pg put: %w", err)
	}
	return nil
}

// Purge implements [Store.Purge].
func (s *PostgresStore) Purge(ctx context.Context, now time.Time) (int, error) {
	tag, err := s.pool.Exec(ctx, pgPurgeIdempotency, now)
	if err != nil {
		return 0, fmt.Errorf("idempotency: pg purge: %w", err)
	}
	return int(tag.RowsAffected()), nil
}

// validateRecord enforces invariants both stores require.
func validateRecord(r Record) error {
	if r.Key == uuid.Nil {
		return fmt.Errorf("%w: nil key", ErrInvalid)
	}
	if r.CallerID == "" {
		return fmt.Errorf("%w: empty caller", ErrInvalid)
	}
	if len(r.CallerID) > MaxCallerIDLen {
		return fmt.Errorf("%w: caller too long (%d > %d)", ErrInvalid, len(r.CallerID), MaxCallerIDLen)
	}
	if r.ExpiresAt.IsZero() {
		return fmt.Errorf("%w: ExpiresAt required", ErrInvalid)
	}
	// RFC 9110 §15: status codes are 1xx–5xx (100–599). Reject anything
	// outside that range — both rules out programmer errors and proves
	// the int→int32 cast in PostgresStore.Put cannot overflow.
	if r.ResponseStatus < 100 || r.ResponseStatus > 599 {
		return fmt.Errorf("%w: ResponseStatus %d outside RFC 9110 range", ErrInvalid, r.ResponseStatus)
	}
	return nil
}

func decodeBodyHash(s string) ([32]byte, error) {
	var out [32]byte
	raw, err := hex.DecodeString(s)
	if err != nil {
		return out, err
	}
	if len(raw) != 32 {
		return out, fmt.Errorf("expected 32 bytes, got %d", len(raw))
	}
	copy(out[:], raw)
	return out, nil
}
