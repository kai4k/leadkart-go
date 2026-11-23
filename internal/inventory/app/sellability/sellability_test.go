package sellability_test

import (
	"testing"
	"time"

	"github.com/leadkart/leadkart-go/internal/common/commercepolicy"
	"github.com/leadkart/leadkart-go/internal/inventory/app/sellability"
	"github.com/leadkart/leadkart-go/internal/inventory/app/sellability/sellabilitytest"
)

func TestResolve_TheBusinessDayFollowsTheCompanyOffsetNotUTC(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	// 18:30 UTC on 28 Feb is 00:00 IST on 1 Mar: the default (market) offset flips the day there.
	r := sellabilitytest.Resolver(time.Date(2027, 2, 28, 18, 30, 0, 0, time.UTC))
	w, err := r.Resolve(ctx, "t")
	if err != nil {
		t.Fatal(err)
	}
	if !w.Today.Equal(time.Date(2027, 3, 1, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("business day = %s, want 2027-03-01", w.Today.Format(time.DateOnly))
	}
	// A company in UTC keeps 28 Feb at the same instant: the offset is company data.
	p := commercepolicy.Default()
	p.Expiry.BusinessDayUTCOffsetMinutes = 0
	utc := sellabilitytest.ResolverWith(p, time.Date(2027, 2, 28, 18, 30, 0, 0, time.UTC))
	w2, _ := utc.Resolve(ctx, "t")
	if !w2.Today.Equal(time.Date(2027, 2, 28, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("UTC company business day = %s", w2.Today.Format(time.DateOnly))
	}
}

func TestWindow_SellabilityAndTheOneBoundAgree(t *testing.T) {
	t.Parallel()
	w := sellability.FromPolicy(commercepolicy.Default(), time.Date(2027, 2, 28, 12, 0, 0, 0, time.UTC)) // stop 1 month before
	printedMarch := time.Date(2027, 3, 31, 0, 0, 0, 0, time.UTC)
	if !w.IsSellable(printedMarch) || w.IsPastPrintedExpiry(printedMarch) {
		t.Fatal("EXP 03/2027 is sellable on 28 Feb with the default stop")
	}
	if w.LastSellableDay(printedMarch).Format(time.DateOnly) != "2027-02-28" || w.DaysLeft(printedMarch) != 0 {
		t.Fatalf("last sellable day %s days left %d", w.LastSellableDay(printedMarch).Format(time.DateOnly), w.DaysLeft(printedMarch))
	}
	if (printedMarch.Before(w.MinSellable)) == w.IsSellable(printedMarch) {
		t.Fatal("the SQL bound disagrees with IsSellable")
	}
}
