package parcel_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/leadkart/leadkart-go/internal/common/parcel"
)

func TestValidate(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC)
	future := now.Add(time.Hour)
	past := now.Add(-time.Hour)
	ok := parcel.Details{CarrierName: "BlueDart", BoxCount: 2, WeightGrams: 500, ExpectedDeliveryAt: &future}

	cases := []struct {
		name string
		edit func(*parcel.Details)
		code string
	}{
		{"valid", func(*parcel.Details) {}, ""},
		{"no delivery date is fine", func(d *parcel.Details) { d.ExpectedDeliveryAt = nil }, ""},
		{"blank carrier", func(d *parcel.Details) { d.CarrierName = "   " }, parcel.CodeCarrierRequired},
		{"carrier 200 chars", func(d *parcel.Details) { d.CarrierName = strings.Repeat("é", 200) }, ""},
		{"carrier 201 chars", func(d *parcel.Details) { d.CarrierName = strings.Repeat("é", 201) }, parcel.CodeCarrierTooLong},
		{"zero boxes", func(d *parcel.Details) { d.BoxCount = 0 }, parcel.CodeBoxCountInvalid},
		{"negative boxes", func(d *parcel.Details) { d.BoxCount = -1 }, parcel.CodeBoxCountInvalid},
		{"zero weight", func(d *parcel.Details) { d.WeightGrams = 0 }, parcel.CodeWeightInvalid},
		{"past delivery", func(d *parcel.Details) { d.ExpectedDeliveryAt = &past }, parcel.CodeDeliveryNotInFuture},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d := ok
			tc.edit(&d)
			err := parcel.Validate(d, now)
			if tc.code == "" {
				if err != nil {
					t.Fatalf("want nil, got %v", err)
				}
				return
			}
			var pe *parcel.Error
			if !errors.As(err, &pe) || pe.Code != tc.code {
				t.Fatalf("want code %s, got %v", tc.code, err)
			}
			if !errors.Is(err, parcel.ErrInvalid) {
				t.Fatalf("errors.Is(ErrInvalid) must hold")
			}
		})
	}
}
