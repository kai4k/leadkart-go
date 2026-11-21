package query_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/leadkart/leadkart-go/internal/identity/adapters"
	"github.com/leadkart/leadkart-go/internal/identity/app/query"
	"github.com/leadkart/leadkart-go/internal/identity/domain/impersonation"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant/tenanttest"
)

func TestNewListImpersonationSessionsHandler_PanicsOnNilStore(t *testing.T) {
	t.Parallel()
	defer func() {
		if r := recover(); r == nil {
			t.Error("expected panic")
		}
	}()
	_ = query.NewListImpersonationSessionsHandler(nil, tenanttest.NewFakeRepository()) // arch-test:ignore-err - test fixture setup
}

func TestListImpersonationSessions_RejectsEmptyOperator(t *testing.T) {
	t.Parallel()
	store := adapters.NewImpersonationInMemoryStore(func() time.Time { return testNow })
	h := query.NewListImpersonationSessionsHandler(store, tenanttest.NewFakeRepository())
	_, err := h.Handle(t.Context(), query.ListImpersonationSessionsQuery{})
	if err == nil {
		t.Fatal("expected error on empty operator id")
	}
}

func TestListImpersonationSessions_EmptyStoreReturnsEmpty(t *testing.T) {
	t.Parallel()
	store := adapters.NewImpersonationInMemoryStore(func() time.Time { return testNow })
	h := query.NewListImpersonationSessionsHandler(store, tenanttest.NewFakeRepository())
	got, err := h.Handle(t.Context(), query.ListImpersonationSessionsQuery{OperatorID: "op-1"})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("len = %d, want 0", len(got))
	}
}

func TestListImpersonationSessions_HappyPath_ProjectsAllFields(t *testing.T) {
	t.Parallel()
	store := adapters.NewImpersonationInMemoryStore(func() time.Time { return testNow })
	sess, err := impersonation.NewSession("op-42", "tenant-acme", "diagnose stuck onboarding", 0, testNow)
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	if err := store.Put(t.Context(), sess); err != nil {
		t.Fatalf("Put: %v", err)
	}
	h := query.NewListImpersonationSessionsHandler(store, tenanttest.NewFakeRepository())
	got, err := h.Handle(t.Context(), query.ListImpersonationSessionsQuery{OperatorID: "op-42"})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("len = %d, want 1", len(got))
	}
	v := got[0]
	if v.SessionID != sess.ID() {
		t.Errorf("SessionID = %q, want %q", v.SessionID, sess.ID())
	}
	if v.OperatorID != "op-42" {
		t.Errorf("OperatorID = %q", v.OperatorID)
	}
	if v.TargetTenantID != "tenant-acme" {
		t.Errorf("TargetTenantID = %q", v.TargetTenantID)
	}
	if v.Reason != "diagnose stuck onboarding" {
		t.Errorf("Reason = %q", v.Reason)
	}
	if v.CreatedAt.IsZero() {
		t.Errorf("CreatedAt zero")
	}
	if v.ExpiresAt.IsZero() {
		t.Errorf("ExpiresAt zero")
	}
}

// errStore lets a test surface a generic error from the store.
type errStore struct {
	impersonation.Store
	err error
}

func (s errStore) ListByOperator(_ context.Context, _ string) ([]impersonation.Session, error) {
	return nil, s.err
}

func TestListImpersonationSessions_PropagatesStoreError(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("store boom")
	store := errStore{Store: adapters.NewImpersonationInMemoryStore(func() time.Time { return testNow }), err: sentinel}
	h := query.NewListImpersonationSessionsHandler(store, tenanttest.NewFakeRepository())
	_, err := h.Handle(t.Context(), query.ListImpersonationSessionsQuery{OperatorID: "op-1"})
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want sentinel", err)
	}
}

func TestListImpersonationSessions_EachSessionCarriesItsOwnTenantsCurrentName(t *testing.T) {
	t.Parallel()
	const tenantB = tenant.ID("99999999-9999-9999-9999-999999999999")
	repo := tenanttest.NewFakeRepository()
	a := newTenant(t, testTenantID, "acme-pharma")
	b := newTenant(t, tenantB, "zenith-labs")
	if err := b.UpdateProfile("Zenith Labs Pvt Ltd", "Zenith Labs", testNow); err != nil {
		t.Fatal(err)
	}
	for _, tn := range []*tenant.Tenant{a, b} {
		if err := repo.Add(t.Context(), tn); err != nil {
			t.Fatal(err)
		}
	}
	store := adapters.NewImpersonationInMemoryStore(func() time.Time { return testNow })
	for _, target := range []tenant.ID{testTenantID, tenantB, testTenantID} {
		sess, err := impersonation.NewSession("op-1", target.String(), "diagnose stuck onboarding", 0, testNow)
		if err != nil {
			t.Fatal(err)
		}
		if err := store.Put(t.Context(), sess); err != nil {
			t.Fatal(err)
		}
	}
	h := query.NewListImpersonationSessionsHandler(store, repo)

	check := func(wantA string) {
		t.Helper()
		got, err := h.Handle(t.Context(), query.ListImpersonationSessionsQuery{OperatorID: "op-1"})
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 3 {
			t.Fatalf("len = %d, want 3", len(got))
		}
		for _, v := range got {
			want := wantA
			if v.TargetTenantID == tenantB.String() {
				want = "Zenith Labs"
			}
			if v.TenantDisplayName != want {
				t.Errorf("session for %s: TenantDisplayName = %q, want %q", v.TargetTenantID, v.TenantDisplayName, want)
			}
		}
	}
	check("Acme Pharma")

	// A rename shows on the next read: the name is not frozen at session start.
	if err := repo.UpdateByID(t.Context(), testTenantID, func(tn *tenant.Tenant) (bool, error) {
		return true, tn.UpdateProfile("Acme Pharma Pvt Ltd", "Acme Renamed", testNow)
	}); err != nil {
		t.Fatal(err)
	}
	check("Acme Renamed")
}
