package integrationevents

import "github.com/leadkart/leadkart-go/internal/platform/domain/leadform"

// LeadSnapshot is the BRD §5 lead-form payload carried by LeadPurchasedV1 and
// LeadVerifiedV1. Plain primitives only — no domain VOs, no nested aggregates
// (messaging.md "Composition, not inheritance"). CRM builds its CrmLead from
// this snapshot without calling back to Platform (Udi Dahan autonomy).
//
// Field shapes are frozen: do not change without a V2 rename (messaging.md
// "Event versioning").
type LeadSnapshot struct {
	ContactName    string   `json:"contact_name"`
	MobileE164     string   `json:"mobile_e164"`
	Email          string   `json:"email"`
	PinCode        string   `json:"pin_code"`
	City           string   `json:"city"`
	District       string   `json:"district"`
	State          string   `json:"state"`
	Street         string   `json:"street"`
	HasDrugLicence bool     `json:"has_drug_licence"`
	HasGst         bool     `json:"has_gst"`
	GstNumber      string   `json:"gst_number"` // empty when HasGst=false
	HasPan         bool     `json:"has_pan"`
	PanNumber      string   `json:"pan_number"`      // empty when HasPan=false
	BusinessType   string   `json:"business_type"`   // "PCD" | "ThirdParty"
	MedicineSystem string   `json:"medicine_system"` // "Allopathic" | "Ayurvedic"
	ProductRanges  []string `json:"product_ranges"`
	DosageForms    []string `json:"dosage_forms"`
	OrderValue     string   `json:"order_value"`  // "Below5000" | "Upto25000" | "Upto50000" | "Above50000"
	BuyTimeline    string   `json:"buy_timeline"` // "WithinWeek" | "Within15Days" | "WithinMonth"
}

// SnapshotFromForm builds a wire-stable LeadSnapshot from the domain VO, for
// handlers emitting LeadVerifiedV1 / LeadPurchasedV1.
//
// Lives here, not in leadform, because LeadSnapshot is the wire contract; the
// inverse (LeadSnapshot → Form) belongs to the consumer side's mapper.
func SnapshotFromForm(f leadform.Form) LeadSnapshot {
	return LeadSnapshot{
		ContactName:    f.ContactName(),
		MobileE164:     f.MobileE164(),
		Email:          f.Email(),
		PinCode:        f.Pincode(),
		City:           f.City(),
		District:       f.District(),
		State:          f.State(),
		Street:         f.Street(),
		HasDrugLicence: f.HasDrugLicence(),
		HasGst:         f.HasGst(),
		GstNumber:      f.GstNumber(),
		HasPan:         f.HasPan(),
		PanNumber:      f.PanNumber(),
		BusinessType:   string(f.BusinessType()),
		MedicineSystem: string(f.MedicineSystem()),
		ProductRanges:  f.ProductRanges(),
		DosageForms:    f.DosageForms(),
		OrderValue:     string(f.OrderValue()),
		BuyTimeline:    string(f.BuyTimeline()),
	}
}
