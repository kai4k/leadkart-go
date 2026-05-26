package gststate_test

import (
	"errors"
	"testing"

	"github.com/leadkart/leadkart-go/internal/common/gststate"
)

func TestByCode_HappyPath(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		alpha string
		name  string
	}{
		"27": {"MH", "Maharashtra"},
		"29": {"KA", "Karnataka"},
		"07": {"DL", "Delhi"},
		"24": {"GJ", "Gujarat"},
		"36": {"TG", "Telangana"},
		"38": {"LA", "Ladakh"},
	}
	for code, want := range cases {
		s, err := gststate.ByCode(code)
		if err != nil {
			t.Errorf("ByCode(%q): %v", code, err)
			continue
		}
		if s.Alpha != want.alpha {
			t.Errorf("ByCode(%q) Alpha=%s want %s", code, s.Alpha, want.alpha)
		}
		if s.Name != want.name {
			t.Errorf("ByCode(%q) Name=%s want %s", code, s.Name, want.name)
		}
	}
}

func TestByCode_BareNumeric(t *testing.T) {
	t.Parallel()
	s, err := gststate.ByCode("7") // single-digit input
	if err != nil {
		t.Fatalf("ByCode(7): %v", err)
	}
	if s.GSTCode != "07" {
		t.Errorf("expected zero-padded 07, got %s", s.GSTCode)
	}
}

func TestByCode_Rejects(t *testing.T) {
	t.Parallel()
	for _, code := range []string{"", "00", "99", "ab", "999", "MH"} {
		if _, err := gststate.ByCode(code); !errors.Is(err, gststate.ErrInvalid) {
			t.Errorf("ByCode(%q): got %v want ErrInvalid", code, err)
		}
	}
}

func TestByAlpha(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"MH": "27",
		"mh": "27", // case-insensitive
		"KA": "29",
		"DL": "07",
	}
	for raw, wantCode := range cases {
		s, err := gststate.ByAlpha(raw)
		if err != nil {
			t.Errorf("ByAlpha(%q): %v", raw, err)
			continue
		}
		if s.GSTCode != wantCode {
			t.Errorf("ByAlpha(%q) code=%s want %s", raw, s.GSTCode, wantCode)
		}
	}
	for _, bad := range []string{"", "X", "XYZ", "ZZ"} {
		if _, err := gststate.ByAlpha(bad); !errors.Is(err, gststate.ErrInvalid) {
			t.Errorf("ByAlpha(%q): got %v want ErrInvalid", bad, err)
		}
	}
}

func TestByName(t *testing.T) {
	t.Parallel()
	for _, raw := range []string{
		"Maharashtra",
		"maharashtra",
		"MAHARASHTRA",
		" Maharashtra ", // trims
	} {
		s, err := gststate.ByName(raw)
		if err != nil {
			t.Errorf("ByName(%q): %v", raw, err)
		}
		if s.GSTCode != "27" {
			t.Errorf("ByName(%q) code=%s want 27", raw, s.GSTCode)
		}
	}
	for _, bad := range []string{"", "Atlantis", "California"} {
		if _, err := gststate.ByName(bad); !errors.Is(err, gststate.ErrInvalid) {
			t.Errorf("ByName(%q): got %v want ErrInvalid", bad, err)
		}
	}
}

func TestAll_CatalogueSize(t *testing.T) {
	t.Parallel()
	all := gststate.All()
	if len(all) < 36 {
		t.Errorf("catalogue size=%d want >=36 (28 states + UTs + special)", len(all))
	}
}

func TestIsKnownCode(t *testing.T) {
	t.Parallel()
	if !gststate.IsKnownCode("27") {
		t.Error("27 should be known")
	}
	if gststate.IsKnownCode("99") {
		t.Error("99 should be unknown")
	}
}

func TestAllCodes_Sorted(t *testing.T) {
	t.Parallel()
	codes := gststate.AllCodes()
	for i := 1; i < len(codes); i++ {
		if codes[i] < codes[i-1] {
			t.Errorf("AllCodes not sorted: %s came after %s", codes[i], codes[i-1])
		}
	}
}

func TestUnionTerritoryFlag(t *testing.T) {
	t.Parallel()
	cases := map[string]bool{
		"07": true,  // Delhi UT
		"34": true,  // Puducherry UT
		"27": false, // Maharashtra is a State
		"29": false, // Karnataka is a State
	}
	for code, want := range cases {
		s, _ := gststate.ByCode(code)
		if s.IsUnionTerritory != want {
			t.Errorf("ByCode(%q).IsUnionTerritory=%v want %v", code, s.IsUnionTerritory, want)
		}
	}
}
