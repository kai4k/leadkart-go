package postaladdress_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/leadkart/leadkart-go/internal/common/postaladdress"
)

func TestNew_AcceptsValid(t *testing.T) {
	t.Parallel()
	addr, err := postaladdress.New(
		"123 MG Road, Block A",
		"Bangalore",
		"Bangalore Urban",
		"Karnataka",
		"KA",
		"560001",
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if addr.City() != "Bangalore" {
		t.Errorf("City = %q", addr.City())
	}
	if addr.Pincode() != "560001" {
		t.Errorf("Pincode = %q", addr.Pincode())
	}
	if addr.IsZero() {
		t.Error("IsZero should be false for valid address")
	}
}

func TestNew_RejectsInvalid(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name                                                 string
		street, city, district, state, stateCode, pincode    string
	}{
		{"empty street", "", "Bangalore", "BU", "Karnataka", "KA", "560001"},
		{"empty city", "Street", "", "BU", "Karnataka", "KA", "560001"},
		{"empty state", "Street", "Bangalore", "BU", "", "KA", "560001"},
		{"pincode 5 digits", "Street", "Bangalore", "BU", "Karnataka", "KA", "12345"},
		{"pincode 7 digits", "Street", "Bangalore", "BU", "Karnataka", "KA", "1234567"},
		{"pincode leading 0", "Street", "Bangalore", "BU", "Karnataka", "KA", "012345"},
		{"pincode alpha", "Street", "Bangalore", "BU", "Karnataka", "KA", "12345A"},
		{"street too long", strings.Repeat("X", 201), "Bangalore", "BU", "Karnataka", "KA", "560001"},
		{"city too long", "Street", strings.Repeat("X", 81), "BU", "Karnataka", "KA", "560001"},
		{"stateCode too long", "Street", "Bangalore", "BU", "Karnataka", "KARNT", "560001"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := postaladdress.New(tc.street, tc.city, tc.district, tc.state, tc.stateCode, tc.pincode)
			if !errors.Is(err, postaladdress.ErrInvalid) {
				t.Errorf("expected ErrInvalid, got %v", err)
			}
		})
	}
}

func TestCreate_CrossValidatesCityVsLookup(t *testing.T) {
	t.Parallel()
	lookup := postaladdress.LookupData{
		Cities:    []string{"Bangalore", "Bengaluru", "Bangalore GPO"},
		District:  "Bangalore Urban",
		State:     "Karnataka",
		StateCode: "KA",
	}
	addr, err := postaladdress.Create("123 MG Road", "bangalore", "560001", lookup)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if addr.City() != "Bangalore" {
		t.Errorf("canonical city not normalized: %q", addr.City())
	}
	if addr.District() != "Bangalore Urban" {
		t.Errorf("District not from lookup: %q", addr.District())
	}
	if addr.State() != "Karnataka" {
		t.Errorf("State not from lookup: %q", addr.State())
	}
}

func TestCreate_RejectsCityNotInLookup(t *testing.T) {
	t.Parallel()
	lookup := postaladdress.LookupData{
		Cities:    []string{"Bangalore", "Bengaluru"},
		District:  "Bangalore Urban",
		State:     "Karnataka",
		StateCode: "KA",
	}
	_, err := postaladdress.Create("123 MG Road", "Mumbai", "560001", lookup)
	if !errors.Is(err, postaladdress.ErrInvalid) {
		t.Errorf("expected ErrInvalid for city/pincode mismatch, got %v", err)
	}
}

func TestEqual(t *testing.T) {
	t.Parallel()
	a, _ := postaladdress.New("S", "C", "D", "St", "ST", "560001")
	b, _ := postaladdress.New("S", "C", "D", "St", "ST", "560001")
	c, _ := postaladdress.New("X", "C", "D", "St", "ST", "560001")
	if !a.Equal(b) {
		t.Error("a should equal b")
	}
	if a.Equal(c) {
		t.Error("a should not equal c")
	}
}

func TestZeroIsZero(t *testing.T) {
	t.Parallel()
	var a postaladdress.Address
	if !a.IsZero() {
		t.Error("zero value should be IsZero")
	}
	if a.String() != "" {
		t.Errorf("zero String() = %q", a.String())
	}
}
