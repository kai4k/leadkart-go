package adapters_test

import (
	"errors"
	"testing"
	"time"

	"github.com/leadkart/leadkart-go/internal/identity/adapters"
	"github.com/leadkart/leadkart-go/internal/identity/domain/impersonation"
)

func TestImpersonationInMemoryStore_PutGet_RoundTrip(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 5, 7, 12, 0, 0, 0, time.UTC)
	clock := now
	store := adapters.NewImpersonationInMemoryStore(func() time.Time { return clock })
	sess, err := impersonation.NewSession("op-1", "tenant-1",
		"audit: investigating ticket TICKET-1234",
		30*time.Minute, now)
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	if err := store.Put(t.Context(), sess); err != nil {
		t.Fatalf("Put: %v", err)
	}
	got, err := store.Get(t.Context(), sess.ID())
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.ID() != sess.ID() {
		t.Errorf("ID mismatch: %q vs %q", got.ID(), sess.ID())
	}
}

func TestImpersonationInMemoryStore_Get_ExpiredSession_TreatedAsAbsent(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 5, 7, 12, 0, 0, 0, time.UTC)
	clock := now
	store := adapters.NewImpersonationInMemoryStore(func() time.Time { return clock })
	sess, err := impersonation.NewSession("op-1", "tenant-1",
		"diagnostic: legitimate audit reason here",
		1*time.Minute, now)
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	_ = store.Put(t.Context(), sess)

	// Advance past expiry.
	clock = now.Add(2 * time.Minute)

	_, err = store.Get(t.Context(), sess.ID())
	if !errors.Is(err, impersonation.ErrSessionNotFound) {
		t.Fatalf("err = %v, want ErrSessionNotFound", err)
	}
}

func TestImpersonationInMemoryStore_Delete_Idempotent(t *testing.T) {
	t.Parallel()
	store := adapters.NewImpersonationInMemoryStore(time.Now)
	if err := store.Delete(t.Context(), "non-existent"); err != nil {
		t.Errorf("Delete on absent: %v", err)
	}
}

func TestImpersonationInMemoryStore_ListByOperator_FiltersByOwner(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 5, 7, 12, 0, 0, 0, time.UTC)
	store := adapters.NewImpersonationInMemoryStore(func() time.Time { return now })
	mine, _ := impersonation.NewSession("op-1", "tenant-1",
		"audit: investigating ticket TICKET-1", 30*time.Minute, now)
	other, _ := impersonation.NewSession("op-2", "tenant-1",
		"audit: investigating ticket TICKET-2", 30*time.Minute, now)
	_ = store.Put(t.Context(), mine)
	_ = store.Put(t.Context(), other)

	got, err := store.ListByOperator(t.Context(), "op-1")
	if err != nil {
		t.Fatalf("ListByOperator: %v", err)
	}
	if len(got) != 1 || got[0].ID() != mine.ID() {
		t.Errorf("expected 1 session for op-1, got %v", got)
	}
}
