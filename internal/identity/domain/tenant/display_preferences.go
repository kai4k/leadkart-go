package tenant

import (
	"fmt"
	"strings"
	"time"
)

// DisplayPreferences is a tenant's UI rendering preferences:
//   - Locale (BCP 47 language tag — "en-IN", "hi-IN")
//   - TimeZone (IANA tz database — "Asia/Kolkata")
//   - DateFormat (Go time.Layout-style or canonical short codes)
//   - Currency (ISO 4217 — "INR", "USD")
//
// Validation:
//   - Locale: BCP 47 shape — 2-3 lowercase letters + optional region.
//   - TimeZone: must successfully load via time.LoadLocation.
//   - DateFormat: non-empty, ≤32 chars, accepts Go time.Layout strings
//     ("02-Jan-2006") or canonical short codes ("DD-MMM-YYYY",
//     "YYYY-MM-DD", "DD/MM/YYYY").
//   - Currency: ISO 4217 — 3 uppercase letters.
//
// Per LeadKart .NET parent's DisplayPreferences VO. Defaults to
// India: en-IN / Asia/Kolkata / DD-MMM-YYYY / INR.
type DisplayPreferences struct {
	locale     string
	timeZone   string
	dateFormat string
	currency   string
}

const (
	maxLocaleLen     = 16
	maxDateFormatLen = 32
)

// NewDisplayPreferences validates each field + returns a DisplayPreferences.
//
// Pass empty strings to keep defaults — but the factory is strict (no
// silent fallbacks): empty inputs fail validation. Use [DefaultDisplayPreferences]
// as the starting point, then mutate via the per-field apply pattern
// the application layer wraps around this VO.
func NewDisplayPreferences(locale, timeZone, dateFormat, currency string) (DisplayPreferences, error) {
	locale = strings.TrimSpace(locale)
	timeZone = strings.TrimSpace(timeZone)
	dateFormat = strings.TrimSpace(dateFormat)
	currency = strings.TrimSpace(currency)

	if locale == "" {
		return DisplayPreferences{}, fmt.Errorf("%w: locale required", ErrInvalid)
	}
	if len(locale) > maxLocaleLen {
		return DisplayPreferences{}, fmt.Errorf("%w: locale %q too long (max %d)", ErrInvalid, locale, maxLocaleLen)
	}
	if !isBCP47Like(locale) {
		return DisplayPreferences{}, fmt.Errorf("%w: locale %q must be BCP 47 shape (e.g. en-IN, hi)", ErrInvalid, locale)
	}
	if timeZone == "" {
		return DisplayPreferences{}, fmt.Errorf("%w: timeZone required", ErrInvalid)
	}
	if _, err := time.LoadLocation(timeZone); err != nil {
		return DisplayPreferences{}, fmt.Errorf("%w: timeZone %q not in IANA tz database: %w", ErrInvalid, timeZone, err)
	}
	if dateFormat == "" {
		return DisplayPreferences{}, fmt.Errorf("%w: dateFormat required", ErrInvalid)
	}
	if len(dateFormat) > maxDateFormatLen {
		return DisplayPreferences{}, fmt.Errorf("%w: dateFormat too long (max %d)", ErrInvalid, maxDateFormatLen)
	}
	if !isISO4217Currency(currency) {
		return DisplayPreferences{}, fmt.Errorf("%w: currency %q must be ISO 4217 (3 uppercase letters)", ErrInvalid, currency)
	}
	return DisplayPreferences{
		locale:     locale,
		timeZone:   timeZone,
		dateFormat: dateFormat,
		currency:   currency,
	}, nil
}

// DefaultDisplayPreferences returns the system default — India-tuned.
func DefaultDisplayPreferences() DisplayPreferences {
	return DisplayPreferences{
		locale:     "en-IN",
		timeZone:   "Asia/Kolkata",
		dateFormat: "DD-MMM-YYYY",
		currency:   "INR",
	}
}

// Locale returns the BCP 47 language tag.
func (d DisplayPreferences) Locale() string { return d.locale }

// TimeZone returns the IANA tz string.
func (d DisplayPreferences) TimeZone() string { return d.timeZone }

// DateFormat returns the date-format string (Go layout or short code).
func (d DisplayPreferences) DateFormat() string { return d.dateFormat }

// Currency returns the ISO 4217 code.
func (d DisplayPreferences) Currency() string { return d.currency }

// IsZero reports whether the preferences are uninitialised.
func (d DisplayPreferences) IsZero() bool {
	return d.locale == "" && d.timeZone == "" && d.dateFormat == "" && d.currency == ""
}

// Equal compares two DisplayPreferences by all fields.
func (d DisplayPreferences) Equal(other DisplayPreferences) bool {
	return d == other
}

// isBCP47Like enforces a permissive shape: 2-3 lowercase letters
// optionally followed by a region subtag (- + 2 uppercase letters
// or 3 digits). Accepts "en", "hi", "en-IN", "hi-IN", "es-419".
// Doesn't validate against the IANA registry.
func isBCP47Like(s string) bool {
	if len(s) < 2 || len(s) > maxLocaleLen {
		return false
	}
	parts := strings.Split(s, "-")
	if len(parts) > 3 {
		return false
	}
	// Primary subtag: 2-3 lowercase letters.
	if !allLowerAlpha(parts[0], 2, 3) {
		return false
	}
	for i := 1; i < len(parts); i++ {
		p := parts[i]
		if !allUpperAlpha(p, 2, 2) && !allDigits(p, 3, 3) {
			return false
		}
	}
	return true
}

func allLowerAlpha(s string, minN, maxN int) bool {
	if len(s) < minN || len(s) > maxN {
		return false
	}
	for _, r := range s {
		if r < 'a' || r > 'z' {
			return false
		}
	}
	return true
}

func allUpperAlpha(s string, minN, maxN int) bool {
	if len(s) < minN || len(s) > maxN {
		return false
	}
	for _, r := range s {
		if r < 'A' || r > 'Z' {
			return false
		}
	}
	return true
}

func allDigits(s string, minN, maxN int) bool {
	if len(s) < minN || len(s) > maxN {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// isISO4217Currency enforces 3 uppercase ASCII letters. Doesn't
// validate against the ISO 4217 registry.
func isISO4217Currency(s string) bool {
	return allUpperAlpha(s, 3, 3)
}
