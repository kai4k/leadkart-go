package indianfy_test

import (
	"errors"
	"testing"
	"time"

	"github.com/leadkart/leadkart-go/internal/common/indianfy"
)

func TestParse_HappyPath(t *testing.T) {
	t.Parallel()
	for _, raw := range []string{"2026-27", "2000-01", "2099-00", "1999-00"} {
		got, err := indianfy.Parse(raw)
		if err != nil {
			t.Errorf("Parse(%q): %v", raw, err)
		}
		if got.String() != raw {
			t.Errorf("Parse(%q) round-trip mismatch: %s", raw, got)
		}
	}
}

func TestParse_Rejects(t *testing.T) {
	t.Parallel()
	for _, raw := range []string{
		"",
		"2026",
		"2026-26",   // tail not (start+1)
		"2026-28",   // tail beyond +1
		"2026-2027", // wrong format
		"2026/27",
		"FY26-27",
	} {
		if _, err := indianfy.Parse(raw); !errors.Is(err, indianfy.ErrInvalid) {
			t.Errorf("Parse(%q): got %v want ErrInvalid", raw, err)
		}
	}
}

func TestStartYearEndYear(t *testing.T) {
	t.Parallel()
	fy := indianfy.FY("2026-27")
	if got := fy.StartYear(); got != 2026 {
		t.Errorf("StartYear=%d want 2026", got)
	}
	if got := fy.EndYear(); got != 2027 {
		t.Errorf("EndYear=%d want 2027", got)
	}

	fy2 := indianfy.FY("2099-00")
	if got := fy2.StartYear(); got != 2099 {
		t.Errorf("StartYear=%d want 2099", got)
	}
	if got := fy2.EndYear(); got != 2100 {
		t.Errorf("EndYear=%d want 2100", got)
	}

	// Zero/invalid → returns 0.
	if got := indianfy.FY("").StartYear(); got != 0 {
		t.Errorf("zero StartYear=%d want 0", got)
	}
	if got := indianfy.FY("garbage").EndYear(); got != 0 {
		t.Errorf("invalid EndYear=%d want 0", got)
	}
}

func TestStartEnd_Instants(t *testing.T) {
	t.Parallel()
	fy := indianfy.FY("2026-27")
	wantStart := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	wantEnd := time.Date(2027, 4, 1, 0, 0, 0, 0, time.UTC)
	if !fy.Start().Equal(wantStart) {
		t.Errorf("Start=%v want %v", fy.Start(), wantStart)
	}
	if !fy.End().Equal(wantEnd) {
		t.Errorf("End=%v want %v", fy.End(), wantEnd)
	}
}

func TestContains(t *testing.T) {
	t.Parallel()
	fy := indianfy.FY("2026-27")
	cases := map[time.Time]bool{
		time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC):       true,    // lower bound INCLUSIVE
		time.Date(2026, 12, 31, 23, 59, 59, 0, time.UTC):  true,
		time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC):       true,
		time.Date(2027, 3, 31, 23, 59, 59, 0, time.UTC):   true,
		time.Date(2027, 4, 1, 0, 0, 0, 0, time.UTC):       false,   // upper bound EXCLUSIVE
		time.Date(2026, 3, 31, 23, 59, 59, 0, time.UTC):   false,   // day before start
		time.Date(2028, 4, 1, 0, 0, 0, 0, time.UTC):       false,
	}
	for ts, want := range cases {
		got := fy.Contains(ts)
		if got != want {
			t.Errorf("Contains(%s) = %v want %v", ts.Format(time.RFC3339), got, want)
		}
	}

	// Zero FY contains nothing — any time, fixed for determinism per
	// Khorikov "Unit Testing" §8 clock-injection canon.
	if indianfy.FY("").Contains(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)) {
		t.Error("zero FY.Contains() returned true")
	}
}

func TestFromDate(t *testing.T) {
	t.Parallel()
	cases := []struct {
		date time.Time
		want indianfy.FY
	}{
		{time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC), "2026-27"},
		{time.Date(2026, 12, 31, 23, 59, 59, 0, time.UTC), "2026-27"},
		{time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC), "2026-27"},
		{time.Date(2027, 3, 31, 23, 59, 59, 0, time.UTC), "2026-27"},
		{time.Date(2027, 4, 1, 0, 0, 0, 0, time.UTC), "2027-28"},
		{time.Date(2099, 4, 1, 0, 0, 0, 0, time.UTC), "2099-00"},
		{time.Date(2100, 1, 1, 0, 0, 0, 0, time.UTC), "2099-00"},
		{time.Date(2100, 4, 1, 0, 0, 0, 0, time.UTC), "2100-01"},
	}
	for _, c := range cases {
		got := indianfy.FromDate(c.date)
		if got != c.want {
			t.Errorf("FromDate(%s) = %s want %s", c.date.Format(time.DateOnly), got, c.want)
		}
	}
}

func TestCurrent_UsesInjectedNow(t *testing.T) {
	t.Parallel()
	fixedNow := func() time.Time { return time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC) }
	if got := indianfy.Current(fixedNow); got != "2026-27" {
		t.Errorf("Current(injected) = %s want 2026-27", got)
	}
	// nil now falls back to time.Now — just assert it doesn't panic.
	_ = indianfy.Current(nil) // arch-test:ignore-err -- Current returns FY (no error); test asserts non-panic with nil now-injection.
}

func TestPreviousNext(t *testing.T) {
	t.Parallel()
	fy := indianfy.FY("2026-27")
	if got := fy.Previous(); got != "2025-26" {
		t.Errorf("Previous=%s want 2025-26", got)
	}
	if got := fy.Next(); got != "2027-28" {
		t.Errorf("Next=%s want 2027-28", got)
	}

	// Cross-century rollover.
	if got := indianfy.FY("2099-00").Next(); got != "2100-01" {
		t.Errorf("2099-00.Next=%s want 2100-01", got)
	}
	if got := indianfy.FY("2100-01").Previous(); got != "2099-00" {
		t.Errorf("2100-01.Previous=%s want 2099-00", got)
	}

	// Zero FY → empty.
	if got := indianfy.FY("").Previous(); got != "" {
		t.Errorf("zero.Previous=%s want empty", got)
	}
	if got := indianfy.FY("").Next(); got != "" {
		t.Errorf("zero.Next=%s want empty", got)
	}
}
