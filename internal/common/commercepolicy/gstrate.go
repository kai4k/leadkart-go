package commercepolicy

import (
	"errors"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/leadkart/leadkart-go/internal/common/humantext"
)

// GST rate limits. A rate is basis points of the taxable value (500 is 5%).
const (
	GSTRateMaxBps          = 10000
	GSTLabelMaxChars       = 40
	GSTConfirmedByMaxChars = 120
	// DateLayout is the wire form of a date (confirmed_on, a filing date).
	DateLayout = "2006-01-02"
)

// GSTRate is one entry of a company's GST rate list. The company confirms each
// rate by name; LeadKart does not check a rate against the law. A rate is
// deactivated, never deleted, because products and documents refer to it.
type GSTRate struct {
	ID          string `json:"id"`
	RateBps     int    `json:"rate_bps"`
	Label       string `json:"label"`
	ConfirmedBy string `json:"confirmed_by"`
	ConfirmedOn string `json:"confirmed_on"`
	Active      bool   `json:"active"`
}

// Stable codes of a refused GST rate field, carried in errors{field:[code]}.
const (
	CodeRequired          = "required"
	CodeTooLong           = "too_long"
	CodeInvalidCharacters = "invalid_characters"
	CodeOutOfRange        = "out_of_range"
	CodeInvalidFormat     = "invalid_format"
	CodeDuplicate         = "duplicate"
)

// ErrGSTRate is the sentinel for a refused GST rate; the field codes are in a
// [FieldErrors].
var ErrGSTRate = errors.New("commercepolicy: invalid gst rate")

// FieldErrors maps a field name to its stable refusal codes.
type FieldErrors map[string][]string

func (f FieldErrors) add(field, code string) {
	if !slices.Contains(f[field], code) {
		f[field] = append(f[field], code)
	}
}

// Err returns nil for no errors, else the map as an error wrapping [ErrGSTRate].
func (f FieldErrors) Err() error {
	if len(f) == 0 {
		return nil
	}
	return &fieldErrorsError{fields: f}
}

type fieldErrorsError struct{ fields FieldErrors }

func (e *fieldErrorsError) Error() string   { return "commercepolicy: invalid fields" }
func (e *fieldErrorsError) Is(t error) bool { return t == ErrGSTRate }

// Fields returns the field errors an error from this package carries, or nil.
func Fields(err error) FieldErrors {
	var fe *fieldErrorsError
	if errors.As(err, &fe) {
		return fe.fields
	}
	return nil
}

// CheckGSTRate returns the field errors of a rate's editable fields: the rate,
// the label and the confirming person's name. Field names are the wire names.
func CheckGSTRate(rateBps int, label, confirmedBy string) FieldErrors {
	errs := FieldErrors{}
	if rateBps < 0 || rateBps > GSTRateMaxBps {
		errs.add("rate_bps", CodeOutOfRange)
	}
	checkLine(errs, "label", label, GSTLabelMaxChars)
	checkLine(errs, "confirmed_by", confirmedBy, GSTConfirmedByMaxChars)
	return errs
}

// CheckDate returns the code a date string breaks, or "".
func CheckDate(s string) string {
	if _, err := time.Parse(DateLayout, s); err != nil {
		return CodeInvalidFormat
	}
	return ""
}

// checkLine records the rule a one-line text field breaks.
func checkLine(errs FieldErrors, field, v string, maxChars int) {
	v = strings.TrimSpace(v)
	switch {
	case v == "":
		errs.add(field, CodeRequired)
	case utf8.RuneCountInString(v) > maxChars:
		errs.add(field, CodeTooLong)
	case !humantext.Valid(v) || strings.ContainsRune(v, 0):
		errs.add(field, CodeInvalidCharacters)
	}
}

// NormalizeLabel is the stored form of a label: trimmed.
func NormalizeLabel(s string) string { return strings.TrimSpace(s) }

// SameRate reports whether two entries are the same rate under the same label,
// which a company's list holds once (case-insensitive label).
func SameRate(a, b GSTRate) bool {
	return a.RateBps == b.RateBps && strings.EqualFold(NormalizeLabel(a.Label), NormalizeLabel(b.Label))
}

// validateGSTRates checks the list as stored: each entry valid, no duplicate ID,
// no duplicate (rate, label), ordered by rate then label.
func validateGSTRates(rates []GSTRate) []string {
	var codes []string
	for i, r := range rates {
		if r.ID == "" {
			codes = append(codes, CodeRequired)
		}
		if len(CheckGSTRate(r.RateBps, r.Label, r.ConfirmedBy)) > 0 || CheckDate(r.ConfirmedOn) != "" {
			codes = append(codes, CodeInvalidFormat)
		}
		for _, o := range rates[i+1:] {
			if o.ID == r.ID || SameRate(o, r) {
				codes = append(codes, CodeDuplicate)
			}
		}
	}
	return codes
}

// SortGSTRates orders a list by rate, then label, then ID: the one order every
// reader sees.
func SortGSTRates(rates []GSTRate) {
	slices.SortFunc(rates, func(a, b GSTRate) int {
		if a.RateBps != b.RateBps {
			return a.RateBps - b.RateBps
		}
		if c := strings.Compare(strings.ToLower(a.Label), strings.ToLower(b.Label)); c != 0 {
			return c
		}
		return strings.Compare(a.ID, b.ID)
	})
}
