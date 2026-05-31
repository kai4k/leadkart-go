// Package invoicenumber provides the gapless-number allocation primitive
// for invoices, credit notes, and cancellation notes (BRD §A-014; ADR 0063 §3).
//
// Allocation unit: (TenantID, FinancialYear, Kind). The adapter UPDATEs
// orders.invoice_number_sequences inside the surrounding tx; rollback rolls
// back the increment, preserving gaplessness. This package owns the value
// objects and the [Allocator] interface.
//
// Display format: INV/2026-27/00047. [Format] builds the string from
// (Kind, FinancialYear, seq).
package invoicenumber

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"time"
)

// ErrInvalid is the sentinel for VO / input invariant violations.
var ErrInvalid = errors.New("invoicenumber: invalid")

// Kind tags the document class; each kind has its own independent sequence.
type Kind string

// Wire-stable lowercase strings matching the CHECK constraint on
// orders.invoice_number_sequences.kind.
const (
	// KindInvoice is the INV/... tax invoice issued post-packing.
	KindInvoice Kind = "invoice"
	// KindCreditNote is the CDN/... financial reversal post-delivery
	// (customer return, post-delivery cancellation).
	KindCreditNote Kind = "credit_note"
	// KindCancellationNote is the CN/... cancellation pre-delivery
	// (order cancelled after invoice but before dispatch).
	KindCancellationNote Kind = "cancellation_note"
)

// String returns the wire form.
func (k Kind) String() string { return string(k) }

// IsValid reports whether k is a known catalogue entry.
func (k Kind) IsValid() bool {
	switch k {
	case KindInvoice, KindCreditNote, KindCancellationNote:
		return true
	}
	return false
}

// Prefix returns the short display prefix per BRD §A-014 — INV, CDN, CN.
func (k Kind) Prefix() string {
	switch k {
	case KindInvoice:
		return "INV"
	case KindCreditNote:
		return "CDN"
	case KindCancellationNote:
		return "CN"
	}
	return ""
}

// FinancialYear is the Indian YYYY-YY string spanning April–March
// (BRD §A-014). Validated on construction.
type FinancialYear string

// fyPattern matches "YYYY-YY".
var fyPattern = regexp.MustCompile(`^(\d{4})-(\d{2})$`)

// String returns the underlying YYYY-YY form.
func (fy FinancialYear) String() string { return string(fy) }

// IsZero reports whether the value is unset.
func (fy FinancialYear) IsZero() bool { return fy == "" }

// ParseFinancialYear validates raw and returns a [FinancialYear] or [ErrInvalid].
func ParseFinancialYear(raw string) (FinancialYear, error) {
	m := fyPattern.FindStringSubmatch(raw)
	if m == nil {
		return "", fmt.Errorf("%w: financial year %q must be YYYY-YY", ErrInvalid, raw)
	}
	startYear, _ := strconv.Atoi(m[1])
	endYearTail, _ := strconv.Atoi(m[2])
	// tail must be (startYear+1) % 100 — handles cross-millennium (2099 → 00).
	wantTail := (startYear + 1) % 100
	if endYearTail != wantTail {
		return "", fmt.Errorf("%w: financial year %q tail must be %02d (got %02d)",
			ErrInvalid, raw, wantTail, endYearTail)
	}
	return FinancialYear(raw), nil
}

// FromDate returns the Indian financial year containing t (April 1 – March 31).
func FromDate(t time.Time) FinancialYear {
	year := t.Year()
	if int(t.Month()) < 4 {
		year--
	}
	tail := (year + 1) % 100
	return FinancialYear(fmt.Sprintf("%04d-%02d", year, tail))
}

// Number is an immutable allocated invoice/credit-note/cancellation number.
// Format: INV/2026-27/00047.
type Number struct {
	kind          Kind
	financialYear FinancialYear
	seq           int64
}

// New constructs a Number. seq must be the last_used value freshly
// allocated inside the surrounding tx.
func New(k Kind, fy FinancialYear, seq int64) (Number, error) {
	if !k.IsValid() {
		return Number{}, fmt.Errorf("%w: kind %q not in catalogue", ErrInvalid, k)
	}
	if fy.IsZero() {
		return Number{}, fmt.Errorf("%w: financial year required", ErrInvalid)
	}
	if seq <= 0 {
		return Number{}, fmt.Errorf("%w: seq must be positive (got %d)", ErrInvalid, seq)
	}
	return Number{kind: k, financialYear: fy, seq: seq}, nil
}

// MustNew is the test/init-time variant of [New]; panics on invalid input.
// Never use in request paths (CLAUDE.md: MustNewX init-time only).
func MustNew(k Kind, fy FinancialYear, seq int64) Number {
	n, err := New(k, fy, seq)
	if err != nil {
		panic(err)
	}
	return n
}

// Kind returns the document class.
func (n Number) Kind() Kind { return n.kind }

// FinancialYear returns the FY this number is anchored to.
func (n Number) FinancialYear() FinancialYear { return n.financialYear }

// Seq returns the per-(tenant, fy, kind) sequence number.
func (n Number) Seq() int64 { return n.seq }

// String returns the canonical display form, e.g. INV/2026-27/00047.
// seq is zero-padded to 5 digits; widens beyond 99999.
func (n Number) String() string {
	if n.kind == "" {
		return ""
	}
	return fmt.Sprintf("%s/%s/%05d", n.kind.Prefix(), n.financialYear, n.seq)
}

// IsZero reports whether n is the zero value.
func (n Number) IsZero() bool { return n.kind == "" }

// Format is a convenience wrapper around [New] + [Number.String]
// for callers that only need the display string.
func Format(k Kind, fy FinancialYear, seq int64) (string, error) {
	n, err := New(k, fy, seq)
	if err != nil {
		return "", err
	}
	return n.String(), nil
}
