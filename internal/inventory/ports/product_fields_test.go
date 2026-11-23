package ports

import (
	"maps"
	"slices"
	"strings"
	"testing"
)

// The product form's new fields are refused by name: errors{field:[code]}.
func TestProductFieldErrors(t *testing.T) {
	t.Parallel()
	str := func(s string) *string { return &s }
	cases := []struct {
		name      string
		category  *string
		reorder   *int
		threshold *int
		want      map[string][]string
	}{
		{"nothing sent", nil, nil, nil, nil},
		{"all valid", str("Cardiac"), new(0), new(36500), nil},
		{"blank category means the default", str("  "), nil, nil, nil},
		{"64 characters of Devanagari fit", str(strings.Repeat("क", 64)), nil, nil, nil},
		{"category over 64 characters", str(strings.Repeat("क", 65)), nil, nil, map[string][]string{"product_category": {"too_long"}}},
		{"category with NUL", str("a\x00b"), nil, nil, map[string][]string{"product_category": {"invalid_characters"}}},
		{"negative reorder level", nil, new(-1), nil, map[string][]string{"reorder_level": {"too_small"}}},
		{"reorder level over the column", nil, new(2147483648), nil, map[string][]string{"reorder_level": {"too_large"}}},
		{"negative threshold", nil, nil, new(-1), map[string][]string{"expiry_alert_threshold_days": {"too_small"}}},
		{"threshold over 100 years", nil, nil, new(36501), map[string][]string{"expiry_alert_threshold_days": {"too_large"}}},
		{"every field wrong at once", str("a\x00"), new(-1), new(-1), map[string][]string{
			"product_category": {"invalid_characters"}, "reorder_level": {"too_small"}, "expiry_alert_threshold_days": {"too_small"},
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			got := productFieldErrors(c.category, c.reorder, c.threshold)
			if !maps.EqualFunc(got, c.want, slices.Equal[[]string]) {
				t.Fatalf("got %v, want %v", got, c.want)
			}
		})
	}
}
