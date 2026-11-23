// Package sellability resolves a company's expiry rule for one moment: the
// business day (the date at the company's UTC offset), the stop-before months
// and the ONE SQL bound every stock query compares expiry_date to.
//
// It reads the policy through the Inventory replica, which joins the caller's
// transaction when ctx carries one (a second pooled connection taken while a
// transaction is open is the pool-deadlock shape).
package sellability

import (
	"context"
	"fmt"
	"time"

	"github.com/leadkart/leadkart-go/internal/common/commercepolicy"
	"github.com/leadkart/leadkart-go/internal/common/expiry"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)

// PolicyReader is the read port of the Inventory policy replica (the same shape
// as query.PolicyReader; one adapter satisfies both).
type PolicyReader interface {
	ReadPolicy(ctx context.Context, tenantID tenant.ID) (commercepolicy.Versioned, error)
}

// Window is the expiry rule resolved for one instant.
type Window struct {
	// Today is the business day: the date at the company's UTC offset (midnight
	// UTC, a date-only value).
	Today time.Time
	// StopMonthsBefore is the company's expiry.stop_months_before.
	StopMonthsBefore int
	// OffsetMinutes is the company's expiry.business_day_utc_offset_minutes.
	OffsetMinutes int
	// WarningDays is the company's expiry warning tiers (descending).
	WarningDays []int
	// MinSellable is the one SQL bound: a batch is sellable exactly when
	// expiry_date >= MinSellable.
	MinSellable time.Time
}

// IsSellable reports whether a batch with this stored expiry may be sold today.
func (w Window) IsSellable(expiryDate time.Time) bool {
	return expiry.IsSellable(expiryDate, w.Today, w.StopMonthsBefore)
}

// IsPastPrintedExpiry reports whether the printed expiry month is over today.
func (w Window) IsPastPrintedExpiry(expiryDate time.Time) bool {
	return expiry.IsPastPrintedExpiry(expiryDate, w.Today)
}

// LastSellableDay is the last day a batch with this expiry may be sold.
func (w Window) LastSellableDay(expiryDate time.Time) time.Time {
	return expiry.LastSellableDay(expiryDate, w.StopMonthsBefore)
}

// DaysLeft is the days from today to the last sellable day (negative once past).
func (w Window) DaysLeft(expiryDate time.Time) int {
	return expiry.DaysLeft(expiryDate, w.Today, w.StopMonthsBefore)
}

// FromPolicy resolves the window for an instant under a policy.
func FromPolicy(p commercepolicy.Policy, now time.Time) Window {
	off := p.Expiry.BusinessDayUTCOffsetMinutes
	n := p.Expiry.StopMonthsBefore
	today := expiry.BusinessDay(now, off)
	return Window{
		Today: today, StopMonthsBefore: n, OffsetMinutes: off,
		WarningDays: append([]int{}, p.Expiry.WarningDays...),
		MinSellable: expiry.MinSellableExpiry(today, n),
	}
}

// Resolver reads the policy and resolves the window.
type Resolver struct {
	policy PolicyReader
	now    func() time.Time
}

// NewResolver wires the resolver. now nil means time.Now.
func NewResolver(policy PolicyReader, now func() time.Time) Resolver {
	if policy == nil {
		panic("sellability: NewResolver policy required")
	}
	if now == nil {
		now = time.Now
	}
	return Resolver{policy: policy, now: now}
}

// Resolve returns the tenant's window at the resolver's clock.
func (r Resolver) Resolve(ctx context.Context, tenantID tenant.ID) (Window, error) {
	v, err := r.policy.ReadPolicy(ctx, tenantID)
	if err != nil {
		return Window{}, fmt.Errorf("sellability: read policy: %w", err)
	}
	return FromPolicy(v.Policy, r.now()), nil
}
