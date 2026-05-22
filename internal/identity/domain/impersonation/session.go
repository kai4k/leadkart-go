// Package impersonation provides the platform-operator impersonation
// session primitives — Session value type + Store interface +
// validation constants.
//
// Per ADR 0051 (Wave 9.1b): MOVED from `internal/common/impersonation/`
// to `internal/identity/domain/impersonation/` because the surface is
// Identity-only (consumed by CreateImpersonationSession / End /
// ListSessions handlers exclusively). TDL single-module rule: domain
// types + ports live with the consuming module.
//
// Per LeadKart .NET `multi-tenancy.md` "Impersonation": session-based
// flow modelled on AWS IAM AssumeRole + Stripe Connect Stripe-Account
// + GitHub Enterprise "Sign in as" + Salesforce Login As. Reason is
// captured ONCE at session creation, not per-request — the
// X-Impersonation-Session-Id header carries the session into each
// downstream request.
//
// The Store implementation hot-path is Redis in production
// (TTL-evicted); v0.2 ships an in-memory adapter at
// `internal/identity/adapters/impersonation_inmemory_store.go`
// suitable for single-process deployments + integration tests.
package impersonation

import (
	"context"
	"errors"
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

// NewSession constructs a fresh impersonation session. The session ID
// is a UUIDv7 (locality + ordering for audit-trail lookups).
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

// UnmarshalSession rehydrates a [Session] from persisted fields —
// used by adapters loading from Redis / in-memory store. Direct
// construction skips invariant validation; callers MUST source from
// trusted storage.
func UnmarshalSession(id, operatorID, targetTenantID, reason string, createdAt, expiresAt time.Time) Session {
	return Session{
		id:             id,
		operatorID:     operatorID,
		targetTenantID: targetTenantID,
		reason:         reason,
		createdAt:      createdAt.UTC(),
		expiresAt:      expiresAt.UTC(),
	}
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
// implementation at
// `internal/identity/adapters/impersonation_inmemory_store.go` for
// single-process deployments + tests.
type Store interface {
	// Put persists a fresh session.
	Put(ctx context.Context, s Session) error

	// Get retrieves a session by ID, or [ErrSessionNotFound] if
	// missing or expired. Implementations MUST treat expired sessions
	// as absent (no separate "expired" branch).
	Get(ctx context.Context, id string) (Session, error)

	// Delete removes a session. Idempotent — deleting a non-existent
	// session returns nil.
	Delete(ctx context.Context, id string) error

	// ListByOperator returns every active (non-expired) session for
	// the supplied operator. Used by GET .../impersonation/sessions —
	// operators see their OWN sessions only.
	ListByOperator(ctx context.Context, operatorID string) ([]Session, error)
}
