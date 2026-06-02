// Package platformlead defines the PlatformLead aggregate: a verified lead
// sold to a tenant via the marketplace (BRD §6.2, ADR 0059).
//
// State machine: Available (sold_to_tenant_id NULL) → Sold (terminal).
// BRD §5 form fields live on the [leadform.Form] VO, snapshotted at
// verification and never re-edited.
//
// Per ADR 0059, RLS allows cross-tenant SELECT on unsold rows; writes
// (create + purchase UPDATE) are platform-only.
package platformlead

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/leadkart/leadkart-go/internal/platform/domain/leadform"
	"github.com/leadkart/leadkart-go/internal/platform/domain/unverifiedcontact"
)

// ErrInvalid is the sentinel for invariant violations.
var ErrInvalid = errors.New("platformlead: invalid")

// ErrAlreadyPurchased is returned by [RecordPurchase] when the buying tenant
// already holds a purchase row for this lead (no double-buy). Handler maps to
// HTTP 409. Mirrors the UNIQUE(lead_id, tenant_id) 23505 the adapter
// translates.
var ErrAlreadyPurchased = errors.New("platformlead: already purchased by tenant")

// ErrSoldOut is returned by [RecordPurchase] when the lead's purchase count
// has reached its effective sale limit. Handler maps to HTTP 409.
var ErrSoldOut = errors.New("platformlead: sold out (sale limit reached)")

// Tier is the marketplace pricing/eligibility band of a lead (ADR 0065). The
// per-tier default sale limit + base price live in the platform.lead_tiers
// config table; tenant-eligibility enforcement is deferred.
type Tier string

// Tier values — the closed set mirrored by the lead_tiers CHECK constraint.
const (
	TierStandard Tier = "standard"
	TierPriority Tier = "priority"
	TierPremium  Tier = "premium"
)

// IsValid reports whether t is one of the known tiers.
func (t Tier) IsValid() bool {
	switch t {
	case TierStandard, TierPriority, TierPremium:
		return true
	default:
		return false
	}
}

// String returns the underlying tier code.
func (t Tier) String() string { return string(t) }

// TierConfig is the per-tier marketplace config loaded from
// platform.lead_tiers — the default sale limit + base price the
// PurchaseLead handler feeds into pricing + the limit invariant.
type TierConfig struct {
	Code             Tier
	DefaultSaleLimit int
	BasePricePaisa   int64
}

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

// PlatformLead is the aggregate root. A verified lead is inventory resold to
// several tenants up to a sale limit (ADR 0065) — not a one-off sale. The
// buyers slice is the consistency boundary for the sale-limit + no-double-buy
// invariants enforced by [RecordPurchase].
type PlatformLead struct {
	id                     ID
	sourceContactID        unverifiedcontact.ID
	form                   leadform.Form
	gstVerified            bool       // BRD §4.3 filter; set by external GST API in Phase 2
	tier                   Tier       // pricing/eligibility band (ADR 0065)
	saleLimit              *int       // per-lead override; nil = use tier default
	buyers                 []TenantID // tenants that already hold a purchase row
	verifiedAt             time.Time
	verifiedByMembershipID unverifiedcontact.MembershipID
	createdAt              time.Time

	pendingPurchases []*LeadPurchase // recorded this session; drained by the repo
	events           []Event
}

// LeadPurchase is one buyer's purchase row (ADR 0065). Immutable record of
// what that tenant was charged. Created by [RecordPurchase]; persisted by the
// repository, which drains [PlatformLead.PullPendingPurchases].
type LeadPurchase struct {
	id           string
	leadID       ID
	tenantID     TenantID
	membershipID unverifiedcontact.MembershipID
	amountPaisa  int64
	purchasedAt  time.Time
}

// ID returns the purchase primary key (UUIDv7 string).
func (p *LeadPurchase) ID() string { return p.id }

// LeadID returns the parent lead.
func (p *LeadPurchase) LeadID() ID { return p.leadID }

// TenantID returns the buyer tenant.
func (p *LeadPurchase) TenantID() TenantID { return p.tenantID }

// MembershipID returns the buying member.
func (p *LeadPurchase) MembershipID() unverifiedcontact.MembershipID { return p.membershipID }

// AmountPaisa returns the price this buyer was charged.
func (p *LeadPurchase) AmountPaisa() int64 { return p.amountPaisa }

// PurchasedAt returns the purchase timestamp.
func (p *LeadPurchase) PurchasedAt() time.Time { return p.purchasedAt }

// NewFromUnverifiedContact constructs a PlatformLead from a freshly-verified
// contact's snapshot and the verifying agent's membership. Called in the
// UnverifiedContact.MarkVerified handler; same UoW tx writes both aggregates.
func NewFromUnverifiedContact(
	id ID,
	sourceContactID unverifiedcontact.ID,
	form leadform.Form,
	verifiedBy unverifiedcontact.MembershipID,
	now time.Time,
) (*PlatformLead, error) {
	if err := validateUUID("id", id.String()); err != nil {
		return nil, err
	}
	if err := validateUUID("sourceContactID", sourceContactID.String()); err != nil {
		return nil, err
	}
	if err := validateUUID("verifiedBy", verifiedBy.String()); err != nil {
		return nil, err
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
		tier:                   TierStandard, // default band; eligibility/tier assignment deferred (ADR 0065)
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

// validateUUID enforces the H6 reviewer rule: every domain ID must parse
// as a UUID at AGGREGATE-CONSTRUCTION time, not later at the adapter
// boundary. Trims surrounding whitespace before parsing.
func validateUUID(name, val string) error {
	val = strings.TrimSpace(val)
	if val == "" {
		return fmt.Errorf("%w: %s required", ErrInvalid, name)
	}
	if _, err := uuid.Parse(val); err != nil {
		return fmt.Errorf("%w: %s not a valid uuid", ErrInvalid, name)
	}
	return nil
}

// Snapshot is the persistence-layer DTO consumed by [UnmarshalFromDB].
// BuyerTenantIDs is hydrated only on the purchase path (the repo loads the
// buyer set under a row lock so RecordPurchase enforces the sale limit
// race-free); plain reads/browse leave it nil.
type Snapshot struct {
	ID                     ID
	SourceContactID        unverifiedcontact.ID
	Form                   leadform.Form
	GstVerified            bool
	Tier                   Tier
	SaleLimit              *int
	BuyerTenantIDs         []TenantID
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
		tier:                   s.Tier,
		saleLimit:              s.SaleLimit,
		buyers:                 s.BuyerTenantIDs,
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

// GstVerified reports whether the external GST check has run. Always false
// in Slice 1 (Phase 2 enhancement).
func (l *PlatformLead) GstVerified() bool { return l.gstVerified }

// Tier returns the lead's marketplace tier.
func (l *PlatformLead) Tier() Tier { return l.tier }

// SaleLimit returns the per-lead sale-limit override, or nil when the lead
// falls back to its tier's default limit.
func (l *PlatformLead) SaleLimit() *int { return l.saleLimit }

// PurchaseCount returns how many tenants have bought this lead (the loaded
// buyer set). Meaningful only when the aggregate was hydrated with buyers
// (the purchase path); plain reads return 0.
func (l *PlatformLead) PurchaseCount() int { return len(l.buyers) }

// EffectiveSaleLimit resolves the sale limit: the per-lead override if set,
// else the supplied tier default (coalesce, per ADR 0065).
func (l *PlatformLead) EffectiveSaleLimit(tierDefaultLimit int) int {
	if l.saleLimit != nil {
		return *l.saleLimit
	}
	return tierDefaultLimit
}

// IsAvailable reports whether the lead is still openly listed given the
// supplied tier default limit (purchase count below the effective limit).
func (l *PlatformLead) IsAvailable(tierDefaultLimit int) bool {
	return l.PurchaseCount() < l.EffectiveSaleLimit(tierDefaultLimit)
}

// BuyerTenantIDs returns a copy of the loaded buyer set (the tenants that
// already hold a purchase row). Used by test fakes to deep-copy state.
func (l *PlatformLead) BuyerTenantIDs() []TenantID {
	if len(l.buyers) == 0 {
		return nil
	}
	out := make([]TenantID, len(l.buyers))
	copy(out, l.buyers)
	return out
}

// HasBuyer reports whether the given tenant already holds a purchase row.
func (l *PlatformLead) HasBuyer(tenantID TenantID) bool {
	for _, b := range l.buyers {
		if b == tenantID {
			return true
		}
	}
	return false
}

// VerifiedAt returns the verification timestamp.
func (l *PlatformLead) VerifiedAt() time.Time { return l.verifiedAt }

// VerifiedByMembershipID returns the verifying Lead Agent.
func (l *PlatformLead) VerifiedByMembershipID() unverifiedcontact.MembershipID {
	return l.verifiedByMembershipID
}

// CreatedAt returns the row-creation timestamp (== VerifiedAt in Slice 1;
// separate column for a future draft-then-publish flow).
func (l *PlatformLead) CreatedAt() time.Time { return l.createdAt }

// ----- State transitions ----------------------------------------------------

// RecordPurchase adds one buyer to the lead (ADR 0065 multi-buyer). It
// enforces the two invariants that make this the lead's consistency boundary:
// no-double-buy (the tenant must not already hold a purchase row) and the
// sale limit (purchase count must stay below the effective limit). The
// caller supplies the resolved purchase price (amountPaisa, computed by the
// pricing service) and the tier's default sale limit (for the coalesce).
//
// The new LeadPurchase is appended to pendingPurchases for the repository to
// INSERT; the LeadCredit debit happens on a sibling aggregate in the same UoW
// tx. The buyer set must have been hydrated under a row lock (the repo's
// UpdateByID locks the lead) so the count check is race-free.
func (l *PlatformLead) RecordPurchase(
	purchaseID string,
	tenantID TenantID,
	purchasingMembershipID unverifiedcontact.MembershipID,
	amountPaisa int64,
	tierDefaultLimit int,
	now time.Time,
) error {
	if err := validateUUID("purchaseID", purchaseID); err != nil {
		return err
	}
	if err := validateUUID("tenantID", tenantID.String()); err != nil {
		return err
	}
	if err := validateUUID("purchasingMembershipID", purchasingMembershipID.String()); err != nil {
		return err
	}
	if amountPaisa <= 0 {
		return fmt.Errorf("%w: amountPaisa must be positive (got %d)", ErrInvalid, amountPaisa)
	}
	if tierDefaultLimit <= 0 {
		return fmt.Errorf("%w: tierDefaultLimit must be positive (got %d)", ErrInvalid, tierDefaultLimit)
	}
	if l.HasBuyer(tenantID) {
		return ErrAlreadyPurchased
	}
	if l.PurchaseCount() >= l.EffectiveSaleLimit(tierDefaultLimit) {
		return ErrSoldOut
	}
	lp := &LeadPurchase{
		id:           purchaseID,
		leadID:       l.id,
		tenantID:     tenantID,
		membershipID: purchasingMembershipID,
		amountPaisa:  amountPaisa,
		purchasedAt:  now,
	}
	l.buyers = append(l.buyers, tenantID)
	l.pendingPurchases = append(l.pendingPurchases, lp)
	l.recordEvent(PurchasedEvent{
		PlatformLeadID:          l.id,
		PurchaseID:              purchaseID,
		TenantID:                tenantID,
		PurchasedAt:             now,
		PurchasedByMembershipID: purchasingMembershipID,
		AmountPaisa:             amountPaisa,
	})
	return nil
}

// PullPendingPurchases drains the purchases recorded this session so the
// repository can INSERT them. Mirrors PullEvents.
func (l *PlatformLead) PullPendingPurchases() []*LeadPurchase {
	if len(l.pendingPurchases) == 0 {
		return nil
	}
	out := l.pendingPurchases
	l.pendingPurchases = nil
	return out
}

// ----- Events --------------------------------------------------------------

// PullEvents drains and returns the recorded domain events.
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

// PurchasedEvent fires on each [RecordPurchase] (one per buyer). Suppressed
// by the integration-event mapper — the handler emits LeadPurchasedV1
// directly with the lead snapshot (ADR 0065 per-purchase event).
type PurchasedEvent struct {
	PlatformLeadID          ID
	PurchaseID              string
	TenantID                TenantID
	PurchasedAt             time.Time
	PurchasedByMembershipID unverifiedcontact.MembershipID
	AmountPaisa             int64
}

func (PurchasedEvent) isPlatformLeadEvent() {}
