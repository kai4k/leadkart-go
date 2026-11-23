package ports

import (
	"maps"
	"net/url"
	"slices"
	"strings"
	"testing"

	"github.com/leadkart/leadkart-go/internal/inventory/app/query"
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

// The product list's stock filters: read when valid, refused by name when not.
func TestProductStockFilters(t *testing.T) {
	t.Parallel()
	type want struct {
		category string
		lowStock bool
		days     int // -1: not set
		errs     map[string][]string
	}
	cases := []struct {
		name  string
		query string
		want  want
	}{
		{"nothing sent", "", want{days: -1}},
		{"all three", "category=+Cardiac+&low_stock=true&expiring_within_days=30", want{category: "Cardiac", lowStock: true, days: 30}},
		{"low_stock=false is no filter", "low_stock=false", want{days: -1}},
		{"zero days is a filter", "expiring_within_days=0", want{days: 0}},
		{"upper bound", "expiring_within_days=36500", want{days: 36500}},
		{"low_stock not a boolean", "low_stock=yes", want{days: -1, errs: map[string][]string{"low_stock": {"invalid_type"}}}},
		{"days not a number", "expiring_within_days=soon", want{days: -1, errs: map[string][]string{"expiring_within_days": {"invalid_type"}}}},
		{"days negative", "expiring_within_days=-1", want{days: -1, errs: map[string][]string{"expiring_within_days": {"too_small"}}}},
		{"days over the limit", "expiring_within_days=36501", want{days: 36501, errs: map[string][]string{"expiring_within_days": {"too_large"}}}},
		{"category too long", "category=" + strings.Repeat("c", 65), want{category: strings.Repeat("c", 65), days: -1, errs: map[string][]string{"category": {"too_long"}}}},
		{"category with NUL", "category=a%00b", want{category: "a\x00b", days: -1, errs: map[string][]string{"category": {"invalid_characters"}}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			q, err := url.ParseQuery(c.query)
			if err != nil {
				t.Fatalf("parse query: %v", err)
			}
			var f query.ProductListFilter
			errs := productStockFilters(q, &f)
			if !maps.EqualFunc(errs, c.want.errs, slices.Equal[[]string]) {
				t.Fatalf("errors %v, want %v", errs, c.want.errs)
			}
			if c.want.errs != nil {
				return // a refused request never reaches the query
			}
			days := -1
			if f.ExpiringWithinDays != nil {
				days = *f.ExpiringWithinDays
			}
			if f.Category != c.want.category || f.LowStock != c.want.lowStock || days != c.want.days {
				t.Fatalf("filter category %q low_stock %v days %d; want %q %v %d",
					f.Category, f.LowStock, days, c.want.category, c.want.lowStock, c.want.days)
			}
		})
	}
}
