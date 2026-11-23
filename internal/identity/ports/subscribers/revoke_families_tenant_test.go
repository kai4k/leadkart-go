package subscribers_test

import (
	"io"
	"log/slog"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/leadkart/leadkart-go/internal/common/ids"
	"github.com/leadkart/leadkart-go/internal/identity/domain/person"
	"github.com/leadkart/leadkart-go/internal/identity/domain/refreshtoken"
	"github.com/leadkart/leadkart-go/internal/identity/domain/refreshtoken/refreshtokentest"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
	"github.com/leadkart/leadkart-go/internal/identity/integrationevents"
	"github.com/leadkart/leadkart-go/internal/identity/ports/subscribers"
)

var tenantCascadeNow = time.Date(2026, 11, 20, 9, 0, 0, 0, time.UTC)

func seedTenantFamily(t *testing.T, repo *refreshtokentest.FakeRepository, tid tenant.ID, n int) *refreshtoken.Family {
	t.Helper()
	hash, err := refreshtoken.NewTokenHash(strings.Repeat("a", 60) + leftPad4(n))
	if err != nil {
		t.Fatal(err)
	}
	f, err := refreshtoken.NewFamily(
		refreshtoken.FamilyID(ids.NewV7().String()), person.ID(ids.NewV7().String()), tid,
		"device", hash, time.Hour, tenantCascadeNow.Add(time.Duration(n)*time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.Add(t.Context(), f); err != nil {
		t.Fatal(err)
	}
	return f
}

func leftPad4(n int) string {
	s := strconv.Itoa(n)
	return strings.Repeat("0", 4-len(s)) + s
}

// A tenant suspension ends every active family of that tenant (more than one page of
// them), leaves the other tenants' families alone, and is idempotent on redelivery.
func TestRevokeFamilies_TenantSuspended_RevokesEveryActiveFamilyOfThatTenantOnly(t *testing.T) {
	t.Parallel()
	repo := refreshtokentest.NewFakeRepository()
	suspended := tenant.ID(uuid.NewString())
	other := tenant.ID(uuid.NewString())

	const mine = 1001 // two pages of 500 and a remainder
	for i := range mine {
		seedTenantFamily(t, repo, suspended, i)
	}
	survivor := seedTenantFamily(t, repo, other, mine+1)

	h := subscribers.NewRevokeFamiliesOnSecurityChange(repo, slog.New(slog.NewTextHandler(io.Discard, nil)), func() time.Time { return tenantCascadeNow })
	evt := &integrationevents.TenantSuspendedV1{TenantID: uuid.MustParse(suspended.String()), Reason: "payment overdue", OccurredAtUTC: tenantCascadeNow}
	if err := h.HandleTenantSuspended(t.Context(), evt); err != nil {
		t.Fatalf("HandleTenantSuspended: %v", err)
	}

	left, err := repo.ListActiveForTenant(t.Context(), suspended, 5000)
	if err != nil || len(left) != 0 {
		t.Fatalf("%d active families left for the suspended tenant (%v), want 0", len(left), err)
	}
	if active, _ := repo.ListActiveForTenant(t.Context(), other, 10); len(active) != 1 || active[0].ID() != survivor.ID() {
		t.Fatalf("another tenant's family was touched: %d active", len(active))
	}
	some, _ := repo.GetByID(t.Context(), survivor.ID())
	if some.IsRevoked() {
		t.Fatal("survivor revoked")
	}

	// Redelivery (the outbox is at-least-once): a no-op, no error.
	if err := h.HandleTenantSuspended(t.Context(), evt); err != nil {
		t.Fatalf("redelivery: %v", err)
	}
}

func TestRevokeFamilies_TenantSuspended_RecordsTheReason(t *testing.T) {
	t.Parallel()
	repo := refreshtokentest.NewFakeRepository()
	tid := tenant.ID(uuid.NewString())
	f := seedTenantFamily(t, repo, tid, 1)
	h := subscribers.NewRevokeFamiliesOnSecurityChange(repo, slog.New(slog.NewTextHandler(io.Discard, nil)), func() time.Time { return tenantCascadeNow })
	if err := h.HandleTenantSuspended(t.Context(), &integrationevents.TenantSuspendedV1{TenantID: uuid.MustParse(tid.String())}); err != nil {
		t.Fatal(err)
	}
	got, err := repo.GetByID(t.Context(), f.ID())
	if err != nil || !got.IsRevoked() || got.RevokeReason() != "tenant_suspended" {
		t.Fatalf("revoked=%v reason=%q err=%v, want tenant_suspended", got.IsRevoked(), got.RevokeReason(), err)
	}
}
