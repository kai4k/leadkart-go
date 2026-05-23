package integrationevents

import (
	"time"

	"github.com/google/uuid"
)

// LeadCreditAdjustedV1 — a per-tenant credit balance changed (topup
// from operator action or charge from marketplace purchase).
// TenantScoped: the tenant whose balance changed.
//
// DeltaCredits is SIGNED — positive on topup, negative on charge.
// NewBalanceCredits is the post-adjustment balance. Consumers (tenant
// dashboard, audit indexer) consume both for forensic reconstruction.
type LeadCreditAdjustedV1 struct {
	TenantIDValue          uuid.UUID `json:"tenant_id"`
	AdjustmentID           uuid.UUID `json:"adjustment_id"`
	DeltaCredits           int64     `json:"delta_credits"`
	NewBalanceCredits      int64     `json:"new_balance_credits"`
	Reason                 string    `json:"reason"`
	AdjustedAt             time.Time `json:"adjusted_at"`
	AdjustedByMembershipID uuid.UUID `json:"adjusted_by_membership_id"`
}

// Topic returns the canonical wire alias.
func (LeadCreditAdjustedV1) Topic() string { return "platform.lead_credit_adjusted.v1" }

// OccurredAt returns the domain timestamp.
func (e LeadCreditAdjustedV1) OccurredAt() time.Time { return e.AdjustedAt }

// TenantID satisfies [TenantScoped].
func (e LeadCreditAdjustedV1) TenantID() uuid.UUID { return e.TenantIDValue }

var (
	_ TenantScoped = LeadCreditAdjustedV1{}
	_              = register(LeadCreditAdjustedV1{})
)
