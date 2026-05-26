package drugschedule_test

import (
	"errors"
	"testing"

	"github.com/leadkart/leadkart-go/internal/common/drugschedule"
)

func TestParse_HappyPath(t *testing.T) {
	t.Parallel()
	cases := map[string]drugschedule.Schedule{
		"otc":            drugschedule.OTC,
		"schedule_h":     drugschedule.ScheduleH,
		"schedule_h1":    drugschedule.ScheduleH1,
		"schedule_x":     drugschedule.ScheduleX,
		"schedule_c":     drugschedule.ScheduleC,
		"not_applicable": drugschedule.NotApplicable,
	}
	for raw, want := range cases {
		got, err := drugschedule.Parse(raw)
		if err != nil {
			t.Errorf("Parse(%q): %v", raw, err)
		}
		if got != want {
			t.Errorf("Parse(%q) = %s want %s", raw, got, want)
		}
	}
}

func TestParse_Rejects(t *testing.T) {
	t.Parallel()
	for _, raw := range []string{
		"",
		"OTC",          // case-sensitive
		"Schedule H",   // wrong format (spaces)
		"schedule-h",   // wrong separator
		"NotApplicable", // wrong case
		"garbage",
		"schedule_z",   // not a real value
	} {
		if _, err := drugschedule.Parse(raw); !errors.Is(err, drugschedule.ErrInvalid) {
			t.Errorf("Parse(%q): got %v want ErrInvalid", raw, err)
		}
	}
}

func TestRequiresPrescription(t *testing.T) {
	t.Parallel()
	cases := map[drugschedule.Schedule]bool{
		drugschedule.OTC:           false,
		drugschedule.ScheduleH:     true,
		drugschedule.ScheduleH1:    true,
		drugschedule.ScheduleX:     true,
		drugschedule.ScheduleC:     true,  // cold-chain biologicals — conservative TRUE
		drugschedule.NotApplicable: false, // nutraceuticals / cosmetics
	}
	for s, want := range cases {
		if got := s.RequiresPrescription(); got != want {
			t.Errorf("%s.RequiresPrescription() = %v want %v", s, got, want)
		}
	}
}

func TestRequiresColdChain(t *testing.T) {
	t.Parallel()
	for _, s := range []drugschedule.Schedule{
		drugschedule.OTC, drugschedule.ScheduleH, drugschedule.ScheduleH1,
		drugschedule.ScheduleX, drugschedule.NotApplicable,
	} {
		if s.RequiresColdChain() {
			t.Errorf("%s.RequiresColdChain() = true want false", s)
		}
	}
	if !drugschedule.ScheduleC.RequiresColdChain() {
		t.Errorf("ScheduleC.RequiresColdChain() = false want true")
	}
}

func TestIsNarcotic(t *testing.T) {
	t.Parallel()
	for _, s := range []drugschedule.Schedule{
		drugschedule.OTC, drugschedule.ScheduleH, drugschedule.ScheduleH1,
		drugschedule.ScheduleC, drugschedule.NotApplicable,
	} {
		if s.IsNarcotic() {
			t.Errorf("%s.IsNarcotic() = true want false", s)
		}
	}
	if !drugschedule.ScheduleX.IsNarcotic() {
		t.Errorf("ScheduleX.IsNarcotic() = false want true")
	}
}

func TestAll_CatalogueComplete(t *testing.T) {
	t.Parallel()
	all := drugschedule.All()
	if len(all) != 6 {
		t.Errorf("All() len=%d want 6 (BRD §C.2 fixes the catalogue at 6)", len(all))
	}
	for _, s := range all {
		if !s.IsValid() {
			t.Errorf("All() contains invalid %s", s)
		}
	}
}
