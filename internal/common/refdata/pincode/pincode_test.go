package pincode_test

import (
	"errors"
	"testing"

	"github.com/leadkart/leadkart-go/internal/common/refdata/pincode"
	"github.com/leadkart/leadkart-go/internal/common/refdata/pincode/pincodetest"
)

func TestNew_HappyPath(t *testing.T) {
	t.Parallel()
	for _, raw := range []string{"411001", "110001", "999999", "100000"} {
		c, err := pincode.New(raw)
		if err != nil {
			t.Errorf("New(%q): %v", raw, err)
		}
		if c.String() != raw {
			t.Errorf("New(%q).String() = %s", raw, c.String())
		}
	}
}

func TestNew_Rejects(t *testing.T) {
	t.Parallel()
	cases := []string{
		"",          // empty
		"12345",     // too short
		"1234567",   // too long
		"012345",    // first digit 0 (India Post reserved)
		"abcdef",    // non-numeric
		"123 456",   // whitespace
		"123-456",   // dash
		"41100A",    // alpha tail
	}
	for _, raw := range cases {
		if _, err := pincode.New(raw); !errors.Is(err, pincode.ErrInvalid) {
			t.Errorf("New(%q): got %v want ErrInvalid", raw, err)
		}
	}
}

func TestMustNew_PanicsOnInvalid(t *testing.T) {
	t.Parallel()
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic")
		}
	}()
	_ = pincode.MustNew("garbage")
}

func TestFakeReader_DefaultSeedCoversMetros(t *testing.T) {
	t.Parallel()
	r := pincodetest.NewFakeReader()

	cases := []struct {
		raw         string
		wantState   string
		wantStateCode string
		wantGST     string
	}{
		{"411001", "Maharashtra", "MH", "27"},
		{"400001", "Maharashtra", "MH", "27"},
		{"110001", "Delhi", "DL", "07"},
		{"560001", "Karnataka", "KA", "29"},
		{"600001", "Tamil Nadu", "TN", "33"},
		{"700001", "West Bengal", "WB", "19"},
		{"500001", "Telangana", "TG", "36"},
		{"380001", "Gujarat", "GJ", "24"},
	}
	for _, c := range cases {
		t.Run(c.raw, func(t *testing.T) {
			t.Parallel()
			code := pincode.MustNew(c.raw)
			got, err := r.Lookup(t.Context(), code)
			if err != nil {
				t.Fatalf("Lookup: %v", err)
			}
			if got.State != c.wantState {
				t.Errorf("state=%s want %s", got.State, c.wantState)
			}
			if got.StateCode != c.wantStateCode {
				t.Errorf("state_code=%s want %s", got.StateCode, c.wantStateCode)
			}
			if got.StateGSTCode != c.wantGST {
				t.Errorf("gst_code=%s want %s", got.StateGSTCode, c.wantGST)
			}
		})
	}
}

func TestFakeReader_ReturnsErrNotFound(t *testing.T) {
	t.Parallel()
	r := pincodetest.NewFakeReader()
	_, err := r.Lookup(t.Context(), pincode.MustNew("999999"))
	if !errors.Is(err, pincode.ErrNotFound) {
		t.Errorf("got %v want ErrNotFound", err)
	}
}

func TestFakeReader_Seed(t *testing.T) {
	t.Parallel()
	r := pincodetest.NewFakeReader()
	custom := pincode.Lookup{
		Pincode:      pincode.MustNew("131001"),
		City:         "Sonipat",
		District:     "Sonipat",
		State:        "Haryana",
		StateCode:    "HR",
		StateGSTCode: "06",
	}
	r.Seed(custom)
	got, err := r.Lookup(t.Context(), custom.Pincode)
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if got.City != "Sonipat" {
		t.Errorf("city=%s", got.City)
	}
}

func TestFakeReader_SeedReplacesExisting(t *testing.T) {
	t.Parallel()
	r := pincodetest.NewFakeReader()
	// Override a default — useful when a test wants the same pincode
	// to resolve to a different city (e.g. testing renamed districts).
	r.Seed(pincode.Lookup{
		Pincode:      pincode.MustNew("411001"),
		City:         "Custom City",
		District:     "Custom",
		State:        "Custom State",
		StateCode:    "CS",
		StateGSTCode: "00",
	})
	got, _ := r.Lookup(t.Context(), pincode.MustNew("411001"))
	if got.City != "Custom City" {
		t.Errorf("override didn't apply: city=%s", got.City)
	}
}
