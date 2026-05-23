// Package leadform defines the BRD §5 lead-form value object shared by
// the Platform module's UnverifiedContact + PlatformLead aggregates +
// the integration-event LeadSnapshot payload.
//
// All fields per BRD §5 "Lead Form — Locked Fields" — Indian PCD pharma
// canon. The factory validates field-by-field invariants; cross-field
// references to shared.pincodes / GST checksum live at the caller (the
// BFF pre-fills city/district/state from pincode, then the handler calls
// New here for shape validation).
//
// Per coding-standards.md "VO factories cross-validate, not just per-
// field": HasGst implies GstNumber non-empty + format-valid; HasPan
// implies PanNumber non-empty + format-valid. These cross-checks live
// here.
package leadform

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
)

// ErrInvalid is the sentinel for lead-form invariant violations.
var ErrInvalid = errors.New("leadform: invalid")

// BusinessType + MedicineSystem + OrderValue + BuyTimeline are
// closed-set strings per BRD §5. Exported as typed constants so callers
// + tests don't sprinkle literals.
type BusinessType string

// Closed-set [BusinessType] values per BRD §5.
const (
	BusinessTypePCD        BusinessType = "PCD"
	BusinessTypeThirdParty BusinessType = "ThirdParty"
)

// IsValid reports whether b is one of the closed-set entries.
func (b BusinessType) IsValid() bool {
	return b == BusinessTypePCD || b == BusinessTypeThirdParty
}

// String returns the wire form.
func (b BusinessType) String() string { return string(b) }

// MedicineSystem closed set.
type MedicineSystem string

// Closed-set [MedicineSystem] values per BRD §5.
const (
	MedicineSystemAllopathic MedicineSystem = "Allopathic"
	MedicineSystemAyurvedic  MedicineSystem = "Ayurvedic"
)

// IsValid reports whether m is one of the closed-set entries.
func (m MedicineSystem) IsValid() bool {
	return m == MedicineSystemAllopathic || m == MedicineSystemAyurvedic
}

// String returns the wire form.
func (m MedicineSystem) String() string { return string(m) }

// OrderValue closed set per BRD §5.
type OrderValue string

// Closed-set [OrderValue] bands per BRD §5.
const (
	OrderValueBelow5000  OrderValue = "Below5000"
	OrderValueUpto25000  OrderValue = "Upto25000"
	OrderValueUpto50000  OrderValue = "Upto50000"
	OrderValueAbove50000 OrderValue = "Above50000"
)

// IsValid reports whether o is one of the closed-set entries.
func (o OrderValue) IsValid() bool {
	switch o {
	case OrderValueBelow5000, OrderValueUpto25000, OrderValueUpto50000, OrderValueAbove50000:
		return true
	}
	return false
}

// String returns the wire form.
func (o OrderValue) String() string { return string(o) }

// BuyTimeline closed set per BRD §5.
type BuyTimeline string

// Closed-set [BuyTimeline] values per BRD §5.
const (
	BuyTimelineWithinWeek    BuyTimeline = "WithinWeek"
	BuyTimelineWithin15Days  BuyTimeline = "Within15Days"
	BuyTimelineWithinMonth   BuyTimeline = "WithinMonth"
)

// IsValid reports whether t is one of the closed-set entries.
func (t BuyTimeline) IsValid() bool {
	switch t {
	case BuyTimelineWithinWeek, BuyTimelineWithin15Days, BuyTimelineWithinMonth:
		return true
	}
	return false
}

// String returns the wire form.
func (t BuyTimeline) String() string { return string(t) }

// Regexes for E.164 (+91 + 10 digits), 6-digit pincode, GST + PAN
// patterns per Indian regulatory canon (BRD Appendix B).
var (
	mobileRE  = regexp.MustCompile(`^\+91[0-9]{10}$`)
	pincodeRE = regexp.MustCompile(`^[0-9]{6}$`)
	gstRE     = regexp.MustCompile(`^[0-9]{2}[A-Z]{5}[0-9]{4}[A-Z][0-9A-Z][Z][0-9A-Z]$`)
	panRE     = regexp.MustCompile(`^[A-Z]{5}[0-9]{4}[A-Z]$`)
)

// Form is the immutable BRD §5 value object. Compare via [Form.Equal].
//
// All getters return the contained value verbatim — no defensive copy
// for primitives; collections (ProductRanges + DosageForms) return a
// shared backing slice and callers MUST NOT mutate.
type Form struct {
	contactName    string
	mobileE164     string
	emailAddr      string
	pincode        string
	city           string
	district       string
	stateName      string
	street         string
	hasDrugLicence bool
	hasGst         bool
	gstNumber      string
	hasPan         bool
	panNumber      string
	businessType   BusinessType
	medicineSystem MedicineSystem
	productRanges  []string
	dosageForms    []string
	orderValue     OrderValue
	buyTimeline    BuyTimeline
}

// Input is the bag of raw strings + bools the factory consumes. Keeps
// the [New] signature manageable as BRD §5 fields evolve.
type Input struct {
	ContactName    string
	MobileE164     string
	Email          string
	Pincode        string
	City           string
	District       string
	State          string
	Street         string
	HasDrugLicence bool
	HasGst         bool
	GstNumber      string
	HasPan         bool
	PanNumber      string
	BusinessType   BusinessType
	MedicineSystem MedicineSystem
	ProductRanges  []string
	DosageForms    []string
	OrderValue     OrderValue
	BuyTimeline    BuyTimeline
}

// New constructs a Form from raw inputs, validating BRD §5 invariants.
// Returns [ErrInvalid] wrapped with the specific failure on rejection.
//
// Validation policy:
//   - All required text fields trimmed-non-empty.
//   - mobile_e164 matches +91 + 10 digits.
//   - pincode is exactly 6 digits.
//   - has_gst implies gst_number format-valid (and vice versa: gst
//     supplied without has_gst is rejected).
//   - has_pan implies pan_number format-valid.
//   - business_type, medicine_system, order_value, buy_timeline are
//     closed-set members.
//   - product_ranges + dosage_forms entries trimmed-non-empty; nil
//     slices are tolerated (downstream stores as `text[]` zero array).
//
// Returns a value type (not a pointer) — Form is small + immutable;
// pointer indirection buys nothing here.
func New(in Input) (Form, error) {
	if strings.TrimSpace(in.ContactName) == "" {
		return Form{}, fmt.Errorf("%w: contact_name required", ErrInvalid)
	}
	if len(in.ContactName) > 200 {
		return Form{}, fmt.Errorf("%w: contact_name too long (max 200)", ErrInvalid)
	}
	if !mobileRE.MatchString(in.MobileE164) {
		return Form{}, fmt.Errorf("%w: mobile_e164 must match +91XXXXXXXXXX (got %q)", ErrInvalid, in.MobileE164)
	}
	if !pincodeRE.MatchString(in.Pincode) {
		return Form{}, fmt.Errorf("%w: pincode must be 6 digits (got %q)", ErrInvalid, in.Pincode)
	}
	if strings.TrimSpace(in.City) == "" {
		return Form{}, fmt.Errorf("%w: city required", ErrInvalid)
	}
	if strings.TrimSpace(in.District) == "" {
		return Form{}, fmt.Errorf("%w: district required", ErrInvalid)
	}
	if strings.TrimSpace(in.State) == "" {
		return Form{}, fmt.Errorf("%w: state required", ErrInvalid)
	}
	if !in.BusinessType.IsValid() {
		return Form{}, fmt.Errorf("%w: business_type %q not in {PCD,ThirdParty}", ErrInvalid, in.BusinessType)
	}
	if !in.MedicineSystem.IsValid() {
		return Form{}, fmt.Errorf("%w: medicine_system %q not in {Allopathic,Ayurvedic}", ErrInvalid, in.MedicineSystem)
	}
	if !in.OrderValue.IsValid() {
		return Form{}, fmt.Errorf("%w: order_value %q invalid", ErrInvalid, in.OrderValue)
	}
	if !in.BuyTimeline.IsValid() {
		return Form{}, fmt.Errorf("%w: buy_timeline %q invalid", ErrInvalid, in.BuyTimeline)
	}

	// HasGst/PAN cross-validation. has_x=true ↔ x_number non-empty +
	// format-valid; has_x=false ↔ x_number empty.
	gstNorm := strings.TrimSpace(strings.ToUpper(in.GstNumber))
	if in.HasGst {
		if !gstRE.MatchString(gstNorm) {
			return Form{}, fmt.Errorf("%w: has_gst=true but gst_number %q invalid", ErrInvalid, in.GstNumber)
		}
	} else {
		if gstNorm != "" {
			return Form{}, fmt.Errorf("%w: has_gst=false but gst_number supplied", ErrInvalid)
		}
	}
	panNorm := strings.TrimSpace(strings.ToUpper(in.PanNumber))
	if in.HasPan {
		if !panRE.MatchString(panNorm) {
			return Form{}, fmt.Errorf("%w: has_pan=true but pan_number %q invalid", ErrInvalid, in.PanNumber)
		}
	} else {
		if panNorm != "" {
			return Form{}, fmt.Errorf("%w: has_pan=false but pan_number supplied", ErrInvalid)
		}
	}

	// Clean product_ranges + dosage_forms — drop empties, keep order.
	ranges := cleanStringList(in.ProductRanges)
	dfs := cleanStringList(in.DosageForms)

	return Form{
		contactName:    strings.TrimSpace(in.ContactName),
		mobileE164:     in.MobileE164,
		emailAddr:      strings.TrimSpace(in.Email),
		pincode:        in.Pincode,
		city:           strings.TrimSpace(in.City),
		district:       strings.TrimSpace(in.District),
		stateName:      strings.TrimSpace(in.State),
		street:         strings.TrimSpace(in.Street),
		hasDrugLicence: in.HasDrugLicence,
		hasGst:         in.HasGst,
		gstNumber:      gstNorm,
		hasPan:         in.HasPan,
		panNumber:      panNorm,
		businessType:   in.BusinessType,
		medicineSystem: in.MedicineSystem,
		productRanges:  ranges,
		dosageForms:    dfs,
		orderValue:     in.OrderValue,
		buyTimeline:    in.BuyTimeline,
	}, nil
}

// UnmarshalFromDB rehydrates a Form WITHOUT re-validating — TDL canon
// for trusted-storage re-hydration paths. Callers (repositories) MUST
// have read the row from a trusted source.
func UnmarshalFromDB(in Input) Form {
	return Form{
		contactName:    in.ContactName,
		mobileE164:     in.MobileE164,
		emailAddr:      in.Email,
		pincode:        in.Pincode,
		city:           in.City,
		district:       in.District,
		stateName:      in.State,
		street:         in.Street,
		hasDrugLicence: in.HasDrugLicence,
		hasGst:         in.HasGst,
		gstNumber:      in.GstNumber,
		hasPan:         in.HasPan,
		panNumber:      in.PanNumber,
		businessType:   in.BusinessType,
		medicineSystem: in.MedicineSystem,
		productRanges:  cleanStringList(in.ProductRanges),
		dosageForms:    cleanStringList(in.DosageForms),
		orderValue:     in.OrderValue,
		buyTimeline:    in.BuyTimeline,
	}
}

// cleanStringList trims each entry + drops empties. Returns nil for
// empty/all-empty input so storage layer writes an empty `text[]`.
func cleanStringList(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	out := make([]string, 0, len(in))
	for _, s := range in {
		t := strings.TrimSpace(s)
		if t == "" {
			continue
		}
		out = append(out, t)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// ----- Getters --------------------------------------------------------------

// ContactName returns the contact-person full name.
func (f Form) ContactName() string { return f.contactName }

// MobileE164 returns the +91XXXXXXXXXX mobile number.
func (f Form) MobileE164() string { return f.mobileE164 }

// Email returns the contact email (may be empty — BRD §5 not required).
func (f Form) Email() string { return f.emailAddr }

// Pincode returns the 6-digit Indian PIN code.
func (f Form) Pincode() string { return f.pincode }

// City returns the city auto-derived from pincode (BRD §5).
func (f Form) City() string { return f.city }

// District returns the district auto-derived from pincode.
func (f Form) District() string { return f.district }

// State returns the state name auto-derived from pincode.
func (f Form) State() string { return f.stateName }

// Street returns the optional free-text street address.
func (f Form) Street() string { return f.street }

// HasDrugLicence reports the boolean drug-licence declaration.
func (f Form) HasDrugLicence() bool { return f.hasDrugLicence }

// HasGst reports whether the lead has a GST registration.
func (f Form) HasGst() bool { return f.hasGst }

// GstNumber returns the GSTIN (empty if HasGst == false).
func (f Form) GstNumber() string { return f.gstNumber }

// HasPan reports whether the lead has a PAN.
func (f Form) HasPan() bool { return f.hasPan }

// PanNumber returns the PAN (empty if HasPan == false).
func (f Form) PanNumber() string { return f.panNumber }

// BusinessType returns the closed-set business type.
func (f Form) BusinessType() BusinessType { return f.businessType }

// MedicineSystem returns the closed-set medicine system.
func (f Form) MedicineSystem() MedicineSystem { return f.medicineSystem }

// ProductRanges returns the (possibly nil) product-range list.
// Callers MUST NOT mutate the returned slice — VO is immutable.
func (f Form) ProductRanges() []string { return f.productRanges }

// DosageForms returns the (possibly nil) dosage-form list.
// Callers MUST NOT mutate the returned slice.
func (f Form) DosageForms() []string { return f.dosageForms }

// OrderValue returns the closed-set order-value band.
func (f Form) OrderValue() OrderValue { return f.orderValue }

// BuyTimeline returns the closed-set buy timeline.
func (f Form) BuyTimeline() BuyTimeline { return f.buyTimeline }

// Equal reports value equality. Used in idempotency no-op checks +
// tests.
func (f Form) Equal(other Form) bool {
	if f.contactName != other.contactName ||
		f.mobileE164 != other.mobileE164 ||
		f.emailAddr != other.emailAddr ||
		f.pincode != other.pincode ||
		f.city != other.city ||
		f.district != other.district ||
		f.stateName != other.stateName ||
		f.street != other.street ||
		f.hasDrugLicence != other.hasDrugLicence ||
		f.hasGst != other.hasGst ||
		f.gstNumber != other.gstNumber ||
		f.hasPan != other.hasPan ||
		f.panNumber != other.panNumber ||
		f.businessType != other.businessType ||
		f.medicineSystem != other.medicineSystem ||
		f.orderValue != other.orderValue ||
		f.buyTimeline != other.buyTimeline {
		return false
	}
	return slices.Equal(f.productRanges, other.productRanges) &&
		slices.Equal(f.dosageForms, other.dosageForms)
}
