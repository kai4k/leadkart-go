// Package indianfy is the canonical Indian financial-year primitive.
// Per BRD §A-014 (GSTR-1 invoice numbering scopes by FY).
//
// India's FY runs April 1 → March 31. The canonical text form is
// "2026-27" — start-year long form + the next-year tail (2 digits,
// mod 100 so 2099-00 is valid as the FY 2099–2100).
//
// This package is the SHARED implementation. The orders module's
// `invoicenumber.FinancialYear` is a TYPE ALIAS to this when both
// branches merge (currently has its own implementation on
// `feature/orders-domain-skeleton` — extracted here so OTHER modules
// that need FY-scoped reasoning (payment reconciliation, GST returns,
// year-end roll-up reports) don't duplicate the parser).
//
// Zero dependencies on other internal packages.
package indianfy

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"time"
)

// ErrInvalid is the sentinel for parse failures.
var ErrInvalid = errors.New("indianfy: invalid")

// FY is the validated "YYYY-YY" string.
type FY string

// pattern enforces the canonical format. Loose enough to accept any
// 4-digit year + 2-digit tail; tight enough to reject malformed input.
var pattern = regexp.MustCompile(`^(\d{4})-(\d{2})$`)

// String returns the wire form.
func (fy FY) String() string { return string(fy) }

// IsZero reports whether the value is unset.
func (fy FY) IsZero() bool { return fy == "" }

// StartYear returns the calendar year the FY began in (e.g. 2026 for
// "2026-27"). Panics if the FY was constructed without [Parse] (zero
// value or malformed). Use [Parse] in all callers.
func (fy FY) StartYear() int {
	m := pattern.FindStringSubmatch(string(fy))
	if m == nil {
		return 0
	}
	y, _ := strconv.Atoi(m[1])
	return y
}

// EndYear returns the calendar year the FY ends in (e.g. 2027 for
// "2026-27", 2100 for "2099-00").
func (fy FY) EndYear() int {
	start := fy.StartYear()
	if start == 0 {
		return 0
	}
	return start + 1
}

// Start returns the FY start instant (April 1 00:00:00 UTC of the
// start year).
func (fy FY) Start() time.Time {
	y := fy.StartYear()
	if y == 0 {
		return time.Time{}
	}
	return time.Date(y, time.April, 1, 0, 0, 0, 0, time.UTC)
}

// End returns the FY end instant (April 1 00:00:00 UTC of the start
// year + 1 — i.e. the EXCLUSIVE upper bound on FY-scoped queries).
// Half-open interval `[Start, End)` per Go's canonical time-range
// idiom. The LAST INSTANT in the FY is `End.Add(-time.Nanosecond)`.
func (fy FY) End() time.Time {
	y := fy.StartYear()
	if y == 0 {
		return time.Time{}
	}
	return time.Date(y+1, time.April, 1, 0, 0, 0, 0, time.UTC)
}

// Contains reports whether t falls inside the half-open `[Start, End)`
// interval (FY-scoped queries SHOULD use this rather than rebuilding
// the comparison everywhere).
func (fy FY) Contains(t time.Time) bool {
	if fy.IsZero() {
		return false
	}
	tu := t.UTC()
	return !tu.Before(fy.Start()) && tu.Before(fy.End())
}

// Parse validates raw + returns a [FY] or [ErrInvalid].
//
// The tail MUST equal (start + 1) mod 100 — e.g. 2026-27 ✓, 2099-00 ✓,
// 2026-26 ✗ (typo), 2026-28 ✗ (impossible). This catches operator
// typos at the boundary rather than letting "2026-26" silently scope a
// 0-day FY.
func Parse(raw string) (FY, error) {
	m := pattern.FindStringSubmatch(raw)
	if m == nil {
		return "", fmt.Errorf("%w: %q is not YYYY-YY", ErrInvalid, raw)
	}
	startYear, _ := strconv.Atoi(m[1])
	tail, _ := strconv.Atoi(m[2])
	wantTail := (startYear + 1) % 100
	if tail != wantTail {
		return "", fmt.Errorf("%w: %q tail must be %02d (got %02d)",
			ErrInvalid, raw, wantTail, tail)
	}
	return FY(raw), nil
}

// FromDate returns the FY that contains the supplied instant. April
// → March 31 = current calendar year's FY; January → March = the FY
// that started in the PREVIOUS calendar year.
//
// Example:
//
//	FromDate(2026-04-01) → "2026-27"
//	FromDate(2026-12-31) → "2026-27"
//	FromDate(2027-01-01) → "2026-27"
//	FromDate(2027-03-31) → "2026-27"
//	FromDate(2027-04-01) → "2027-28"
//	FromDate(2099-04-01) → "2099-00"
func FromDate(t time.Time) FY {
	y := t.Year()
	if int(t.Month()) < int(time.April) {
		y--
	}
	tail := (y + 1) % 100
	return FY(fmt.Sprintf("%04d-%02d", y, tail))
}

// Current returns the FY containing now. Convenience for cron jobs +
// reporting endpoints. Callers that need test-deterministic time
// should use [FromDate](nowFunc()) instead.
func Current(now func() time.Time) FY {
	if now == nil {
		now = time.Now
	}
	return FromDate(now())
}

// Previous returns the FY preceding fy (the one that ENDED on this
// fy's Start instant). Useful for year-over-year reports.
func (fy FY) Previous() FY {
	y := fy.StartYear()
	if y == 0 {
		return ""
	}
	prev := y - 1
	tail := (prev + 1) % 100
	return FY(fmt.Sprintf("%04d-%02d", prev, tail))
}

// Next returns the FY following fy. Useful for FY-roll-over operations.
func (fy FY) Next() FY {
	y := fy.StartYear()
	if y == 0 {
		return ""
	}
	next := y + 1
	tail := (next + 1) % 100
	return FY(fmt.Sprintf("%04d-%02d", next, tail))
}
