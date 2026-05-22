// Package impersonation provides the platform-operator impersonation
// session primitives — value type + storage contract.
//
// Per LeadKart .NET `multi-tenancy.md` "Impersonation": session-based
// flow modelled on AWS IAM AssumeRole + Stripe Connect Stripe-Account
// + GitHub Enterprise "Sign in as" + Salesforce Login As. Reason is
// captured ONCE at session creation, not per-request — the
// X-Impersonation-Session-Id header carries the session into each
// downstream request.
//
// The hot-path store is Redis in production (TTL-evicted); v0.2
// ships an in-memory implementation suitable for single-process
// deployments + integration tests. The persistent audit record
// lives on a separate table (buildingblocks.admin_impersonation_
// audit) — this package owns ONLY the runtime session.
package impersonation

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/leadkart/leadkart-go/internal/common/ids"
)

// ----- Session value type ---------------------------------------------------

// Session captures one operator's impersonation grant against one
// tenant. Immutable post-construction.
//
// Lifetime: bounded by ExpiresAt. The store automatically evicts
// expired sessions on lookup; explicit cleanup is unnecessary.
type Session struct {
	id             string
	operatorID     string
	targetTenantID string
	reason         string
	createdAt      time.Time
	expiresAt      time.Time
}

// ----- Sentinels ------------------------------------------------------------

// ErrSessionNotFound is returned by [Store.Get] when no matching
// session exists OR the session has expired (treated as absent).
var ErrSessionNotFound = errors.New("impersonation: session not found")

// ErrInvalidSession surfaces from [NewSession] on malformed input
// (missing operator/target/reason, reason too short, duration out of
// bounds).
var ErrInvalidSession = errors.New("impersonation: invalid session")

// ----- Construction --------------------------------------------------------

// MinReasonLength matches the .NET "≥10 chars" canon — short reasons
// like "test" don't satisfy the audit requirement under DPDP §12 +
// SOC2 CC4.1.
const MinReasonLength = 10

// MaxDuration caps session length at 4 hours per Salesforce Login As
// + Stripe Connect canon — long-lived impersonation sessions are an
// anti-pattern (operator forgets, attacker gets unbounded window).
const MaxDuration = 4 * time.Hour

// DefaultDuration is the recommended starting point — 30 minutes is
// long enough for diagnostic work + short enough to limit blast
// radius if an operator abandons the session.
const DefaultDuration = 30 * time.Minute

// NewSession constructs a fresh impersonation session. The session
// ID is a UUIDv7 (locality + ordering for audit-trail lookups).
//
// duration <= 0 is treated as DefaultDuration. duration > MaxDuration
// is rejected.
func NewSession(operatorID, targetTenantID, reason string, duration time.Duration, now time.Time) (Session, error) {
	if operatorID == "" {
		return Session{}, errors.New("impersonation: operator id required")
	}
	if targetTenantID == "" {
		return Session{}, errors.New("impersonation: target tenant id required")
	}
	if len(reason) < MinReasonLength {
		return Session{}, errors.New("impersonation: reason too short — DPDP / SOC2 audit minimum 10 chars")
	}
	if duration <= 0 {
		duration = DefaultDuration
	}
	if duration > MaxDuration {
		return Session{}, errors.New("impersonation: duration exceeds max (4h)")
	}
	return Session{
		id:             ids.NewV7().String(),
		operatorID:     operatorID,
		targetTenantID: targetTenantID,
		reason:         reason,
		createdAt:      now.UTC(),
		expiresAt:      now.UTC().Add(duration),
	}, nil
}

// ID returns the session UUIDv7.
func (s Session) ID() string { return s.id }

// OperatorID returns the operator's PersonID.
func (s Session) OperatorID() string { return s.operatorID }

// TargetTenantID returns the tenant the operator is acting as.
func (s Session) TargetTenantID() string { return s.targetTenantID }

// Reason returns the audit reason supplied at session creation.
func (s Session) Reason() string { return s.reason }

// CreatedAt returns the creation timestamp (UTC).
func (s Session) CreatedAt() time.Time { return s.createdAt }

// ExpiresAt returns the absolute expiry timestamp (UTC).
func (s Session) ExpiresAt() time.Time { return s.expiresAt }

// IsExpired reports whether the session has aged past its TTL.
func (s Session) IsExpired(now time.Time) bool {
	return !now.Before(s.expiresAt)
}

// ----- Store contract -------------------------------------------------------

// Store is the impersonation-session persistence contract. Production
// adapter is Redis-backed (TTL-evicted); v0.2 ships an in-memory
// implementation suitable for single-process deployments + tests.
type Store interface {
	// Put persists a fresh session. Returns the session unchanged for
	// caller-side handler ergonomics (write + return shape).
	Put(ctx context.Context, s Session) error

	// Get retrieves a session by ID, or [ErrSessionNotFound] if
	// missing or expired. Implementations MUST treat expired sessions
	// as absent (no separate "expired" branch).
	Get(ctx context.Context, id string) (Session, error)

	// Delete removes a session. Idempotent — deleting a non-existent
	// session returns nil.
	Delete(ctx context.Context, id string) error

	// ListByOperator returns every active (non-expired) session for
	// the supplied operator. Used by GET .../impersonation/sessions
	// — operators see their OWN sessions only.
	ListByOperator(ctx context.Context, operatorID string) ([]Session, error)
}

// ----- InMemoryStore --------------------------------------------------------

// InMemoryStore is a process-local map-backed [Store]. Adequate for
// single-instance deployments + integration tests; production
// multi-replica needs a Redis-backed implementation.
//
// The store auto-evicts expired sessions inside Get/ListByOperator
// — no separate sweep goroutine needed.
type InMemoryStore struct {
	mu       sync.Mutex
	sessions map[string]Session
	now      func() time.Time
}

// NewInMemoryStore constructs an empty store. now is the clock
// source; pass time.Now in production, a fixed clock in tests.
func NewInMemoryStore(now func() time.Time) *InMemoryStore {
	if now == nil {
		now = time.Now
	}
	return &InMemoryStore{
		sessions: make(map[string]Session),
		now:      now,
	}
}

// Put satisfies [Store].
func (s *InMemoryStore) Put(_ context.Context, sess Session) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sessions[sess.ID()] = sess
	return nil
}

// Get satisfies [Store]. Expired sessions are auto-evicted + return
// ErrSessionNotFound.
func (s *InMemoryStore) Get(_ context.Context, id string) (Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.sessions[id]
	if !ok {
		return Session{}, ErrSessionNotFound
	}
	if sess.IsExpired(s.now()) {
		delete(s.sessions, id)
		return Session{}, ErrSessionNotFound
	}
	return sess, nil
}

// Delete satisfies [Store]. Idempotent.
func (s *InMemoryStore) Delete(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.sessions, id)
	return nil
}

// ListByOperator satisfies [Store]. Skips + evicts expired sessions
// it encounters.
func (s *InMemoryStore) ListByOperator(_ context.Context, operatorID string) ([]Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	out := make([]Session, 0, len(s.sessions))
	for id, sess := range s.sessions {
		if sess.IsExpired(now) {
			delete(s.sessions, id)
			continue
		}
		if sess.OperatorID() == operatorID {
			out = append(out, sess)
		}
	}
	return out, nil
}

// Compile-time assertion: *InMemoryStore satisfies [Store].
var _ Store = (*InMemoryStore)(nil)
