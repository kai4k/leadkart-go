package commercepolicy_test

import (
	"slices"
	"testing"

	"github.com/google/uuid"

	"github.com/leadkart/leadkart-go/internal/common/commercepolicy"
)

func TestDefault_MarketDefaultsComeFromTheEmbeddedData(t *testing.T) {
	t.Parallel()
	d := commercepolicy.Default()
	if d.Expiry.BusinessDayUTCOffsetMinutes != 330 {
		t.Errorf("business day offset default = %d, want the embedded data value 330", d.Expiry.BusinessDayUTCOffsetMinutes)
	}
	if len(d.DrugSchedule.Classes) == 0 {
		t.Fatal("the company starts with the default classes of its market")
	}
	seen := map[string]bool{}
	for _, c := range d.DrugSchedule.Classes {
		if _, err := uuid.Parse(c.ID); err != nil || c.Label == "" || seen[c.ID] {
			t.Errorf("bad default class %+v", c)
		}
		seen[c.ID] = true
	}
	// Defaults are copies: a caller changing one policy never changes the next Default().
	d.DrugSchedule.Classes[0].Label = "changed"
	if commercepolicy.Default().DrugSchedule.Classes[0].Label == "changed" {
		t.Fatal("Default() shares its class list between callers")
	}
}

func TestApply_TheClassListChangesThroughItsOwnRoutesAndTheOffsetIsBounded(t *testing.T) {
	t.Parallel()
	p := commercepolicy.Default()
	_, errs := applyAt(p, changes(t, map[string]any{"drug_schedule.classes": []any{}}))
	if !slices.Equal(errs["drug_schedule.classes"], []string{commercepolicy.CodeManagedElsewhere}) {
		t.Fatalf("classes through the patch: %v", errs)
	}
	// The offset is market data, not a setting: a patch naming it is refused, so no
	// company can move the day that the legal expiry limit is judged on.
	for _, off := range []int{0, -720, 330, 840} {
		_, errs := applyAt(p, changes(t, map[string]any{"expiry.business_day_utc_offset_minutes": off}))
		if !slices.Equal(errs["expiry.business_day_utc_offset_minutes"], []string{commercepolicy.CodeUnknownSetting}) {
			t.Errorf("offset %d patch: %v, want unknown_setting", off, errs)
		}
	}
	for _, info := range commercepolicy.Describe() {
		if info.Key == "expiry.business_day_utc_offset_minutes" {
			t.Fatal("the offset must not be offered as a setting")
		}
	}
	bad := commercepolicy.Default()
	bad.Expiry.BusinessDayUTCOffsetMinutes = 900
	if len(commercepolicy.Validate(bad)["expiry.business_day_utc_offset_minutes"]) == 0 {
		t.Error("a stored offset outside real time zones must not validate")
	}
}

func TestValidate_ClassListRules(t *testing.T) {
	t.Parallel()
	mk := func(classes ...commercepolicy.ScheduleClass) commercepolicy.Policy {
		p := commercepolicy.Default()
		p.DrugSchedule.Classes = classes
		return p
	}
	a := commercepolicy.ScheduleClass{ID: uuid.NewString(), Label: "Class A"}
	if errs := commercepolicy.Validate(mk()); len(errs) != 0 {
		t.Errorf("a company may remove every class: %v", errs)
	}
	if errs := commercepolicy.Validate(mk(a, commercepolicy.ScheduleClass{ID: uuid.NewString(), Label: " class a "})); !slices.Contains(errs["drug_schedule.classes"], commercepolicy.CodeDuplicate) {
		t.Errorf("a repeated label (any case): %v", errs)
	}
	if errs := commercepolicy.Validate(mk(commercepolicy.ScheduleClass{ID: "not-a-uuid", Label: "B"})); len(errs["drug_schedule.classes"]) == 0 {
		t.Error("a class id must be a uuid")
	}
	if errs := commercepolicy.Validate(mk(commercepolicy.ScheduleClass{ID: uuid.NewString(), Label: "  "})); len(errs["drug_schedule.classes"]) == 0 {
		t.Error("a blank label")
	}
	many := make([]commercepolicy.ScheduleClass, 0, commercepolicy.MaxScheduleClasses+1)
	for i := range commercepolicy.MaxScheduleClasses + 1 {
		many = append(many, commercepolicy.ScheduleClass{ID: uuid.NewString(), Label: "L" + string(rune('A'+i))})
	}
	if errs := commercepolicy.Validate(mk(many...)); !slices.Contains(errs["drug_schedule.classes"], commercepolicy.CodeTooMany) {
		t.Errorf("past the cap: %v", errs)
	}
}
