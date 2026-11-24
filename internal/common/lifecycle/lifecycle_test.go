package lifecycle_test

import (
	"testing"

	"github.com/leadkart/leadkart-go/internal/common/lifecycle"
)

func TestParseStatusFilter(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		in           string
		want         lifecycle.StatusFilter
		ok           bool
		active, inac bool
	}{
		{"", lifecycle.FilterActive, true, true, false},
		{"active", lifecycle.FilterActive, true, true, false},
		{"inactive", lifecycle.FilterInactive, true, false, true},
		{"all", lifecycle.FilterAll, true, true, true},
		{"ACTIVE", "", false, false, false},
		{"deleted", "", false, false, false},
	} {
		t.Run(c.in, func(t *testing.T) {
			t.Parallel()
			got, ok := lifecycle.ParseStatusFilter(c.in)
			if got != c.want || ok != c.ok {
				t.Fatalf("ParseStatusFilter(%q) = %q, %v; want %q, %v", c.in, got, ok, c.want, c.ok)
			}
			if ok && (got.IncludesActive() != c.active || got.IncludesInactive() != c.inac) {
				t.Fatalf("%q includes active=%v inactive=%v, want %v %v", c.in, got.IncludesActive(), got.IncludesInactive(), c.active, c.inac)
			}
		})
	}
}

func TestZeroStatusFilterIsTheActiveListing(t *testing.T) {
	t.Parallel()
	var f lifecycle.StatusFilter
	if !f.IncludesActive() || f.IncludesInactive() {
		t.Fatalf("zero filter includes active=%v inactive=%v, want the default active listing", f.IncludesActive(), f.IncludesInactive())
	}
}
