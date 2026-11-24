package commercepolicy

import (
	_ "embed" // defaults.json: the starting values of a company with no stored policy
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/google/uuid"
)

// Drug schedule class limits.
const (
	ScheduleClassLabelMaxChars = 40
	// MaxScheduleClasses caps a company's list; the list rides in every policy
	// event, so it stays small.
	MaxScheduleClasses = 30
)

// ScheduleClass is one entry of a company's drug schedule class list. A
// product stores the class ID, so a class can be renamed without touching a
// product. Which classes exist is company data: nothing in the code names one.
//
// A class is made inactive, never deleted (ADR 0073): an inactive class cannot be set on a
// product, and a product that already carries it keeps it.
type ScheduleClass struct {
	ID     string `json:"id"`
	Label  string `json:"label"`
	Active bool   `json:"active"`
}

// defaults is the embedded data that is specific to the market LeadKart
// launches in. It is data, not code: another market ships another file.
type defaults struct {
	BusinessDayUTCOffsetMinutes int `json:"business_day_utc_offset_minutes"`
	DrugScheduleClasses         []struct {
		ID    string `json:"id"`
		Label string `json:"label"`
	} `json:"drug_schedule_classes"`
}

//go:embed defaults.json
var defaultsJSON []byte

var loadDefaults = sync.OnceValue(func() defaults {
	var d defaults
	if err := json.Unmarshal(defaultsJSON, &d); err != nil {
		panic(fmt.Sprintf("commercepolicy: embedded defaults.json: %v", err))
	}
	return d
})

func defaultData() defaults { return loadDefaults() }

// scheduleClasses returns a fresh copy of the default class list.
func (d defaults) scheduleClasses() []ScheduleClass {
	out := make([]ScheduleClass, 0, len(d.DrugScheduleClasses))
	for _, c := range d.DrugScheduleClasses {
		out = append(out, ScheduleClass{ID: c.ID, Label: c.Label, Active: true})
	}
	return out
}

// CheckScheduleClassLabel returns the field errors of a class label (wire name
// "label").
func CheckScheduleClassLabel(label string) FieldErrors {
	errs := FieldErrors{}
	checkLine(errs, "label", label, ScheduleClassLabelMaxChars)
	return errs
}

// NormalizeScheduleLabel is the stored form of a class label: trimmed.
func NormalizeScheduleLabel(s string) string { return strings.TrimSpace(s) }

// SameScheduleLabel reports whether two labels are the same class name
// (case-insensitive), which a company's list holds once.
func SameScheduleLabel(a, b string) bool {
	return strings.EqualFold(NormalizeScheduleLabel(a), NormalizeScheduleLabel(b))
}

// SortScheduleClasses orders a list by label, then ID: the one order every
// reader sees.
func SortScheduleClasses(classes []ScheduleClass) {
	slices.SortFunc(classes, func(a, b ScheduleClass) int {
		if c := strings.Compare(strings.ToLower(a.Label), strings.ToLower(b.Label)); c != 0 {
			return c
		}
		return strings.Compare(a.ID, b.ID)
	})
}

// ScheduleClassByID returns the class with the id.
func (d DrugSchedule) ScheduleClassByID(id string) (ScheduleClass, bool) {
	for _, c := range d.Classes {
		if c.ID == id {
			return c, true
		}
	}
	return ScheduleClass{}, false
}

// validateScheduleClasses checks the list as stored: each entry valid, no
// duplicate id or active label, within the cap.
func validateScheduleClasses(classes []ScheduleClass) []string {
	var codes []string
	if len(classes) > MaxScheduleClasses {
		codes = append(codes, CodeTooMany)
	}
	for i, c := range classes {
		if _, err := uuid.Parse(c.ID); err != nil {
			codes = append(codes, CodeInvalidFormat)
		}
		if len(CheckScheduleClassLabel(c.Label)) > 0 || utf8.RuneCountInString(strings.TrimSpace(c.Label)) == 0 {
			codes = append(codes, CodeInvalidFormat)
		}
		for _, o := range classes[i+1:] {
			// A label is unique among ACTIVE classes only: a deleted (inactive) class frees it.
			if o.ID == c.ID || (o.Active && c.Active && SameScheduleLabel(o.Label, c.Label)) {
				codes = append(codes, CodeDuplicate)
			}
		}
	}
	return codes
}
