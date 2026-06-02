package tenant

import (
	"fmt"
	"strings"
	"time"
)

// DisplayPreferences holds the tenant's UI rendering preferences: Locale
// (BCP 47), TimeZone (IANA), DateFormat (Go layout or short code), and
// Currency (ISO 4217). Defaults to India: en-IN / Asia/Kolkata / DD-MMM-YYYY / INR.
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

// NewDisplayPreferences validates all four fields and returns a DisplayPreferences.
// Empty inputs are rejected; use [DefaultDisplayPreferences] as the starting point.
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

// DefaultDisplayPreferences returns the India-tuned system default.
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

// Equal reports whether d and other are identical.
func (d DisplayPreferences) Equal(other DisplayPreferences) bool {
	return d == other
}

// isBCP47Like checks a permissive BCP 47 shape: 2-3 lowercase primary
// subtag + optional region (2 uppercase letters or 3 digits).
// Does not validate against the IANA registry.
func isBCP47Like(s string) bool {
	if len(s) < 2 || len(s) > maxLocaleLen {
		return false
	}
	parts := strings.Split(s, "-")
	if len(parts) > 3 {
		return false
	}
	// Primary subtag must be 2-3 lowercase letters.
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

// isISO4217Currency enforces exactly 3 uppercase ASCII letters.
// Does not validate against the ISO 4217 registry.
func isISO4217Currency(s string) bool {
	return allUpperAlpha(s, 3, 3)
}
