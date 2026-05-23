package integrationevents

import (
	"time"
)

// LeadCreditAdjustedV1 — a per-tenant credit balance changed (topup
// from operator action or charge from marketplace purchase).
// TenantScoped: the tenant whose balance changed.
//
// All UUID fields are wire-shaped as strings per ADR 0059 frozen brief
// (matches CRM subscriber's local mirror without per-language uuid
// codec). DeltaCredits is SIGNED — positive on topup, negative on
// charge. NewBalanceCredits is the post-adjustment balance. Consumers
// (tenant dashboard, audit indexer) consume both for forensic
// reconstruction.
type LeadCreditAdjustedV1 struct {
	TenantID               string    `json:"tenant_id"`
	AdjustmentID           string    `json:"adjustment_id"`
	DeltaCredits           int64     `json:"delta_credits"`
	NewBalanceCredits      int64     `json:"new_balance_credits"`
	Reason                 string    `json:"reason"`
	AdjustedAt             time.Time `json:"adjusted_at"`
	AdjustedByMembershipID string    `json:"adjusted_by_membership_id"`
}

// Topic returns the canonical wire alias.
func (LeadCreditAdjustedV1) Topic() string { return "platform.lead_credit_adjusted.v1" }

// OccurredAt returns the domain timestamp.
func (e LeadCreditAdjustedV1) OccurredAt() time.Time { return e.AdjustedAt }

// TenantIDString satisfies [TenantScoped].
func (e LeadCreditAdjustedV1) TenantIDString() string { return e.TenantID }

var (
	_ TenantScoped = LeadCreditAdjustedV1{}
	_              = register(LeadCreditAdjustedV1{})
)
