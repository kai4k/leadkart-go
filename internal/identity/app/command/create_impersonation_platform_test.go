package command_test

import (
	"errors"
	"testing"
	"time"

	"github.com/leadkart/leadkart-go/internal/common/email"
	"github.com/leadkart/leadkart-go/internal/common/slug"
	"github.com/leadkart/leadkart-go/internal/identity/adapters"
	"github.com/leadkart/leadkart-go/internal/identity/app/command"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)

// G2o B1: LeadKart staff never impersonate inside LeadKart's own tenant; they act there directly.
func TestCreateImpersonationSession_RefusesThePlatformTenant(t *testing.T) {
	t.Parallel()
	tenants := newFakeTenantRepo()
	s, err := slug.New(tenant.PlatformSlug)
	if err != nil {
		t.Fatalf("slug.New: %v", err)
	}
	addr, _ := email.New("owner@leadkart.test")
	tn, err := tenant.New(tenant.ID("22222222-2222-2222-2222-222222222222"), s, "LeadKart", "LeadKart", addr, testNow)
	if err != nil {
		t.Fatalf("tenant.New: %v", err)
	}
	if err := tn.Activate(testNow); err != nil {
		t.Fatalf("Activate: %v", err)
	}
	_ = tenants.Add(t.Context(), tn) // arch-test:ignore-err - test fixture setup
	store := adapters.NewImpersonationInMemoryStore(func() time.Time { return testNow })
	h := command.NewCreateImpersonationSessionHandler(store, tenants, newImpersonationIssuer(t), func() time.Time { return testNow })

	res, err := h.Handle(t.Context(), command.CreateImpersonationSessionCommand{
		OperatorID:            "11111111-1111-1111-1111-111111111111",
		OperatorSecurityStamp: "stamp-current",
		TargetTenantID:        tn.ID(),
		Reason:                "investigate-stuck-onboarding-ticket",
		Duration:              30 * time.Minute,
	})
	if !errors.Is(err, command.ErrImpersonationInvalid) {
		t.Fatalf("err = %v, want ErrImpersonationInvalid", err)
	}
	if res.AccessToken != "" || res.SessionID != "" {
		t.Fatalf("a refused session minted %+v", res)
	}
}
