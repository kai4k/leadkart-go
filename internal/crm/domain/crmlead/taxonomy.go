package crmlead

import "fmt"

// CRM-local lead-taxonomy closed sets (BRD §5), expressed as typed
// enums per TDL "Safer Enums in Go": a defined `type X string` + typed
// consts + IsValid / String / Parse. Closed-set values in the DOMAIN
// must be a defined type, never a bare string — so the aggregate's
// stored fields + the exported [Profile] cannot hold an off-catalogue
// value once construction succeeds.
//
// These MIRROR the canonical typed enums in
// internal/platform/domain/leadform, but are redeclared here because
// importing that package would be a cross-module DOMAIN dependency
// (banned by TestArch_NoCrossModuleImports — platform/domain is not on
// the shared-kernel allow-list). The WIRE shapes stay strings: the
// integration-event payload [PurchaseSnapshot] + the
// platform.lead-purchased.v1 contract are the single source of truth
// for the string values, and [NewFromPurchaseSnapshot] parses the
// snapshot strings into these typed enums at construction. If the BRD
// set changes, both declarations must move together.
//
// The empty string is the valid "unset" zero value for every enum here
// — the migration's columns are NOT NULL DEFAULT '' and the aggregate
// accepts partial profiles (manual-import slice 2+). Parse<Type>
// therefore returns the zero value + nil for "", and an error only for
// a genuinely off-catalogue non-empty string. String() round-trips the
// stored value verbatim so the HTTP/DB wire form is byte-identical to
// the prior bare-string representation.

// BusinessType is the lead's pharma-business classification (BRD §5).
type BusinessType string

// Closed catalogue. Wire-stable values matching the
// platform.lead-purchased.v1 contract + the crm.crm_leads.business_type
// CHECK constraint.
const (
	BusinessTypePCD        BusinessType = "PCD"
	BusinessTypeThirdParty BusinessType = "ThirdParty"
)

// String returns the wire form (verbatim stored value).
func (b BusinessType) String() string { return string(b) }

// IsValid reports whether b is the empty (unset) value or a known
// catalogue entry.
func (b BusinessType) IsValid() bool {
	switch b {
	case "", BusinessTypePCD, BusinessTypeThirdParty:
		return true
	}
	return false
}

// ParseBusinessType decodes an untrusted string into a [BusinessType].
// The empty string maps to the unset zero value; any other off-catalogue
// value returns [ErrInvalid].
func ParseBusinessType(s string) (BusinessType, error) {
	b := BusinessType(s)
	if !b.IsValid() {
		return "", fmt.Errorf("%w: business_type %q not in {PCD, ThirdParty}", ErrInvalid, s)
	}
	return b, nil
}

// MedicineSystem is the lead's therapeutic system (BRD §5).
type MedicineSystem string

// Closed catalogue.
const (
	MedicineSystemAllopathic MedicineSystem = "Allopathic"
	MedicineSystemAyurvedic  MedicineSystem = "Ayurvedic"
)

// String returns the wire form (verbatim stored value).
func (m MedicineSystem) String() string { return string(m) }

// IsValid reports whether m is the empty (unset) value or a known
// catalogue entry.
func (m MedicineSystem) IsValid() bool {
	switch m {
	case "", MedicineSystemAllopathic, MedicineSystemAyurvedic:
		return true
	}
	return false
}

// ParseMedicineSystem decodes an untrusted string into a
// [MedicineSystem]. The empty string maps to the unset zero value; any
// other off-catalogue value returns [ErrInvalid].
func ParseMedicineSystem(s string) (MedicineSystem, error) {
	m := MedicineSystem(s)
	if !m.IsValid() {
		return "", fmt.Errorf("%w: medicine_system %q not in {Allopathic, Ayurvedic}", ErrInvalid, s)
	}
	return m, nil
}

// OrderValue is the lead's typical per-order spend band (BRD §5).
type OrderValue string

// Closed catalogue.
const (
	OrderValueBelow5000  OrderValue = "Below5000"
	OrderValueUpto25000  OrderValue = "Upto25000"
	OrderValueUpto50000  OrderValue = "Upto50000"
	OrderValueAbove50000 OrderValue = "Above50000"
)

// String returns the wire form (verbatim stored value).
func (o OrderValue) String() string { return string(o) }

// IsValid reports whether o is the empty (unset) value or a known
// catalogue entry.
func (o OrderValue) IsValid() bool {
	switch o {
	case "", OrderValueBelow5000, OrderValueUpto25000, OrderValueUpto50000, OrderValueAbove50000:
		return true
	}
	return false
}

// ParseOrderValue decodes an untrusted string into an [OrderValue]. The
// empty string maps to the unset zero value; any other off-catalogue
// value returns [ErrInvalid].
func ParseOrderValue(s string) (OrderValue, error) {
	o := OrderValue(s)
	if !o.IsValid() {
		return "", fmt.Errorf("%w: order_value %q not in {Below5000, Upto25000, Upto50000, Above50000}", ErrInvalid, s)
	}
	return o, nil
}

// BuyTimeline is the lead's purchase-intent horizon (BRD §5).
type BuyTimeline string

// Closed catalogue.
const (
	BuyTimelineWithinWeek   BuyTimeline = "WithinWeek"
	BuyTimelineWithin15Days BuyTimeline = "Within15Days"
	BuyTimelineWithinMonth  BuyTimeline = "WithinMonth"
)

// String returns the wire form (verbatim stored value).
func (t BuyTimeline) String() string { return string(t) }

// IsValid reports whether t is the empty (unset) value or a known
// catalogue entry.
func (t BuyTimeline) IsValid() bool {
	switch t {
	case "", BuyTimelineWithinWeek, BuyTimelineWithin15Days, BuyTimelineWithinMonth:
		return true
	}
	return false
}

// ParseBuyTimeline decodes an untrusted string into a [BuyTimeline]. The
// empty string maps to the unset zero value; any other off-catalogue
// value returns [ErrInvalid].
func ParseBuyTimeline(s string) (BuyTimeline, error) {
	t := BuyTimeline(s)
	if !t.IsValid() {
		return "", fmt.Errorf("%w: buy_timeline %q not in {WithinWeek, Within15Days, WithinMonth}", ErrInvalid, s)
	}
	return t, nil
}
