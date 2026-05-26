package query_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/leadkart/leadkart-go/internal/identity/adapters"
	"github.com/leadkart/leadkart-go/internal/identity/app/query"
	"github.com/leadkart/leadkart-go/internal/identity/domain/impersonation"
)

func TestNewListImpersonationSessionsHandler_PanicsOnNilStore(t *testing.T) {
	t.Parallel()
	defer func() {
		if r := recover(); r == nil {
			t.Error("expected panic")
		}
	}()
	_ = query.NewListImpersonationSessionsHandler(nil) // arch-test:ignore-err - test fixture setup
}

func TestListImpersonationSessions_RejectsEmptyOperator(t *testing.T) {
	t.Parallel()
	store := adapters.NewImpersonationInMemoryStore(func() time.Time { return testNow })
	h := query.NewListImpersonationSessionsHandler(store)
	_, err := h.Handle(t.Context(), query.ListImpersonationSessionsQuery{})
	if err == nil {
		t.Fatal("expected error on empty operator id")
	}
}

func TestListImpersonationSessions_EmptyStoreReturnsEmpty(t *testing.T) {
	t.Parallel()
	store := adapters.NewImpersonationInMemoryStore(func() time.Time { return testNow })
	h := query.NewListImpersonationSessionsHandler(store)
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
	h := query.NewListImpersonationSessionsHandler(store)
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
	h := query.NewListImpersonationSessionsHandler(store)
	_, err := h.Handle(t.Context(), query.ListImpersonationSessionsQuery{OperatorID: "op-1"})
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want sentinel", err)
	}
}
