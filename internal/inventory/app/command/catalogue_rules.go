package command

import (
	"errors"
	"slices"

	"github.com/leadkart/leadkart-go/internal/common/commercepolicy"
)

// Stable refusal codes of the company-list product fields, carried in
// errors{field:[code]}.
const (
	CodeRequired      = "required"
	CodeNotInList     = "not_in_list"
	CodeInactive      = "inactive"
	CodeScheduleIsOff = "schedule_off"
)

// ErrFieldRefused is matched by a [FieldRefusal].
var ErrFieldRefused = errors.New("inventory: field refused")

// FieldRefusal is a refusal that names its fields: field name to stable codes.
// The HTTP layer answers 422 with errors{field:[code]}.
type FieldRefusal struct{ Fields map[string][]string }

func (e *FieldRefusal) Error() string { return "inventory: field refused" }

// Is makes errors.Is(err, ErrFieldRefused) match.
func (e *FieldRefusal) Is(target error) bool { return target == ErrFieldRefused }

// catalogueChoice is what a create or update asks for in the company lists.
type catalogueChoice struct {
	// GSTRateID is the requested rate list entry ("" = not given).
	GSTRateID string
	// RequireGST is true on create: a product must name a rate, there is no default.
	RequireGST bool
	// CurrentGSTRateID is the product existing entry (update): keeping it needs no check.
	CurrentGSTRateID string
	// DrugSchedule is the requested class id; Given says whether the request named
	// the field at all (clearing is Given with "").
	DrugSchedule  string
	ScheduleGiven bool
	// CurrentDrugSchedule is the product's existing class id (update): keeping it needs no check
	// even once the class is inactive (ADR 0073).
	CurrentDrugSchedule string
	Create              bool
}

// resolveCatalogue checks the company-list fields against the policy and returns
// the GST rate (basis points) of the chosen entry. A refusal is a [*FieldRefusal].
//
// GST: the rate comes ONLY from the company list; there is no default rate and no
// category fallback. Drug schedule: nullable at every mode except where the company
// itself chose `required`; the class must be one of the company own list; under
// mode `off` a class cannot be set (clearing one is always allowed). The mode is
// read from the replica, which lags the company settings, so the company-side
// refusal to turn the mode off while products carry a class is paired with this one.
func resolveCatalogue(p commercepolicy.Policy, c catalogueChoice) (rateBps int, refusal *FieldRefusal) {
	fields := map[string][]string{}
	add := func(field, code string) {
		if !slices.Contains(fields[field], code) {
			fields[field] = append(fields[field], code)
		}
	}
	rateBps = -1
	switch {
	case c.GSTRateID == "" && c.RequireGST:
		add("gst_rate_id", CodeRequired)
	case c.GSTRateID != "":
		entry, found := findRate(p, c.GSTRateID)
		switch {
		case !found:
			add("gst_rate_id", CodeNotInList)
		case !entry.Active && c.GSTRateID != c.CurrentGSTRateID:
			add("gst_rate_id", CodeInactive)
		default:
			rateBps = entry.RateBps
		}
	}
	if code := scheduleCode(p, c); code != "" {
		add("drug_schedule", code)
	}
	if len(fields) == 0 {
		return rateBps, nil
	}
	return rateBps, &FieldRefusal{Fields: fields}
}

// scheduleCode is the refusal code of the requested drug schedule class, "" when it is accepted.
func scheduleCode(p commercepolicy.Policy, c catalogueChoice) string {
	mode := p.DrugSchedule.Mode
	switch {
	case c.ScheduleGiven && c.DrugSchedule != "":
		class, found := p.DrugSchedule.ScheduleClassByID(c.DrugSchedule)
		switch {
		case mode == commercepolicy.DrugScheduleOff:
			return CodeScheduleIsOff
		case !found:
			return CodeNotInList
		case !class.Active && c.DrugSchedule != c.CurrentDrugSchedule:
			// An inactive class cannot be set on a product; a product already carrying it keeps it.
			return CodeInactive
		}
	case mode == commercepolicy.DrugScheduleRequired && (c.Create && !c.ScheduleGiven || c.ScheduleGiven && c.DrugSchedule == ""):
		return CodeRequired
	}
	return ""
}

func findRate(p commercepolicy.Policy, id string) (commercepolicy.GSTRate, bool) {
	for _, r := range p.Tax.GSTRates {
		if r.ID == id {
			return r, true
		}
	}
	return commercepolicy.GSTRate{}, false
}
