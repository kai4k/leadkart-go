package impersonation_test

import (
	"testing"
	"time"

	"github.com/leadkart/leadkart-go/internal/identity/domain/impersonation"
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
