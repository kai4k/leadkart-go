// Package expiry is the one rule for whether a batch may still be sold.
//
// A medicine label prints a month and year; the law reads it as valid through
// the LAST DAY of that month. A batch's expiry is therefore stored as that
// month-end date. A company may stop selling some whole months earlier
// (stop_months_before, N): N=0 sells through the printed month, N=1 stops at
// the start of the printed month, N=3 stops three months before.
//
// Everything that decides sellability goes through this package:
//
//	LastSellableDay(expiry, N)   last day of (printed month - N months)
//	IsSellable(expiry, today, N) today <= LastSellableDay(expiry, N)
//	MinSellableExpiry(today, N)  last day of (month of today + N months)
//
// IsSellable is exactly equivalent to expiry >= MinSellableExpiry(today, N), so
// every SQL query takes the ONE bound as a parameter (expiry_date >= $bound):
// Go and SQL cannot disagree and the expiry index stays usable.
//
// The business day is the calendar date at the company's UTC offset, not the
// server's UTC date: the date must not flip at the wrong hour. The offset is
// company data (a setting), never a constant here.
//
// Dates are date-only values held as midnight UTC. The package imports only
// the standard library (the common/parcel precedent) so modules may share it.
package expiry

import (
	"fmt"
	"time"
)

// Offset limits, in minutes east of UTC, covering every civil time zone.
const (
	MinOffsetMinutes = -12 * 60
	MaxOffsetMinutes = 14 * 60
)

// BusinessDay returns the calendar date at now's moment in the zone offset
// minutes east of UTC, as midnight UTC (a date-only value).
func BusinessDay(now time.Time, offsetMinutes int) time.Time {
	local := now.UTC().Add(time.Duration(offsetMinutes) * time.Minute)
	return time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, time.UTC)
}

// MonthEnd returns the last day of the given month as a date-only value.
func MonthEnd(year int, month time.Month) time.Time {
	return time.Date(year, month+1, 0, 0, 0, 0, 0, time.UTC)
}

// FromPrintedMonth returns the stored expiry of a batch whose label prints
// year/month.
func FromPrintedMonth(year int, month time.Month) (time.Time, error) {
	if year < 1900 || year > 9999 || month < time.January || month > time.December {
		return time.Time{}, fmt.Errorf("expiry: month %04d-%02d out of range", year, int(month))
	}
	return MonthEnd(year, month), nil
}

// MonthLayout is the wire form of an expiry month ("2027-03").
const MonthLayout = "2006-01"

// ParsePrintedMonth reads "2027-03" (the wire form of an expiry month).
func ParsePrintedMonth(s string) (time.Time, error) {
	t, err := time.Parse(MonthLayout, s)
	t = t.UTC()
	if err != nil || t.Format(MonthLayout) != s {
		return time.Time{}, fmt.Errorf("expiry: %q is not a YYYY-MM month", s)
	}
	return FromPrintedMonth(t.Year(), t.Month())
}

// PrintedMonth is the wire form of a stored expiry: "2027-03".
func PrintedMonth(expiry time.Time) string { return expiry.UTC().Format(MonthLayout) }

// IsMonthEnd reports whether d is the last day of its month (the only form a
// batch expiry takes).
func IsMonthEnd(d time.Time) bool {
	d = d.UTC()
	return d.Day() == MonthEnd(d.Year(), d.Month()).Day()
}

// monthIndex counts months so month arithmetic is plain integer arithmetic.
func monthIndex(d time.Time) int {
	d = d.UTC()
	return d.Year()*12 + int(d.Month()) - 1
}

func endOfMonthIndex(idx int) time.Time {
	return MonthEnd(idx/12, time.Month(idx%12+1))
}

// LastSellableDay is the last day a batch with this printed expiry may be
// sold under a stop of n months: the last day of (printed month - n months).
func LastSellableDay(expiry time.Time, n int) time.Time {
	return endOfMonthIndex(monthIndex(expiry) - n)
}

// IsSellable reports whether a batch with this printed expiry may be sold on
// today (a business day) under a stop of n months.
func IsSellable(expiry, today time.Time, n int) bool {
	return monthIndex(today) <= monthIndex(expiry)-n
}

// MinSellableExpiry is the one SQL bound: a batch is sellable on today exactly
// when its stored expiry is >= this date.
func MinSellableExpiry(today time.Time, n int) time.Time {
	return endOfMonthIndex(monthIndex(today) + n)
}

// IsPastPrintedExpiry reports whether the printed expiry month is over on
// today. This is the legal limit that no setting moves: such stock may only be
// quarantined, returned to the maker or written off.
func IsPastPrintedExpiry(expiry, today time.Time) bool {
	return monthIndex(today) > monthIndex(expiry)
}

// DaysLeft is how many days remain from today to the last sellable day: 0 on
// the last sellable day, negative once it has passed.
func DaysLeft(expiry, today time.Time, n int) int {
	return int(LastSellableDay(expiry, n).Sub(today.UTC()).Hours() / 24)
}

// LatestExpiryBy returns the latest stored expiry whose last sellable day is on
// or before day: the SQL upper bound of "last sellable day <= day" (expiry_date
// <= bound). Together with MinSellableExpiry(today, n) it selects the batches
// still sellable today whose last sellable day falls within a window.
func LatestExpiryBy(day time.Time, n int) time.Time {
	idx := monthIndex(day)
	if !IsMonthEnd(day) {
		idx--
	}
	return endOfMonthIndex(idx + n)
}
