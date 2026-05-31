package integrationevents

import (
	"time"
)

// LeadCreditAdjustedV1 signals a per-tenant credit balance change (operator
// topup or marketplace-purchase charge). TenantScoped to the affected tenant.
//
// UUID fields are wire-shaped strings per ADR 0059. DeltaCredits is signed
// (positive topup, negative charge); NewBalanceCredits is the post-adjustment
// balance. Consumers use both for forensic reconstruction.
type LeadCreditAdjustedV1 struct {
	TenantID               string    `json:"tenant_id"`
	AdjustmentID           string    `json:"adjustment_id"`
	DeltaCredits           int64     `json:"delta_credits"`
	NewBalanceCredits      int64     `json:"new_balance_credits"`
	Reason                 string    `json:"reason"`
	AdjustedAt             time.Time `json:"adjusted_at"`
	AdjustedByMembershipID string    `json:"adjusted_by_membership_id"`
}

// Topic returns the wire alias.
func (LeadCreditAdjustedV1) Topic() string { return "platform.lead_credit_adjusted.v1" }

// OccurredAt returns the domain timestamp.
func (e LeadCreditAdjustedV1) OccurredAt() time.Time { return e.AdjustedAt }

// TenantIDString satisfies [TenantScoped].
func (e LeadCreditAdjustedV1) TenantIDString() string { return e.TenantID }

var (
	_ TenantScoped = LeadCreditAdjustedV1{}
	_              = register(LeadCreditAdjustedV1{})
)
