package leadform_test

import (
	"errors"
	"testing"

	"github.com/leadkart/leadkart-go/internal/platform/domain/leadform"
)

// validInput returns an Input that passes [leadform.New]; tests mutate one
// field to assert a specific rejection.
func validInput() leadform.Input {
	return leadform.Input{
		ContactName:    "Rajesh Pharma",
		MobileE164:     "+919876543210",
		Email:          "rajesh@example.com",
		Pincode:        "411001",
		City:           "Pune",
		District:       "Pune",
		State:          "Maharashtra",
		Street:         "MG Road",
		HasDrugLicence: true,
		HasGst:         true,
		GstNumber:      "27AABCU9603R1ZV",
		HasPan:         true,
		PanNumber:      "AABCU9603R",
		BusinessType:   leadform.BusinessTypePCD,
		MedicineSystem: leadform.MedicineSystemAllopathic,
		ProductRanges:  []string{"Cardiac", "Diabetic"},
		DosageForms:    []string{"Tablet", "Capsule"},
		OrderValue:     leadform.OrderValueUpto25000,
		BuyTimeline:    leadform.BuyTimelineWithin15Days,
	}
}

func TestNew_HappyPath_ReturnsForm(t *testing.T) {
	t.Parallel()
	f, err := leadform.New(validInput())
	if err != nil {
		t.Fatalf("expected success, got %v", err)
	}
	if f.ContactName() != "Rajesh Pharma" {
		t.Errorf("ContactName=%q want %q", f.ContactName(), "Rajesh Pharma")
	}
	if f.MobileE164() != "+919876543210" {
		t.Errorf("MobileE164=%q", f.MobileE164())
	}
	if f.BusinessType() != leadform.BusinessTypePCD {
		t.Errorf("BusinessType=%q", f.BusinessType())
	}
	if got := f.ProductRanges(); len(got) != 2 || got[0] != "Cardiac" {
		t.Errorf("ProductRanges=%v", got)
	}
}

func TestNew_InvalidMobile_Rejected(t *testing.T) {
	t.Parallel()
	tests := []string{
		"",
		"9876543210",     // missing +91
		"+9198765432",    // too short
		"+9198765432109", // too long
		"+91987654321X",  // non-digit
		"+1234567890",    // wrong country
	}
	for _, m := range tests {
		in := validInput()
		in.MobileE164 = m
		_, err := leadform.New(in)
		if !errors.Is(err, leadform.ErrInvalid) {
			t.Errorf("mobile %q: expected ErrInvalid, got %v", m, err)
		}
	}
}

func TestNew_InvalidPincode_Rejected(t *testing.T) {
	t.Parallel()
	tests := []string{"", "12345", "1234567", "ABCDEF"}
	for _, p := range tests {
		in := validInput()
		in.Pincode = p
		_, err := leadform.New(in)
		if !errors.Is(err, leadform.ErrInvalid) {
			t.Errorf("pincode %q: expected ErrInvalid, got %v", p, err)
		}
	}
}

func TestNew_RequiredFields_Rejected(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		mut  func(*leadform.Input)
	}{
		{"empty contact name", func(i *leadform.Input) { i.ContactName = "" }},
		{"empty city", func(i *leadform.Input) { i.City = "" }},
		{"empty district", func(i *leadform.Input) { i.District = "" }},
		{"empty state", func(i *leadform.Input) { i.State = "" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			in := validInput()
			tc.mut(&in)
			_, err := leadform.New(in)
			if !errors.Is(err, leadform.ErrInvalid) {
				t.Errorf("expected ErrInvalid, got %v", err)
			}
		})
	}
}

func TestNew_BusinessTypeClosedSet(t *testing.T) {
	t.Parallel()
	in := validInput()
	in.BusinessType = leadform.BusinessType("Wholesaler")
	_, err := leadform.New(in)
	if !errors.Is(err, leadform.ErrInvalid) {
		t.Errorf("expected ErrInvalid for non-closed-set business type, got %v", err)
	}
}

func TestNew_MedicineSystemClosedSet(t *testing.T) {
	t.Parallel()
	in := validInput()
	in.MedicineSystem = leadform.MedicineSystem("Homoeopathic")
	_, err := leadform.New(in)
	if !errors.Is(err, leadform.ErrInvalid) {
		t.Errorf("expected ErrInvalid for non-closed-set medicine system, got %v", err)
	}
}

func TestNew_OrderValueClosedSet(t *testing.T) {
	t.Parallel()
	in := validInput()
	in.OrderValue = leadform.OrderValue("Above5Lakhs")
	_, err := leadform.New(in)
	if !errors.Is(err, leadform.ErrInvalid) {
		t.Errorf("expected ErrInvalid, got %v", err)
	}
}

func TestNew_BuyTimelineClosedSet(t *testing.T) {
	t.Parallel()
	in := validInput()
	in.BuyTimeline = leadform.BuyTimeline("WithinQuarter")
	_, err := leadform.New(in)
	if !errors.Is(err, leadform.ErrInvalid) {
		t.Errorf("expected ErrInvalid, got %v", err)
	}
}

func TestNew_GstCrossValidation(t *testing.T) {
	t.Parallel()

	in := validInput()
	in.HasGst = true
	in.GstNumber = ""
	_, err := leadform.New(in)
	if !errors.Is(err, leadform.ErrInvalid) {
		t.Errorf("has_gst=true + empty gst should reject, got %v", err)
	}

	in = validInput()
	in.HasGst = true
	in.GstNumber = "INVALID"
	_, err = leadform.New(in)
	if !errors.Is(err, leadform.ErrInvalid) {
		t.Errorf("has_gst=true + malformed gst should reject, got %v", err)
	}

	in = validInput()
	in.HasGst = false
	in.GstNumber = "27AABCU9603R1ZV"
	_, err = leadform.New(in)
	if !errors.Is(err, leadform.ErrInvalid) {
		t.Errorf("has_gst=false + gst supplied should reject, got %v", err)
	}

	in = validInput()
	in.HasGst = false
	in.GstNumber = ""
	if _, err := leadform.New(in); err != nil {
		t.Errorf("has_gst=false + empty gst should accept, got %v", err)
	}
}

func TestNew_PanCrossValidation(t *testing.T) {
	t.Parallel()

	in := validInput()
	in.HasPan = true
	in.PanNumber = ""
	_, err := leadform.New(in)
	if !errors.Is(err, leadform.ErrInvalid) {
		t.Errorf("has_pan=true + empty pan should reject, got %v", err)
	}

	in = validInput()
	in.HasPan = true
	in.PanNumber = "BADPAN"
	_, err = leadform.New(in)
	if !errors.Is(err, leadform.ErrInvalid) {
		t.Errorf("has_pan=true + malformed pan should reject, got %v", err)
	}

	in = validInput()
	in.HasPan = false
	in.PanNumber = "AABCU9603R"
	_, err = leadform.New(in)
	if !errors.Is(err, leadform.ErrInvalid) {
		t.Errorf("has_pan=false + pan supplied should reject, got %v", err)
	}
}

func TestNew_NormalizesProductRangesAndDosageForms(t *testing.T) {
	t.Parallel()
	in := validInput()
	in.ProductRanges = []string{" Cardiac ", "", "Diabetic"}
	in.DosageForms = []string{"", "  ", "Tablet"}
	f, err := leadform.New(in)
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if got := f.ProductRanges(); len(got) != 2 || got[0] != "Cardiac" || got[1] != "Diabetic" {
		t.Errorf("ProductRanges=%v", got)
	}
	if got := f.DosageForms(); len(got) != 1 || got[0] != "Tablet" {
		t.Errorf("DosageForms=%v", got)
	}
}

func TestEqual_SameInputs_True(t *testing.T) {
	t.Parallel()
	a, err := leadform.New(validInput())
	if err != nil {
		t.Fatal(err)
	}
	b, err := leadform.New(validInput())
	if err != nil {
		t.Fatal(err)
	}
	if !a.Equal(b) {
		t.Error("expected equal forms")
	}
}

func TestEqual_DifferentMobile_False(t *testing.T) {
	t.Parallel()
	a, err := leadform.New(validInput())
	if err != nil {
		t.Fatal(err)
	}
	in := validInput()
	in.MobileE164 = "+919999999999"
	b, err := leadform.New(in)
	if err != nil {
		t.Fatal(err)
	}
	if a.Equal(b) {
		t.Error("expected NOT equal")
	}
}

func TestUnmarshalFromDB_SkipsValidation(t *testing.T) {
	t.Parallel()
	// A bad mobile would fail New, but UnmarshalFromDB tolerates it as a
	// trusted-stored row.
	in := leadform.Input{
		ContactName:    "X",
		MobileE164:     "garbage",
		Pincode:        "bad",
		City:           "x",
		District:       "x",
		State:          "x",
		BusinessType:   leadform.BusinessTypePCD,
		MedicineSystem: leadform.MedicineSystemAllopathic,
		OrderValue:     leadform.OrderValueUpto25000,
		BuyTimeline:    leadform.BuyTimelineWithin15Days,
	}
	f := leadform.UnmarshalFromDB(in)
	if f.MobileE164() != "garbage" {
		t.Errorf("rehydration should pass through verbatim, got %q", f.MobileE164())
	}
}
