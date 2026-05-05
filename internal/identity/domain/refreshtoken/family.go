// Package refreshtoken defines the RefreshTokenFamily aggregate — session
// lifecycle per RFC 9700 §4.13 (refresh-token rotation + reuse detection).
//
// Architectural context (per LeadKart .NET `security.md` + Auth0/Okta canon):
//
// A Family is a chain of opaque refresh tokens issued to one device session
// for one (Person, Tenant) pair. Each rotation:
//  1. Client presents the current token (hash matches stored).
//  2. Server marks current as consumed, generates a new opaque token.
//  3. Returns new token to client; subsequent calls use the new token.
//
// **Reuse detection** — the security-load-bearing feature. If a previously
// CONSUMED token is presented (e.g. an attacker stole a token, used it once,
// then the legitimate user retried with the now-stale token), the entire
// family is REVOKED. Both attacker AND legitimate user are forced to re-auth.
// This catches replay attacks within the absolute-lifetime window without
// per-token blacklist tracking.
//
// Family is NOT tenant-scoped — refresh tokens are session-management
// infrastructure (Auth0/Okta/Stripe convention). The TenantID is carried as
// a data column for context propagation; cross-tenant lookup of "which
// family does this hash belong to?" is the foundational read pattern.
package refreshtoken

import (
	"fmt"
	"strings"
	"time"

	"github.com/leadkart/leadkart-go/internal/common/clock"
	"github.com/leadkart/leadkart-go/internal/common/errs"
	"github.com/leadkart/leadkart-go/internal/common/ids"
	"github.com/leadkart/leadkart-go/internal/identity/domain/person"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)

// FamilyID is the per-family primary key (UUIDv7).
type FamilyID string

// IsZero reports whether the ID is unset.
func (i FamilyID) IsZero() bool { return i == "" }

// String returns the underlying UUID string.
func (i FamilyID) String() string { return string(i) }

// ----- Sentinel errors ------------------------------------------------------

// ErrUnknownToken is returned by [Family.Rotate] when the presented hash
// matches no token in the family — typical cause is a token from a
// different family being presented (operator error or active attack).
var ErrUnknownToken = errs.New(errs.KindUnauthenticated, "refreshtoken", "token not found in family")

// ErrReuseDetected is returned by [Family.Rotate] when a CONSUMED token is
// presented — the family is automatically revoked as a side effect (RFC
// 9700 §4.13 mandates this).
var ErrReuseDetected = errs.New(errs.KindUnauthenticated, "refreshtoken", "token reuse detected")

// ErrRevoked is returned when rotation is attempted on a revoked family.
var ErrRevoked = errs.New(errs.KindUnauthenticated, "refreshtoken", "family is revoked")

// ErrExpired is returned when the presented token has passed its absolute
// expiry (typically 14 days from family creation).
var ErrExpired = errs.New(errs.KindUnauthenticated, "refreshtoken", "token expired")

// ----- Family aggregate -----------------------------------------------------

// Family is the aggregate root.
//
// Tokens are stored in chain order; index 0 is generation 0 (the first
// issued token). The CURRENT token is the LAST entry whose ConsumedAt is
// zero — there is exactly one such token at any time on a non-revoked
// family.
type Family struct {
	id           FamilyID
	personID     person.ID
	tenantID     tenant.ID
	deviceLabel  string
	createdAt    time.Time
	lastUsedAt   time.Time
	revokedAt    time.Time
	revokeReason string
	tokens       []Token
	events       []Event
}

// NewFamily creates a new family with one token at generation 0.
//
// firstHash is the SHA-256 hex of the opaque token string the auth adapter
// just generated; ttl is the per-token TTL (typically 14 days = 14*24*time.Hour).
//
// Returns [ErrInvalid] (wrapped) on invariant violation. Emits
// [FamilyCreatedEvent].
func NewFamily(
	id FamilyID,
	personID person.ID,
	tenantID tenant.ID,
	deviceLabel string,
	firstHash TokenHash,
	ttl time.Duration,
) (*Family, error) {
	if id.IsZero() {
		return nil, fmt.Errorf("%w: family id required", ErrInvalid)
	}
	if personID.IsZero() {
		return nil, fmt.Errorf("%w: person id required", ErrInvalid)
	}
	if tenantID.IsZero() {
		return nil, fmt.Errorf("%w: tenant id required", ErrInvalid)
	}
	if strings.TrimSpace(deviceLabel) == "" {
		return nil, fmt.Errorf("%w: device label required", ErrInvalid)
	}
	if firstHash.IsZero() {
		return nil, fmt.Errorf("%w: token hash required", ErrInvalid)
	}
	if ttl <= 0 {
		return nil, fmt.Errorf("%w: ttl must be positive (got %v)", ErrInvalid, ttl)
	}

	now := clock.Now()
	first := Token{
		id:         TokenID(ids.NewV7().String()),
		hash:       firstHash,
		generation: 0,
		issuedAt:   now,
		expiresAt:  now.Add(ttl),
	}
	f := &Family{
		id:          id,
		personID:    personID,
		tenantID:    tenantID,
		deviceLabel: strings.TrimSpace(deviceLabel),
		createdAt:   now,
		lastUsedAt:  now,
		tokens:      []Token{first},
	}
	f.recordEvent(FamilyCreatedEvent{
		FamilyID:    id,
		PersonID:    personID,
		TenantID:    tenantID,
		DeviceLabel: f.deviceLabel,
		At:          now,
	})
	return f, nil
}

// FamilySnapshot is the persistence DTO consumed by [UnmarshalFromDB].
type FamilySnapshot struct {
	ID           FamilyID
	PersonID     person.ID
	TenantID     tenant.ID
	DeviceLabel  string
	CreatedAt    time.Time
	LastUsedAt   time.Time
	RevokedAt    time.Time
	RevokeReason string
	Tokens       []TokenSnapshot
}

// UnmarshalFromDB re-hydrates a Family from persistence. Repository-only
// path; does NOT re-validate (TDL canon).
func UnmarshalFromDB(s FamilySnapshot) *Family {
	f := &Family{
		id:           s.ID,
		personID:     s.PersonID,
		tenantID:     s.TenantID,
		deviceLabel:  s.DeviceLabel,
		createdAt:    s.CreatedAt,
		lastUsedAt:   s.LastUsedAt,
		revokedAt:    s.RevokedAt,
		revokeReason: s.RevokeReason,
		tokens:       make([]Token, len(s.Tokens)),
	}
	for i, ts := range s.Tokens {
		f.tokens[i] = Token{
			id:           ts.ID,
			hash:         ts.Hash,
			generation:   ts.Generation,
			issuedAt:     ts.IssuedAt,
			expiresAt:    ts.ExpiresAt,
			consumedAt:   ts.ConsumedAt,
			replacedByID: ts.ReplacedByID,
		}
	}
	return f
}

// ----- Getters --------------------------------------------------------------

// ID returns the family primary key.
func (f *Family) ID() FamilyID { return f.id }

// PersonID returns the FK to [person.Person].
func (f *Family) PersonID() person.ID { return f.personID }

// TenantID returns the active tenant context for this session.
func (f *Family) TenantID() tenant.ID { return f.tenantID }

// DeviceLabel returns the human-readable session label.
func (f *Family) DeviceLabel() string { return f.deviceLabel }

// CreatedAt returns when the family was first issued.
func (f *Family) CreatedAt() time.Time { return f.createdAt }

// LastUsedAt returns the most recent rotation timestamp.
func (f *Family) LastUsedAt() time.Time { return f.lastUsedAt }

// IsRevoked reports whether the family has been revoked (for any reason).
func (f *Family) IsRevoked() bool { return !f.revokedAt.IsZero() }

// RevokedAt returns the revocation timestamp; zero if not revoked.
func (f *Family) RevokedAt() time.Time { return f.revokedAt }

// RevokeReason returns the reason recorded at revocation; empty if active.
func (f *Family) RevokeReason() string { return f.revokeReason }

// CurrentToken returns the un-consumed, un-expired token at the chain head,
// or nil if the family has no current token (revoked, or all consumed).
func (f *Family) CurrentToken() *Token {
	for i := len(f.tokens) - 1; i >= 0; i-- {
		if !f.tokens[i].IsConsumed() {
			t := f.tokens[i]
			return &t
		}
	}
	return nil
}

// AllTokens returns the full chain in generation order. Repository uses
// this to persist the family state.
func (f *Family) AllTokens() []Token {
	out := make([]Token, len(f.tokens))
	copy(out, f.tokens)
	return out
}

// ----- State transitions ----------------------------------------------------

// Rotate consumes the current token (presented by hash match) and issues
// the next generation. Returns:
//
//   - [ErrRevoked]: family already revoked.
//   - [ErrUnknownToken]: presented hash matches no token in this family.
//   - [ErrReuseDetected]: presented hash matches a CONSUMED token. Family
//     is automatically revoked as a side effect; emits [RevokedEvent].
//   - [ErrExpired]: presented token has passed its absolute expiry.
//
// On success, emits [RotatedEvent].
func (f *Family) Rotate(presentedHash TokenHash, newHash TokenHash, ttl time.Duration) error {
	if f.IsRevoked() {
		return fmt.Errorf("%w", ErrRevoked)
	}
	if newHash.IsZero() {
		return fmt.Errorf("%w: new token hash required", ErrInvalid)
	}
	if ttl <= 0 {
		return fmt.Errorf("%w: ttl must be positive", ErrInvalid)
	}

	idx := f.findToken(presentedHash)
	if idx < 0 {
		return fmt.Errorf("%w", ErrUnknownToken)
	}

	if f.tokens[idx].IsConsumed() {
		// REUSE DETECTED — revoke entire family per RFC 9700 §4.13.
		now := clock.Now()
		f.revokedAt = now
		f.revokeReason = "reuse_detected"
		f.recordEvent(RevokedEvent{
			FamilyID: f.id,
			Reason:   "reuse_detected",
			At:       now,
		})
		return fmt.Errorf("%w", ErrReuseDetected)
	}

	now := clock.Now()
	if f.tokens[idx].IsExpired(now) {
		return fmt.Errorf("%w: token expired at %v", ErrExpired, f.tokens[idx].ExpiresAt())
	}

	// Mint next generation.
	nextID := TokenID(ids.NewV7().String())
	next := Token{
		id:         nextID,
		hash:       newHash,
		generation: f.tokens[idx].generation + 1,
		issuedAt:   now,
		expiresAt:  now.Add(ttl),
	}

	// Mark presented token as consumed; cross-link to next.
	f.tokens[idx].consumedAt = now
	f.tokens[idx].replacedByID = nextID

	f.tokens = append(f.tokens, next)
	f.lastUsedAt = now

	f.recordEvent(RotatedEvent{
		FamilyID:           f.id,
		ConsumedTokenID:    f.tokens[idx].id,
		NewTokenID:         nextID,
		NewTokenGeneration: next.generation,
		At:                 now,
	})
	return nil
}

// Revoke terminates the family (e.g. user-initiated logout or admin action).
//
// reason MUST be non-empty (audit requirement). Subsequent [Rotate] calls
// return [ErrRevoked]. Idempotent — second Revoke on already-revoked family
// is no-op.
func (f *Family) Revoke(reason string) error {
	if strings.TrimSpace(reason) == "" {
		return fmt.Errorf("%w: revoke reason required for audit", ErrInvalid)
	}
	if f.IsRevoked() {
		return nil
	}
	now := clock.Now()
	f.revokedAt = now
	f.revokeReason = reason
	f.recordEvent(RevokedEvent{
		FamilyID: f.id,
		Reason:   reason,
		At:       now,
	})
	return nil
}

// ----- Event handling -------------------------------------------------------

// PullEvents drains recorded events.
func (f *Family) PullEvents() []Event {
	if len(f.events) == 0 {
		return nil
	}
	out := f.events
	f.events = nil
	return out
}

func (f *Family) recordEvent(e Event) {
	f.events = append(f.events, e)
}

// findToken returns the index of the token with the given hash, or -1.
func (f *Family) findToken(h TokenHash) int {
	for i, t := range f.tokens {
		if t.hash.Equal(h) {
			return i
		}
	}
	return -1
}
