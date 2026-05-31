// Package leadcredit defines the LeadCredit aggregate — per-tenant
// purchase-credit balance (BRD §4.2, ADR 0059).
//
// One row per tenant. Optimistic concurrency via [LeadCredit.Version]:
// the repository UPDATEs under `WHERE version = $old`; 0 rows affected =
// conflict, and the command handler retries with backoff (ADR 0059, .NET
// ADR-015). Invariant: balance >= 0; Topup adds, Charge subtracts.
package leadcredit

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// ErrInvalid is the sentinel for invariant violations.
var ErrInvalid = errors.New("leadcredit: invalid")

// ErrInsufficientBalance is returned by [Charge] when amount exceeds the
// balance. Handler maps to HTTP 422.
var ErrInsufficientBalance = errors.New("leadcredit: insufficient balance")

// TenantID is the lead-credit row primary key (matches the tenant row).
type TenantID string

// IsZero reports whether t is unset.
func (t TenantID) IsZero() bool { return t == "" }

// String returns the underlying UUID string.
func (t TenantID) String() string { return string(t) }

// MembershipID is the adjusting operator — FK to
// identity.tenant_memberships.id. Stored as a string to keep the boundary clean.
type MembershipID string

// IsZero reports whether m is unset.
func (m MembershipID) IsZero() bool { return m == "" }

// String returns the underlying UUID string.
func (m MembershipID) String() string { return string(m) }

// LeadCredit is the aggregate root. One row per tenant.
type LeadCredit struct {
	tenantID  TenantID
	balance   int64
	version   int64
	createdAt time.Time
	updatedAt time.Time

	events []Event
}

// NewForTenant constructs a zero-balance LeadCredit row. Created on tenant
// registration (Slice 2 subscriber to identity.TenantRegisteredV1); Slice 1
// creates it on first Topup.
func NewForTenant(tenantID TenantID, now time.Time) (*LeadCredit, error) {
	if tenantID.IsZero() {
		return nil, fmt.Errorf("%w: tenantID required", ErrInvalid)
	}
	if now.IsZero() {
		return nil, fmt.Errorf("%w: now required", ErrInvalid)
	}
	return &LeadCredit{
		tenantID:  tenantID,
		balance:   0,
		version:   0,
		createdAt: now,
		updatedAt: now,
	}, nil
}

// Snapshot is the persistence-layer DTO consumed by [UnmarshalFromDB].
type Snapshot struct {
	TenantID  TenantID
	Balance   int64
	Version   int64
	CreatedAt time.Time
	UpdatedAt time.Time
}

// UnmarshalFromDB rehydrates the aggregate without re-validating.
func UnmarshalFromDB(s Snapshot) *LeadCredit {
	return &LeadCredit{
		tenantID:  s.TenantID,
		balance:   s.Balance,
		version:   s.Version,
		createdAt: s.CreatedAt,
		updatedAt: s.UpdatedAt,
	}
}

// ----- Getters --------------------------------------------------------------

// TenantID returns the tenant FK.
func (l *LeadCredit) TenantID() TenantID { return l.tenantID }

// Balance returns the current credit count.
func (l *LeadCredit) Balance() int64 { return l.balance }

// Version returns the optimistic-concurrency version. The repository reads it
// before the UPDATE, then `WHERE version = $old SET version = $old + 1`.
func (l *LeadCredit) Version() int64 { return l.version }

// CreatedAt returns the row-creation timestamp.
func (l *LeadCredit) CreatedAt() time.Time { return l.createdAt }

// UpdatedAt returns the last-mutation timestamp.
func (l *LeadCredit) UpdatedAt() time.Time { return l.updatedAt }

// ----- State transitions ----------------------------------------------------

// Topup adds credits; delta must be > 0. reason is required for audit: purchases
// are permanent and non-refundable, so the topup record is the only forensic
// anchor (BRD §4.2).
func (l *LeadCredit) Topup(delta int64, reason string, adjustedBy MembershipID, now time.Time) error {
	if delta <= 0 {
		return fmt.Errorf("%w: topup delta must be positive (got %d)", ErrInvalid, delta)
	}
	if strings.TrimSpace(reason) == "" {
		return fmt.Errorf("%w: topup reason required for audit", ErrInvalid)
	}
	if adjustedBy.IsZero() {
		return fmt.Errorf("%w: adjustedBy required", ErrInvalid)
	}
	l.balance += delta
	l.updatedAt = now
	l.recordEvent(AdjustedEvent{
		TenantID:               l.tenantID,
		Delta:                  delta,
		NewBalance:             l.balance,
		Reason:                 reason,
		AdjustedAt:             now,
		AdjustedByMembershipID: adjustedBy,
	})
	return nil
}

// Charge subtracts credits (marketplace-purchase handler). Rejects with
// [ErrInsufficientBalance] when the result would go negative; the balance >= 0
// invariant is also a DB CHECK constraint.
func (l *LeadCredit) Charge(amount int64, reason string, adjustedBy MembershipID, now time.Time) error {
	if amount <= 0 {
		return fmt.Errorf("%w: charge amount must be positive (got %d)", ErrInvalid, amount)
	}
	if strings.TrimSpace(reason) == "" {
		return fmt.Errorf("%w: charge reason required for audit", ErrInvalid)
	}
	if adjustedBy.IsZero() {
		return fmt.Errorf("%w: adjustedBy required", ErrInvalid)
	}
	if l.balance < amount {
		return ErrInsufficientBalance
	}
	l.balance -= amount
	l.updatedAt = now
	l.recordEvent(AdjustedEvent{
		TenantID:               l.tenantID,
		Delta:                  -amount,
		NewBalance:             l.balance,
		Reason:                 reason,
		AdjustedAt:             now,
		AdjustedByMembershipID: adjustedBy,
	})
	return nil
}

// ----- Events --------------------------------------------------------------

// PullEvents drains + returns the recorded domain events.
func (l *LeadCredit) PullEvents() []Event {
	if len(l.events) == 0 {
		return nil
	}
	out := l.events
	l.events = nil
	return out
}

func (l *LeadCredit) recordEvent(e Event) {
	l.events = append(l.events, e)
}

// Event is the sealed marker interface.
type Event interface{ isLeadCreditEvent() }

// AdjustedEvent fires on every Topup and Charge; Delta is signed (+ topup,
// - charge). Consumed by tenant dashboard refresh and audit indexing.
type AdjustedEvent struct {
	TenantID               TenantID
	Delta                  int64 // signed: + on topup, - on charge
	NewBalance             int64
	Reason                 string
	AdjustedAt             time.Time
	AdjustedByMembershipID MembershipID
}

func (AdjustedEvent) isLeadCreditEvent() {}
