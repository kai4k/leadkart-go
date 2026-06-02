// Package leadform defines the BRD §5 lead-form value object shared by the
// Platform module's UnverifiedContact + PlatformLead aggregates and the
// LeadSnapshot integration-event payload.
//
// The factory validates per-field shape (BRD §5 locked fields, Indian PCD
// pharma canon) plus the cross-field rules HasGst↔GstNumber and HasPan↔PanNumber
// (coding-standards.md). Pincode→city/district/state lookup lives at the caller.
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

// BusinessType is a closed-set string per BRD §5.
type BusinessType string

// BusinessType values.
const (
	BusinessTypePCD        BusinessType = "PCD"
	BusinessTypeThirdParty BusinessType = "ThirdParty"
)

// IsValid reports whether b is a known BusinessType.
func (b BusinessType) IsValid() bool {
	return b == BusinessTypePCD || b == BusinessTypeThirdParty
}

// String returns the wire form.
func (b BusinessType) String() string { return string(b) }

// MedicineSystem is a closed-set string per BRD §5.
type MedicineSystem string

// MedicineSystem values.
const (
	MedicineSystemAllopathic MedicineSystem = "Allopathic"
	MedicineSystemAyurvedic  MedicineSystem = "Ayurvedic"
)

// IsValid reports whether m is a known MedicineSystem.
func (m MedicineSystem) IsValid() bool {
	return m == MedicineSystemAllopathic || m == MedicineSystemAyurvedic
}

// String returns the wire form.
func (m MedicineSystem) String() string { return string(m) }

// OrderValue is a closed-set band per BRD §5.
type OrderValue string

// OrderValue bands.
const (
	OrderValueBelow5000  OrderValue = "Below5000"
	OrderValueUpto25000  OrderValue = "Upto25000"
	OrderValueUpto50000  OrderValue = "Upto50000"
	OrderValueAbove50000 OrderValue = "Above50000"
)

// IsValid reports whether o is a known OrderValue.
func (o OrderValue) IsValid() bool {
	switch o {
	case OrderValueBelow5000, OrderValueUpto25000, OrderValueUpto50000, OrderValueAbove50000:
		return true
	}
	return false
}

// String returns the wire form.
func (o OrderValue) String() string { return string(o) }

// BuyTimeline is a closed-set string per BRD §5.
type BuyTimeline string

// BuyTimeline values.
const (
	BuyTimelineWithinWeek   BuyTimeline = "WithinWeek"
	BuyTimelineWithin15Days BuyTimeline = "Within15Days"
	BuyTimelineWithinMonth  BuyTimeline = "WithinMonth"
)

// IsValid reports whether t is a known BuyTimeline.
func (t BuyTimeline) IsValid() bool {
	switch t {
	case BuyTimelineWithinWeek, BuyTimelineWithin15Days, BuyTimelineWithinMonth:
		return true
	}
	return false
}

// String returns the wire form.
func (t BuyTimeline) String() string { return string(t) }

// Indian regulatory format patterns per BRD Appendix B.
var (
	mobileRE  = regexp.MustCompile(`^\+91[0-9]{10}$`)
	pincodeRE = regexp.MustCompile(`^[0-9]{6}$`)
	gstRE     = regexp.MustCompile(`^[0-9]{2}[A-Z]{5}[0-9]{4}[A-Z][0-9A-Z][Z][0-9A-Z]$`)
	panRE     = regexp.MustCompile(`^[A-Z]{5}[0-9]{4}[A-Z]$`)
)

// Form is the immutable BRD §5 value object. Compare via [Form.Equal].
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

// Input carries the raw fields [New] validates and [UnmarshalFromDB] rehydrates.
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

// New constructs a Form from raw inputs, enforcing BRD §5 invariants. It returns
// [ErrInvalid] wrapped with the specific failure on rejection. has_gst/has_pan
// must agree with the respective number (set+valid, or unset+empty); list entries
// are trimmed and empties dropped (nil tolerated).
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

// UnmarshalFromDB rehydrates a Form from a trusted row without re-validating
// (TDL canon for trusted-storage rehydration).
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

// cleanStringList trims entries and drops empties, returning nil when nothing
// remains so storage writes an empty text[].
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

// ContactName returns the contact-person full name.
func (f Form) ContactName() string { return f.contactName }

// MobileE164 returns the +91XXXXXXXXXX mobile number.
func (f Form) MobileE164() string { return f.mobileE164 }

// Email returns the contact email; may be empty (BRD §5 optional).
func (f Form) Email() string { return f.emailAddr }

// Pincode returns the 6-digit PIN code.
func (f Form) Pincode() string { return f.pincode }

// City returns the city.
func (f Form) City() string { return f.city }

// District returns the district.
func (f Form) District() string { return f.district }

// State returns the state name.
func (f Form) State() string { return f.stateName }

// Street returns the optional street address.
func (f Form) Street() string { return f.street }

// HasDrugLicence reports the drug-licence declaration.
func (f Form) HasDrugLicence() bool { return f.hasDrugLicence }

// HasGst reports whether the lead has a GST registration.
func (f Form) HasGst() bool { return f.hasGst }

// GstNumber returns the GSTIN; empty when HasGst is false.
func (f Form) GstNumber() string { return f.gstNumber }

// HasPan reports whether the lead has a PAN.
func (f Form) HasPan() bool { return f.hasPan }

// PanNumber returns the PAN; empty when HasPan is false.
func (f Form) PanNumber() string { return f.panNumber }

// BusinessType returns the business type.
func (f Form) BusinessType() BusinessType { return f.businessType }

// MedicineSystem returns the medicine system.
func (f Form) MedicineSystem() MedicineSystem { return f.medicineSystem }

// ProductRanges returns a defensive copy, keeping the VO immutable.
func (f Form) ProductRanges() []string { return slices.Clone(f.productRanges) }

// DosageForms returns a defensive copy, keeping the VO immutable.
func (f Form) DosageForms() []string { return slices.Clone(f.dosageForms) }

// OrderValue returns the order-value band.
func (f Form) OrderValue() OrderValue { return f.orderValue }

// BuyTimeline returns the buy timeline.
func (f Form) BuyTimeline() BuyTimeline { return f.buyTimeline }

// Equal reports value equality. Used in idempotency no-op checks and tests.
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
