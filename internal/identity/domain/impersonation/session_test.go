package impersonation_test

import (
	"testing"
	"time"

	"github.com/leadkart/leadkart-go/internal/identity/domain/impersonation"
)

var fixedNow = time.Date(2026, 5, 24, 12, 0, 0, 0, time.UTC)

func TestNewSession_RejectsShortReason(t *testing.T) {
	t.Parallel()
	_, err := impersonation.NewSession("op-1", "tenant-1", "short", 0, fixedNow)
	if err == nil {
		t.Fatal("expected error on short reason")
	}
}

func TestNewSession_RejectsExcessiveDuration(t *testing.T) {
	t.Parallel()
	_, err := impersonation.NewSession("op-1", "tenant-1",
		"diagnostic: investigating tenant outage 2026-05-07",
		5*time.Hour, fixedNow)
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

// nonUTCNow constructs a wall-clock in IST (UTC+5:30) so we can prove
// the ctor + Unmarshal both normalise to UTC.
func nonUTCNow() time.Time {
	loc, _ := time.LoadLocation("Asia/Kolkata") // arch-test:ignore-err - test fixture
	return time.Date(2026, 5, 24, 17, 30, 0, 0, loc)
}

func TestNewSession_HappyPath_PopulatesFields_AndNormalisesUTC(t *testing.T) {
	t.Parallel()
	reason := "diagnostic: investigating customer outage now"
	dur := 45 * time.Minute
	s, err := impersonation.NewSession("op-7", "tenant-9", reason, dur, nonUTCNow())
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	if s.ID() == "" {
		t.Error("ID empty — expected UUIDv7")
	}
	if s.OperatorID() != "op-7" {
		t.Errorf("OperatorID = %q, want op-7", s.OperatorID())
	}
	if s.TargetTenantID() != "tenant-9" {
		t.Errorf("TargetTenantID = %q, want tenant-9", s.TargetTenantID())
	}
	if s.Reason() != reason {
		t.Errorf("Reason = %q, want %q", s.Reason(), reason)
	}
	if s.CreatedAt().Location() != time.UTC {
		t.Errorf("CreatedAt location = %v, want UTC", s.CreatedAt().Location())
	}
	if s.ExpiresAt().Location() != time.UTC {
		t.Errorf("ExpiresAt location = %v, want UTC", s.ExpiresAt().Location())
	}
	if !s.ExpiresAt().Equal(s.CreatedAt().Add(dur)) {
		t.Errorf("ExpiresAt = %v, want CreatedAt+%v", s.ExpiresAt(), dur)
	}
}

func TestNewSession_RejectsEmptyOperatorID(t *testing.T) {
	t.Parallel()
	_, err := impersonation.NewSession("", "tenant-1",
		"diagnostic: legitimate audit reason here", 0, fixedNow)
	if err == nil {
		t.Fatal("expected error on empty operatorID")
	}
}

func TestNewSession_RejectsEmptyTargetTenantID(t *testing.T) {
	t.Parallel()
	_, err := impersonation.NewSession("op-1", "",
		"diagnostic: legitimate audit reason here", 0, fixedNow)
	if err == nil {
		t.Fatal("expected error on empty targetTenantID")
	}
}

func TestNewSession_DurationAtMaxBoundary_Accepted(t *testing.T) {
	t.Parallel()
	// MaxDuration is the inclusive upper bound (`duration > MaxDuration`
	// fails). Equal to MaxDuration MUST succeed.
	s, err := impersonation.NewSession("op-1", "tenant-1",
		"diagnostic: legitimate audit reason here",
		impersonation.MaxDuration, fixedNow)
	if err != nil {
		t.Fatalf("MaxDuration exact: %v", err)
	}
	want := fixedNow.Add(impersonation.MaxDuration)
	if !s.ExpiresAt().Equal(want) {
		t.Errorf("ExpiresAt = %v, want %v", s.ExpiresAt(), want)
	}
}

func TestUnmarshalSession_RoundTripsEveryField_AndNormalisesUTC(t *testing.T) {
	t.Parallel()
	createdAt := nonUTCNow()
	expiresAt := createdAt.Add(time.Hour)
	s := impersonation.UnmarshalSession(
		"sess-id-1",
		"op-2",
		"tenant-3",
		"a fully valid stored reason here",
		createdAt,
		expiresAt,
	)
	if s.ID() != "sess-id-1" {
		t.Errorf("ID = %q", s.ID())
	}
	if s.OperatorID() != "op-2" {
		t.Errorf("OperatorID = %q", s.OperatorID())
	}
	if s.TargetTenantID() != "tenant-3" {
		t.Errorf("TargetTenantID = %q", s.TargetTenantID())
	}
	if s.Reason() != "a fully valid stored reason here" {
		t.Errorf("Reason = %q", s.Reason())
	}
	if s.CreatedAt().Location() != time.UTC {
		t.Errorf("CreatedAt location = %v, want UTC", s.CreatedAt().Location())
	}
	if s.ExpiresAt().Location() != time.UTC {
		t.Errorf("ExpiresAt location = %v, want UTC", s.ExpiresAt().Location())
	}
	if !s.CreatedAt().Equal(createdAt.UTC()) {
		t.Errorf("CreatedAt = %v, want %v", s.CreatedAt(), createdAt.UTC())
	}
	if !s.ExpiresAt().Equal(expiresAt.UTC()) {
		t.Errorf("ExpiresAt = %v, want %v", s.ExpiresAt(), expiresAt.UTC())
	}
}

func TestSession_IsExpired_TruthTable(t *testing.T) {
	t.Parallel()
	created := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	dur := time.Hour
	s, err := impersonation.NewSession("op-1", "tenant-1",
		"diagnostic: legitimate audit reason here", dur, created)
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	expires := s.ExpiresAt()

	tests := []struct {
		name string
		now  time.Time
		want bool
	}{
		{"now before expiresAt", expires.Add(-time.Nanosecond), false},
		{"now equal to expiresAt", expires, true},
		{"now after expiresAt", expires.Add(time.Nanosecond), true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := s.IsExpired(tc.now); got != tc.want {
				t.Errorf("IsExpired(%v) = %v, want %v", tc.now, got, tc.want)
			}
		})
	}
}
