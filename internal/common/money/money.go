// Package money is the cross-module money-formatting primitive. Per
// ADR 0061 (Stripe canon — int64 paise everywhere; never float, never
// decimal at the wire boundary).
//
// This package owns:
//
//   - Paise-to-display-string formatting in the Indian lakhs/crores
//     comma style (₹1,23,45,678.00 — NOT the Western 12,345,678.00 style).
//   - Display-string-to-paise parsing for operator HTTP inputs (rare —
//     most callers send int64 paise; the parser exists for "MRP shown on
//     packaging matches int64 paise" reconciliation flows).
//   - Currency enum (INR canonical at v0.2; USD/EUR sketched for the
//     multi-currency expansion path).
//
// The package has ZERO dependencies on other internal packages — it
// imports only stdlib. Safe to import from any layer (domain / app /
// adapters / ports). Aggregates referencing money in their domain
// methods import this for the formatter; HTTP DTOs use it on the
// response side.
package money

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// ErrInvalid is the sentinel returned by [ParsePaise] for malformed
// inputs.
var ErrInvalid = errors.New("money: invalid")

// Currency is the wire-stable ISO-4217 code enum. INR is canonical at
// v0.2 per BRD; USD + EUR sketched for the multi-currency expansion.
type Currency string

// Closed catalogue. Wire-stable uppercase strings — ISO-4217 canon.
const (
	INR Currency = "INR"
	USD Currency = "USD"
	EUR Currency = "EUR"
)

// IsValid reports whether c is in the catalogue.
func (c Currency) IsValid() bool {
	switch c {
	case INR, USD, EUR:
		return true
	}
	return false
}

// String returns the wire form.
func (c Currency) String() string { return string(c) }

// Symbol returns the canonical display symbol for c. INR → ₹, USD → $,
// EUR → €. Empty for unknown.
func (c Currency) Symbol() string {
	switch c {
	case INR:
		return "₹"
	case USD:
		return "$"
	case EUR:
		return "€"
	}
	return ""
}

// MinorUnitsPerMajor returns the divisor that converts minor-unit
// integers to major-unit display values. INR / USD / EUR all use 100
// (paise / cents / cents). Future currencies (JPY 1, KWD 1000, etc)
// extend this table.
func (c Currency) MinorUnitsPerMajor() int64 {
	switch c {
	case INR, USD, EUR:
		return 100
	}
	return 0
}

// FormatPaiseINR is a shorthand for [Format](paise, [INR]). The most
// common call site (BRD §6.4 invoices are INR-only at v0.2).
func FormatPaiseINR(paise int64) string {
	return Format(paise, INR)
}

// Format converts an int64 minor-unit value into the canonical display
// string for c.
//
// For INR the integer part is grouped in the Indian lakhs/crores style:
// the LAST three digits are grouped first, then every subsequent group
// is TWO digits (`₹1,23,45,678.50`). For USD/EUR the Western group-of-
// three style applies (`$12,345.67`).
//
// Negative values are rendered with a leading minus AFTER the symbol
// (`₹-1,234.50`) — sign immediately after symbol mirrors the Stripe
// + Plaid display canon + matches how Indian banking statements
// render debit lines.
//
// Zero renders as `₹0.00` (NOT bare `₹0`) so all values have a
// consistent two-decimal display width.
func Format(minor int64, c Currency) string {
	div := c.MinorUnitsPerMajor()
	if div == 0 {
		// Unknown currency — fall back to bare integer with the wire
		// code as suffix (e.g. "12345 XYZ"). Operators see this only
		// when migration adds a new currency without extending the
		// table here.
		return fmt.Sprintf("%d %s", minor, c)
	}

	negative := minor < 0
	abs := minor
	if negative {
		abs = -minor
	}
	majorPart := abs / div
	minorPart := abs % div

	var grouped string
	if c == INR {
		grouped = groupINR(majorPart)
	} else {
		grouped = groupWestern(majorPart)
	}

	sign := ""
	if negative {
		sign = "-"
	}
	// Decimal width: 2 digits for the 100-minor-units currencies.
	return fmt.Sprintf("%s%s%s.%02d", c.Symbol(), sign, grouped, minorPart)
}

// ParsePaise is the inverse of [FormatPaiseINR] — parses an Indian
// rupee display string back into int64 paise. Tolerant of whitespace +
// optional ₹ + grouping commas; rejects malformed input with
// [ErrInvalid].
//
// Example: " ₹ 1,23,456.78 " → 12345678. Negative: "-12.50" → -1250.
//
// NOTE: this parser is intentionally permissive on grouping style — it
// strips commas without enforcing the lakhs/crores grouping. Operator
// reconciliation inputs may be auto-generated from external systems
// that use the Western style.
func ParsePaise(raw string) (int64, error) {
	s := strings.TrimSpace(raw)
	// Strip leading minus (outside-symbol style: "-₹1.00").
	negative := strings.HasPrefix(s, "-")
	if negative {
		s = strings.TrimSpace(s[1:])
	}
	// Strip the currency symbol (either before or after the minus).
	s = strings.TrimPrefix(s, "₹")
	s = strings.TrimSpace(s)
	// Strip leading minus AGAIN for the inside-symbol style ("₹-1.00").
	if strings.HasPrefix(s, "-") {
		negative = true
		s = s[1:]
	}
	s = strings.ReplaceAll(s, ",", "")

	parts := strings.SplitN(s, ".", 2)
	major, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%w: major part %q: %w", ErrInvalid, parts[0], err)
	}
	var minor int64
	if len(parts) == 2 {
		// Pad / truncate fractional part to exactly 2 digits.
		frac := parts[1]
		switch {
		case len(frac) == 0:
			// trailing dot, no digits — treat as zero minor.
		case len(frac) == 1:
			n, perr := strconv.ParseInt(frac, 10, 64)
			if perr != nil {
				return 0, fmt.Errorf("%w: fractional %q: %w", ErrInvalid, frac, perr)
			}
			minor = n * 10
		case len(frac) >= 2:
			// Only first 2 digits matter (rest is rounded away).
			n, perr := strconv.ParseInt(frac[:2], 10, 64)
			if perr != nil {
				return 0, fmt.Errorf("%w: fractional %q: %w", ErrInvalid, frac, perr)
			}
			minor = n
		}
	}

	v := major*100 + minor
	if negative {
		v = -v
	}
	return v, nil
}

// groupINR returns the integer part formatted in the Indian
// lakhs/crores style: last 3 digits, then every subsequent group of 2.
// Example: 12345678 → "1,23,45,678".
func groupINR(n int64) string {
	s := strconv.FormatInt(n, 10)
	if len(s) <= 3 {
		return s
	}
	// Last 3 digits.
	tail := s[len(s)-3:]
	head := s[:len(s)-3]
	// Subsequent groups of 2 (from the right).
	var groups []string
	for len(head) > 2 {
		groups = append([]string{head[len(head)-2:]}, groups...)
		head = head[:len(head)-2]
	}
	if head != "" {
		groups = append([]string{head}, groups...)
	}
	return strings.Join(groups, ",") + "," + tail
}

// groupWestern returns the integer part formatted in the standard
// group-of-3 Western style: 12345678 → "12,345,678".
func groupWestern(n int64) string {
	s := strconv.FormatInt(n, 10)
	if len(s) <= 3 {
		return s
	}
	tail := s[len(s)-3:]
	head := s[:len(s)-3]
	var groups []string
	for len(head) > 3 {
		groups = append([]string{head[len(head)-3:]}, groups...)
		head = head[:len(head)-3]
	}
	if head != "" {
		groups = append([]string{head}, groups...)
	}
	return strings.Join(groups, ",") + "," + tail
}
