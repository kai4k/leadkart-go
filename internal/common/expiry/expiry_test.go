package expiry_test

import (
	"testing"
	"time"

	"github.com/leadkart/leadkart-go/internal/common/expiry"
)

const ist = 330 // minutes east of UTC, passed as data

func day(y int, m time.Month, d int) time.Time { return time.Date(y, m, d, 0, 0, 0, 0, time.UTC) }

func utc(y int, m time.Month, d, h, mi, s int) time.Time {
	return time.Date(y, m, d, h, mi, s, 0, time.UTC)
}

func TestBusinessDay_TheDayFlipsAtLocalMidnightNotAt0530(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		now  time.Time
		want time.Time
	}{
		{"23:59:59 IST on 28 Feb is still 28 Feb", utc(2027, 2, 28, 18, 29, 59), day(2027, 2, 28)},
		{"00:00 IST on 1 Mar is 1 Mar", utc(2027, 2, 28, 18, 30, 0), day(2027, 3, 1)},
		{"00:00 UTC (05:30 IST) on 1 Mar is 1 Mar", utc(2027, 3, 1, 0, 0, 0), day(2027, 3, 1)},
		{"23:59 UTC on 28 Feb (05:29 IST on 1 Mar) is 1 Mar", utc(2027, 2, 28, 23, 59, 0), day(2027, 3, 1)},
		{"a non-UTC instant is read as the same moment", utc(2027, 2, 28, 18, 30, 0).In(time.FixedZone("x", -3600)), day(2027, 3, 1)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := expiry.BusinessDay(tt.now, ist); !got.Equal(tt.want) {
				t.Errorf("BusinessDay(%s) = %s, want %s", tt.now, got.Format(time.DateOnly), tt.want.Format(time.DateOnly))
			}
		})
	}
	if got := expiry.BusinessDay(utc(2027, 2, 28, 23, 0, 0), 0); !got.Equal(day(2027, 2, 28)) {
		t.Errorf("offset 0 must be the UTC date, got %s", got.Format(time.DateOnly))
	}
}

func TestLastSellableDay_PrintedMarch2027(t *testing.T) {
	t.Parallel()
	exp := day(2027, 3, 31)
	tests := []struct {
		n    int
		last time.Time
	}{
		{0, day(2027, 3, 31)},
		{1, day(2027, 2, 28)},
		{3, day(2026, 12, 31)},
		{14, day(2026, 1, 31)},
	}
	for _, tt := range tests {
		if got := expiry.LastSellableDay(exp, tt.n); !got.Equal(tt.last) {
			t.Errorf("N=%d: LastSellableDay = %s, want %s", tt.n, got.Format(time.DateOnly), tt.last.Format(time.DateOnly))
		}
	}
	if got := expiry.LastSellableDay(day(2028, 3, 31), 1); !got.Equal(day(2028, 2, 29)) {
		t.Errorf("leap: %s", got.Format(time.DateOnly))
	}
}

// The month-end boundary at the two instants the design names: 00:00 IST
// (18:30 UTC the day before) and 05:30 IST (00:00 UTC), for the default N=1
// and the legal limit N=0.
func TestIsSellable_BoundaryAtMonthEndInIST(t *testing.T) {
	t.Parallel()
	exp := day(2027, 3, 31) // EXP 03/2027
	tests := []struct {
		name string
		n    int
		now  time.Time
		want bool
	}{
		{"N=1 23:59:59 IST 28 Feb", 1, utc(2027, 2, 28, 18, 29, 59), true},
		{"N=1 00:00 IST 1 Mar", 1, utc(2027, 2, 28, 18, 30, 0), false},
		{"N=1 05:30 IST 1 Mar (00:00 UTC)", 1, utc(2027, 3, 1, 0, 0, 0), false},
		{"N=1 05:29 IST 28 Feb is 28 Feb", 1, utc(2027, 2, 27, 23, 59, 0), true},
		{"N=1 05:30 IST 28 Feb (00:00 UTC)", 1, utc(2027, 2, 28, 0, 0, 0), true},
		{"N=0 23:59:59 IST 31 Mar", 0, utc(2027, 3, 31, 18, 29, 59), true},
		{"N=0 00:00 IST 1 Apr", 0, utc(2027, 3, 31, 18, 30, 0), false},
		{"N=0 05:30 IST 1 Apr (00:00 UTC)", 0, utc(2027, 4, 1, 0, 0, 0), false},
		{"N=0 05:29 IST 31 Mar is 31 Mar", 0, utc(2027, 3, 30, 23, 59, 0), true},
		{"N=3 last day 31 Dec 23:59 IST", 3, utc(2026, 12, 31, 18, 29, 0), true},
		{"N=3 1 Jan 00:00 IST", 3, utc(2026, 12, 31, 18, 30, 0), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			today := expiry.BusinessDay(tt.now, ist)
			if got := expiry.IsSellable(exp, today, tt.n); got != tt.want {
				t.Errorf("IsSellable = %v, want %v (business day %s)", got, tt.want, today.Format(time.DateOnly))
			}
			if got := !exp.Before(expiry.MinSellableExpiry(today, tt.n)); got != tt.want {
				t.Errorf("the SQL bound says %v, want %v", got, tt.want)
			}
		})
	}
}

// IsSellable and the single SQL bound agree with the definition (today is not
// after the last sellable day) on every day of five years, every stop from 0 to
// 24 and every printed month in a window around it.
func TestSellable_GoAndTheSQLBoundAgreeEverywhere(t *testing.T) {
	t.Parallel()
	for d := day(2026, 1, 1); d.Before(day(2031, 1, 1)); d = d.AddDate(0, 0, 1) {
		for n := 0; n <= 24; n++ {
			bound := expiry.MinSellableExpiry(d, n)
			if !expiry.IsMonthEnd(bound) {
				t.Fatalf("bound %s is not a month end", bound.Format(time.DateOnly))
			}
			for m := -30; m <= 40; m++ {
				exp := expiry.MonthEnd(d.Year(), d.Month()+time.Month(m))
				want := !d.After(expiry.LastSellableDay(exp, n))
				if got := expiry.IsSellable(exp, d, n); got != want {
					t.Fatalf("IsSellable(%s, %s, %d) = %v, want %v", exp.Format(time.DateOnly), d.Format(time.DateOnly), n, got, want)
				}
				if sqlSide := !exp.Before(bound); sqlSide != want {
					t.Fatalf("SQL bound: expiry %s today %s N=%d: %v, want %v", exp.Format(time.DateOnly), d.Format(time.DateOnly), n, sqlSide, want)
				}
			}
		}
	}
}

func TestPastPrintedExpiry_NoSettingMovesIt(t *testing.T) {
	t.Parallel()
	exp := day(2027, 3, 31)
	if expiry.IsPastPrintedExpiry(exp, day(2027, 3, 31)) {
		t.Error("the last day of the printed month is not past expiry")
	}
	if !expiry.IsPastPrintedExpiry(exp, day(2027, 4, 1)) {
		t.Error("1 April is past a March expiry")
	}
}

func TestLatestExpiryBy_IsTheUpperBoundOfALastSellableDayWindow(t *testing.T) {
	t.Parallel()
	for d := day(2026, 1, 1); d.Before(day(2028, 1, 1)); d = d.AddDate(0, 0, 1) {
		for n := 0; n <= 4; n++ {
			upper := expiry.LatestExpiryBy(d, n)
			for m := -6; m <= 40; m++ {
				exp := expiry.MonthEnd(2026, time.Month(1+m))
				want := !expiry.LastSellableDay(exp, n).After(d)
				if got := !exp.After(upper); got != want {
					t.Fatalf("day %s N=%d expiry %s: bound says %v, want %v", d.Format(time.DateOnly), n, exp.Format(time.DateOnly), got, want)
				}
			}
		}
	}
}

func TestDaysLeft_CountsToTheLastSellableDay(t *testing.T) {
	t.Parallel()
	exp := day(2027, 3, 31)
	if got := expiry.DaysLeft(exp, day(2027, 2, 28), 1); got != 0 {
		t.Errorf("on the last sellable day: %d", got)
	}
	if got := expiry.DaysLeft(exp, day(2027, 1, 29), 1); got != 30 {
		t.Errorf("30 days before: %d", got)
	}
	if got := expiry.DaysLeft(exp, day(2027, 3, 1), 1); got != -1 {
		t.Errorf("after: %d", got)
	}
}

func TestParsePrintedMonth(t *testing.T) {
	t.Parallel()
	got, err := expiry.ParsePrintedMonth("2027-03")
	if err != nil || !got.Equal(day(2027, 3, 31)) {
		t.Fatalf("got %v %v", got, err)
	}
	if expiry.PrintedMonth(got) != "2027-03" {
		t.Error("round trip")
	}
	for _, bad := range []string{"", "2027-3", "2027-13", "2027-03-31", "03/2027", "2027-00", "abcd-ef"} {
		if _, err := expiry.ParsePrintedMonth(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	leap, _ := expiry.ParsePrintedMonth("2028-02")
	if !leap.Equal(day(2028, 2, 29)) {
		t.Errorf("leap: %s", leap)
	}
}
