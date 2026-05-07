package impersonation_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/leadkart/leadkart-go/internal/platform/impersonation"
)

func TestNewSession_RejectsShortReason(t *testing.T) {
	t.Parallel()
	_, err := impersonation.NewSession("op-1", "tenant-1", "short", 0, time.Now())
	if err == nil {
		t.Fatal("expected error on short reason")
	}
}

func TestNewSession_RejectsExcessiveDuration(t *testing.T) {
	t.Parallel()
	_, err := impersonation.NewSession("op-1", "tenant-1",
		"diagnostic: investigating tenant outage 2026-05-07",
		5*time.Hour, time.Now())
	if err == nil {
		t.Fatal("expected error on duration > 4h")
	}
}

func TestNewSession_AppliesDefaultDuration(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 5, 7, 12, 0, 0, 0, time.UTC)
	s, err := impersonation.NewSession("op-1", "tenant-1",
		"diagnostic: legitimate audit reason here",
		0, now)
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	want := now.Add(impersonation.DefaultDuration)
	if !s.ExpiresAt().Equal(want) {
		t.Errorf("ExpiresAt = %v, want %v", s.ExpiresAt(), want)
	}
}

func TestInMemoryStore_PutGet_RoundTrip(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 5, 7, 12, 0, 0, 0, time.UTC)
	clock := now
	store := impersonation.NewInMemoryStore(func() time.Time { return clock })
	sess, err := impersonation.NewSession("op-1", "tenant-1",
		"audit: investigating ticket TICKET-1234",
		30*time.Minute, now)
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	if err := store.Put(context.Background(), sess); err != nil {
		t.Fatalf("Put: %v", err)
	}
	got, err := store.Get(context.Background(), sess.ID())
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.ID() != sess.ID() {
		t.Errorf("ID mismatch: %q vs %q", got.ID(), sess.ID())
	}
}

func TestInMemoryStore_Get_ExpiredSession_TreatedAsAbsent(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 5, 7, 12, 0, 0, 0, time.UTC)
	clock := now
	store := impersonation.NewInMemoryStore(func() time.Time { return clock })
	sess, err := impersonation.NewSession("op-1", "tenant-1",
		"diagnostic: legitimate audit reason here",
		1*time.Minute, now)
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	_ = store.Put(context.Background(), sess)

	// Advance past expiry.
	clock = now.Add(2 * time.Minute)

	_, err = store.Get(context.Background(), sess.ID())
	if !errors.Is(err, impersonation.ErrSessionNotFound) {
		t.Fatalf("err = %v, want ErrSessionNotFound", err)
	}
}

func TestInMemoryStore_Delete_Idempotent(t *testing.T) {
	t.Parallel()
	store := impersonation.NewInMemoryStore(time.Now)
	if err := store.Delete(context.Background(), "non-existent"); err != nil {
		t.Errorf("Delete on absent: %v", err)
	}
}

func TestInMemoryStore_ListByOperator_FiltersByOwner(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 5, 7, 12, 0, 0, 0, time.UTC)
	store := impersonation.NewInMemoryStore(func() time.Time { return now })
	mine, _ := impersonation.NewSession("op-1", "tenant-1",
		"audit: investigating ticket TICKET-1", 30*time.Minute, now)
	other, _ := impersonation.NewSession("op-2", "tenant-1",
		"audit: investigating ticket TICKET-2", 30*time.Minute, now)
	_ = store.Put(context.Background(), mine)
	_ = store.Put(context.Background(), other)

	got, err := store.ListByOperator(context.Background(), "op-1")
	if err != nil {
		t.Fatalf("ListByOperator: %v", err)
	}
	if len(got) != 1 || got[0].ID() != mine.ID() {
		t.Errorf("expected 1 session for op-1, got %v", got)
	}
}
