// Package invoicenumber owns the gapless-number allocation primitive
// shared by [invoice.Invoice] + [creditnote.CreditNote] + the
// `CancellationNote` path (BRD §A-014; ADR 0063 §3).
//
// The unit of allocation is the triple (TenantID, FinancialYear, Kind).
// Per the canon, a number is allocated by UPDATEing the
// `orders.invoice_number_sequences` row INSIDE the surrounding
// business tx + returning the new last_used value. Tx rollback rolls
// back the increment — preserving gaplessness under both happy + failure
// paths. This package owns the value objects + the [Allocator] interface
// the adapter implements.
//
// Format: `INV/2026-27/00047` (per BRD §A-014). [Format] builds the
// final display string from a (Kind, FinancialYear, Seq) triple.
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

// Kind tags the document class — each kind has its own sequence
// independent of the others.
type Kind string

// Closed catalogue. Wire-stable lowercase strings — match the CHECK
// constraint on orders.invoice_number_sequences.kind in the init
// migration.
const (
	// KindInvoice — `INV/...` tax invoice issued post-packing.
	KindInvoice Kind = "invoice"
	// KindCreditNote — `CDN/...` financial reversal of an invoice post-
	// delivery (e.g. customer return, post-delivery cancellation).
	KindCreditNote Kind = "credit_note"
	// KindCancellationNote — `CN/...` cancellation of an invoice
	// pre-delivery (e.g. order cancelled after invoice but before
	// dispatch).
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

// Prefix returns the short display prefix per BRD §A-014 — `INV`,
// `CDN`, `CN`.
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

// FinancialYear is the Indian-style "2026-27" string spanning April to
// March. Validated on construction; the embedded representation is
// the YYYY-YY string per BRD §A-014.
type FinancialYear string

// fyPattern matches "YYYY-YY" — the canonical format. Loose enough to
// accept any 4-digit year + 2-digit subsequent year; tight enough to
// reject malformed input.
var fyPattern = regexp.MustCompile(`^(\d{4})-(\d{2})$`)

// String returns the underlying YYYY-YY form.
func (fy FinancialYear) String() string { return string(fy) }

// IsZero reports whether the value is unset.
func (fy FinancialYear) IsZero() bool { return fy == "" }

// ParseFinancialYear validates + returns a [FinancialYear] or
// [ErrInvalid].
func ParseFinancialYear(raw string) (FinancialYear, error) {
	m := fyPattern.FindStringSubmatch(raw)
	if m == nil {
		return "", fmt.Errorf("%w: financial year %q must be YYYY-YY", ErrInvalid, raw)
	}
	startYear, _ := strconv.Atoi(m[1])
	endYearTail, _ := strconv.Atoi(m[2])
	// End year tail must be (startYear + 1) mod 100 — e.g. 2026 → 27,
	// 2099 → 00. Cross-millennium also handled.
	wantTail := (startYear + 1) % 100
	if endYearTail != wantTail {
		return "", fmt.Errorf("%w: financial year %q tail must be %02d (got %02d)",
			ErrInvalid, raw, wantTail, endYearTail)
	}
	return FinancialYear(raw), nil
}

// FromDate returns the Indian financial year containing t. India FY
// runs April 1 → March 31. April 2026 → FY 2026-27; January 2027 → FY
// 2026-27; April 2027 → FY 2027-28.
func FromDate(t time.Time) FinancialYear {
	year := t.Year()
	if int(t.Month()) < 4 {
		year--
	}
	tail := (year + 1) % 100
	return FinancialYear(fmt.Sprintf("%04d-%02d", year, tail))
}

// Number is a single allocated invoice / credit-note / cancellation
// number — immutable, valuable per its existence in the sequences
// table. Format `INV/2026-27/00047`.
type Number struct {
	kind          Kind
	financialYear FinancialYear
	seq           int64
}

// New constructs a Number. The seq value is the last_used returned by
// the allocator — caller is responsible for guaranteeing this is the
// value freshly allocated inside the surrounding tx.
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

// MustNew is the init-time / test variant of [New] that panics on
// invalid input. Use ONLY in test fixtures + composition-root literals,
// NEVER in request-path code (per CLAUDE.md "MustNewX init-time only").
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

// String returns the canonical display form `INV/2026-27/00047`. The
// seq segment is zero-padded to 5 digits (the BRD's example uses 5;
// when the tenant crosses 100k invoices in a year we widen).
func (n Number) String() string {
	if n.kind == "" {
		return ""
	}
	return fmt.Sprintf("%s/%s/%05d", n.kind.Prefix(), n.financialYear, n.seq)
}

// IsZero reports whether n is the zero value.
func (n Number) IsZero() bool { return n.kind == "" }

// Format is a free-standing convenience shortcut around [New] +
// [Number.String] — useful in tests + adapters that just want the
// display string.
func Format(k Kind, fy FinancialYear, seq int64) (string, error) {
	n, err := New(k, fy, seq)
	if err != nil {
		return "", err
	}
	return n.String(), nil
}
