package query_test

import (
	"context"
	"errors"
	"testing"

	"github.com/leadkart/leadkart-go/internal/common/ids"
	"github.com/leadkart/leadkart-go/internal/platform/app/query"
	"github.com/leadkart/leadkart-go/internal/platform/domain/leadcredit"
	"github.com/leadkart/leadkart-go/internal/platform/platformtest"
)

// TestGetLeadCreditBalance_HappyPath — handler returns the live
// balance projection. C2 — review-pass.
func TestGetLeadCreditBalance_HappyPath(t *testing.T) {
	t.Parallel()

	credits := platformtest.NewFakeLeadCreditRepository()
	tenantID := leadcredit.TenantID(ids.NewV7().String())

	// Seed: a tenant with 250 credits.
	c, err := leadcredit.NewForTenant(tenantID, qNow())
	if err != nil {
		t.Fatalf("ctor: %v", err)
	}
	op := leadcredit.MembershipID(ids.NewV7().String())
	if err := c.Topup(250, "seed", op, qNow()); err != nil {
		t.Fatalf("topup: %v", err)
	}
	if err := credits.UpsertWithVersion(context.Background(), c); err != nil {
		t.Fatalf("persist: %v", err)
	}

	h := query.NewGetLeadCreditBalanceHandler(credits)
	out, err := h.Handle(context.Background(), query.GetLeadCreditBalanceQuery{
		TenantID: tenantID,
	})
	if err != nil {
		t.Fatalf("handle: %v", err)
	}
	if out.Balance != 250 {
		t.Errorf("Balance=%d want 250", out.Balance)
	}
	if out.TenantID != tenantID.String() {
		t.Errorf("TenantID=%q want %q", out.TenantID, tenantID)
	}
}

// TestGetLeadCreditBalance_NoRowYet_TypedSentinel — the typed sentinel
// the HTTP layer maps to 200 + zero balance. C2.
func TestGetLeadCreditBalance_NoRowYet_TypedSentinel(t *testing.T) {
	t.Parallel()

	credits := platformtest.NewFakeLeadCreditRepository()
	h := query.NewGetLeadCreditBalanceHandler(credits)

	_, err := h.Handle(context.Background(), query.GetLeadCreditBalanceQuery{
		TenantID: leadcredit.TenantID(ids.NewV7().String()),
	})
	if !errors.Is(err, query.ErrCreditRowNotFound) {
		t.Fatalf("expected ErrCreditRowNotFound, got %v", err)
	}
}
