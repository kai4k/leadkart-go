// Package pincodetest provides the in-memory FakeReader implementing
// [pincode.Reader]. TDL canon — per-primitive fakes live in
// <primitive>test/ sibling packages.
package pincodetest

import (
	"context"

	"github.com/leadkart/leadkart-go/internal/common/refdata/pincode"
)

// FakeReader is an in-memory [pincode.Reader]. Pre-seeded with the
// common Indian metro pincodes via [NewFakeReader] for the common test
// case; extend via [FakeReader.Seed] for additional fixtures.
type FakeReader struct {
	Store map[pincode.Code]pincode.Lookup
}

// NewFakeReader returns a reader pre-populated with a few canonical
// metro pincodes — sufficient for most app-layer + handler unit tests
// without needing custom seeding.
func NewFakeReader() *FakeReader {
	r := &FakeReader{Store: make(map[pincode.Code]pincode.Lookup)}
	for _, l := range defaultSeed() {
		r.Store[l.Pincode] = l
	}
	return r
}

// Compile-time interface conformance.
var _ pincode.Reader = (*FakeReader)(nil)

// Lookup satisfies [pincode.Reader].
func (r *FakeReader) Lookup(_ context.Context, code pincode.Code) (pincode.Lookup, error) {
	l, ok := r.Store[code]
	if !ok {
		return pincode.Lookup{}, pincode.ErrNotFound
	}
	return l, nil
}

// Seed adds / replaces a row in the fake's store. Tests that need a
// non-default pincode pre-populate via this method.
func (r *FakeReader) Seed(l pincode.Lookup) {
	r.Store[l.Pincode] = l
}

// defaultSeed returns the metro-pincode pre-population. Coverage is
// intentionally narrow — the production migration seeds the full
// India Post directory.
func defaultSeed() []pincode.Lookup {
	return []pincode.Lookup{
		{
			Pincode:      pincode.MustNew("411001"),
			City:         "Pune City",
			District:     "Pune",
			State:        "Maharashtra",
			StateCode:    "MH",
			StateGSTCode: "27",
		},
		{
			Pincode:      pincode.MustNew("400001"),
			City:         "Mumbai",
			District:     "Mumbai",
			State:        "Maharashtra",
			StateCode:    "MH",
			StateGSTCode: "27",
		},
		{
			Pincode:      pincode.MustNew("110001"),
			City:         "New Delhi",
			District:     "Central Delhi",
			State:        "Delhi",
			StateCode:    "DL",
			StateGSTCode: "07",
		},
		{
			Pincode:      pincode.MustNew("560001"),
			City:         "Bangalore",
			District:     "Bengaluru Urban",
			State:        "Karnataka",
			StateCode:    "KA",
			StateGSTCode: "29",
		},
		{
			Pincode:      pincode.MustNew("600001"),
			City:         "Chennai",
			District:     "Chennai",
			State:        "Tamil Nadu",
			StateCode:    "TN",
			StateGSTCode: "33",
		},
		{
			Pincode:      pincode.MustNew("700001"),
			City:         "Kolkata",
			District:     "Kolkata",
			State:        "West Bengal",
			StateCode:    "WB",
			StateGSTCode: "19",
		},
		{
			Pincode:      pincode.MustNew("500001"),
			City:         "Hyderabad",
			District:     "Hyderabad",
			State:        "Telangana",
			StateCode:    "TG",
			StateGSTCode: "36",
		},
		{
			Pincode:      pincode.MustNew("380001"),
			City:         "Ahmedabad",
			District:     "Ahmedabad",
			State:        "Gujarat",
			StateCode:    "GJ",
			StateGSTCode: "24",
		},
	}
}
