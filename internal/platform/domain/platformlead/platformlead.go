// Package platformlead defines the PlatformLead aggregate — a verified
// lead exposed in the marketplace + purchasable by a tenant per BRD §6.2
// + ADR 0059.
//
// State machine: Available (sold_to_tenant_id NULL) → Sold (terminal).
// All BRD §5 form fields live on the contained [leadform.Form] VO and
// are snapshotted at verification (never re-edited).
//
// Per ADR 0059, the table's RLS posture allows cross-tenant SELECT for
// unsold rows; writes (create + purchase UPDATE) are platform-only.
package platformlead

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/leadkart/leadkart-go/internal/platform/domain/leadform"
	"github.com/leadkart/leadkart-go/internal/platform/domain/unverifiedcontact"
)

// ErrInvalid is the sentinel for invariant violations.
var ErrInvalid = errors.New("platformlead: invalid")

// ErrAlreadySold is returned when [Purchase] runs against a lead that
// has already been sold to a different tenant. Handler maps to HTTP 409.
var ErrAlreadySold = errors.New("platformlead: already sold")

// ID is the lead primary key (UUIDv7 string).
type ID string

// IsZero reports whether i is unset.
func (i ID) IsZero() bool { return i == "" }

// String returns the underlying UUID string.
func (i ID) String() string { return string(i) }

// TenantID is the FK to identity.tenants.id — opaque from this module.
type TenantID string

// IsZero reports whether t is unset.
func (t TenantID) IsZero() bool { return t == "" }

// String returns the underlying UUID string.
func (t TenantID) String() string { return string(t) }

// PlatformLead is the aggregate root.
type PlatformLead struct {
	id                     ID
	sourceContactID        unverifiedcontact.ID
	form                   leadform.Form
	gstVerified            bool      // BRD §4.3 filter — set by external GST API in Phase 2
	soldToTenantID         TenantID  // empty until sold
	soldAt                 time.Time // zero until sold
	soldToMembershipID     unverifiedcontact.MembershipID
	amountPaisa            int64 // 0 until sold
	verifiedAt             time.Time
	verifiedByMembershipID unverifiedcontact.MembershipID
	createdAt              time.Time

	events []Event
}

// NewFromUnverifiedContact constructs a brand-new PlatformLead from a
// freshly-verified contact's snapshot data + the verifying agent's
// membership. Called inside the UnverifiedContact.MarkVerified flow's
// handler — the same UoW tx writes both aggregates.
func NewFromUnverifiedContact(
	id ID,
	sourceContactID unverifiedcontact.ID,
	form leadform.Form,
	verifiedBy unverifiedcontact.MembershipID,
	now time.Time,
) (*PlatformLead, error) {
	if id.IsZero() {
		return nil, fmt.Errorf("%w: id required", ErrInvalid)
	}
	if sourceContactID.IsZero() {
		return nil, fmt.Errorf("%w: sourceContactID required", ErrInvalid)
	}
	if verifiedBy.IsZero() {
		return nil, fmt.Errorf("%w: verifiedBy required", ErrInvalid)
	}
	if now.IsZero() {
		return nil, fmt.Errorf("%w: now required", ErrInvalid)
	}
	if strings.TrimSpace(form.ContactName()) == "" {
		return nil, fmt.Errorf("%w: form must be populated", ErrInvalid)
	}
	l := &PlatformLead{
		id:                     id,
		sourceContactID:        sourceContactID,
		form:                   form,
		verifiedAt:             now,
		verifiedByMembershipID: verifiedBy,
		createdAt:              now,
	}
	l.recordEvent(VerifiedEvent{
		PlatformLeadID:         id,
		VerifiedAt:             now,
		VerifiedByMembershipID: verifiedBy,
	})
	return l, nil
}

// Snapshot is the persistence-layer DTO consumed by [UnmarshalFromDB].
type Snapshot struct {
	ID                     ID
	SourceContactID        unverifiedcontact.ID
	Form                   leadform.Form
	GstVerified            bool
	SoldToTenantID         TenantID
	SoldAt                 time.Time
	SoldToMembershipID     unverifiedcontact.MembershipID
	AmountPaisa            int64
	VerifiedAt             time.Time
	VerifiedByMembershipID unverifiedcontact.MembershipID
	CreatedAt              time.Time
}

// UnmarshalFromDB rehydrates the aggregate without re-validating.
func UnmarshalFromDB(s Snapshot) *PlatformLead {
	return &PlatformLead{
		id:                     s.ID,
		sourceContactID:        s.SourceContactID,
		form:                   s.Form,
		gstVerified:            s.GstVerified,
		soldToTenantID:         s.SoldToTenantID,
		soldAt:                 s.SoldAt,
		soldToMembershipID:     s.SoldToMembershipID,
		amountPaisa:            s.AmountPaisa,
		verifiedAt:             s.VerifiedAt,
		verifiedByMembershipID: s.VerifiedByMembershipID,
		createdAt:              s.CreatedAt,
	}
}

// ----- Getters --------------------------------------------------------------

// ID returns the lead primary key.
func (l *PlatformLead) ID() ID { return l.id }

// SourceContactID returns the originating UnverifiedContact FK.
func (l *PlatformLead) SourceContactID() unverifiedcontact.ID { return l.sourceContactID }

// Form returns the snapshotted BRD §5 lead-form VO.
func (l *PlatformLead) Form() leadform.Form { return l.form }

// GstVerified returns whether the external GST status check has run.
// false in Slice 1 (Phase 2 enhancement).
func (l *PlatformLead) GstVerified() bool { return l.gstVerified }

// IsAvailable reports whether the lead is unsold (visible in the
// marketplace).
func (l *PlatformLead) IsAvailable() bool { return l.soldToTenantID.IsZero() }

// SoldToTenantID returns the purchasing tenant; zero if unsold.
func (l *PlatformLead) SoldToTenantID() TenantID { return l.soldToTenantID }

// SoldAt returns the purchase timestamp; zero if unsold.
func (l *PlatformLead) SoldAt() time.Time { return l.soldAt }

// SoldToMembershipID returns the purchasing member; zero if unsold.
func (l *PlatformLead) SoldToMembershipID() unverifiedcontact.MembershipID {
	return l.soldToMembershipID
}

// AmountPaisa returns the price paid in INR paise; zero if unsold.
func (l *PlatformLead) AmountPaisa() int64 { return l.amountPaisa }

// VerifiedAt returns the verification timestamp.
func (l *PlatformLead) VerifiedAt() time.Time { return l.verifiedAt }

// VerifiedByMembershipID returns the verifying Lead Agent.
func (l *PlatformLead) VerifiedByMembershipID() unverifiedcontact.MembershipID {
	return l.verifiedByMembershipID
}

// CreatedAt returns the row-creation timestamp (== VerifiedAt in
// Slice 1; separate column for future "draft then publish" flow).
func (l *PlatformLead) CreatedAt() time.Time { return l.createdAt }

// ----- State transitions ----------------------------------------------------

// Purchase transitions an Available lead to Sold. amountPaisa is the
// price the purchasing tenant paid — captured here for forensic + audit
// queries (the actual LeadCredit balance debit happens on a sibling
// aggregate inside the same UoW tx).
//
// Returns [ErrAlreadySold] when called on a sold lead.
func (l *PlatformLead) Purchase(
	tenantID TenantID,
	purchasingMembershipID unverifiedcontact.MembershipID,
	amountPaisa int64,
	now time.Time,
) error {
	if tenantID.IsZero() {
		return fmt.Errorf("%w: tenantID required", ErrInvalid)
	}
	if purchasingMembershipID.IsZero() {
		return fmt.Errorf("%w: purchasingMembershipID required", ErrInvalid)
	}
	if amountPaisa <= 0 {
		return fmt.Errorf("%w: amountPaisa must be positive (got %d)", ErrInvalid, amountPaisa)
	}
	if !l.soldToTenantID.IsZero() {
		// Idempotent only when re-purchase by the same tenant + same
		// price (handles retry inside the purchase handler).
		if l.soldToTenantID == tenantID && l.amountPaisa == amountPaisa {
			return nil
		}
		return ErrAlreadySold
	}
	l.soldToTenantID = tenantID
	l.soldAt = now
	l.soldToMembershipID = purchasingMembershipID
	l.amountPaisa = amountPaisa
	l.recordEvent(PurchasedEvent{
		PlatformLeadID:          l.id,
		TenantID:                tenantID,
		PurchasedAt:             now,
		PurchasedByMembershipID: purchasingMembershipID,
		AmountPaisa:             amountPaisa,
	})
	return nil
}

// ----- Events --------------------------------------------------------------

// PullEvents drains + returns the recorded domain events.
func (l *PlatformLead) PullEvents() []Event {
	if len(l.events) == 0 {
		return nil
	}
	out := l.events
	l.events = nil
	return out
}

func (l *PlatformLead) recordEvent(e Event) {
	l.events = append(l.events, e)
}

// Event is the sealed marker interface.
type Event interface{ isPlatformLeadEvent() }

// VerifiedEvent fires on [NewFromUnverifiedContact] — every fresh lead.
type VerifiedEvent struct {
	PlatformLeadID         ID
	VerifiedAt             time.Time
	VerifiedByMembershipID unverifiedcontact.MembershipID
}

func (VerifiedEvent) isPlatformLeadEvent() {}

// PurchasedEvent fires on the Available → Sold transition.
type PurchasedEvent struct {
	PlatformLeadID          ID
	TenantID                TenantID
	PurchasedAt             time.Time
	PurchasedByMembershipID unverifiedcontact.MembershipID
	AmountPaisa             int64
}

func (PurchasedEvent) isPlatformLeadEvent() {}
