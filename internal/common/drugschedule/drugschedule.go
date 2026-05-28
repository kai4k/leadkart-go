// Package drugschedule defines the [Schedule] enum — the Indian Drugs
// & Cosmetics Act classification a pharma SKU carries. Per BRD §C.2.
//
// Six values; sourced from the BRD table verbatim:
//
//	OTC            — No prescription needed; no restriction.
//	ScheduleH      — Prescription required (antibiotics, antihypertensives,
//	                  antidiabetics, steroids). Informational flag at Phase 1.
//	ScheduleH1     — High-risk subset of Schedule H (specific antibiotics,
//	                  anti-TB). Informational flag at Phase 1.
//	ScheduleX      — Narcotics / psychotropics — Form 20F/20G licence.
//	                  Dispatch WARNING.
//	ScheduleC      — Biologicals (insulin, vaccines, cold chain).
//	                  Dispatch WARNING.
//	NotApplicable  — Nutraceuticals, Ayurvedic, cosmetics. No restriction.
//
// Phase 1 (per BRD §C.2): informational only — values stored on the
// Product master + surfaced on dispatch screens. Phase 2: hard
// enforcement (block sale of Schedule X to non-licensed buyers, etc).
//
// This package is a thin VO + parser — the enforcement gate lives in
// the consuming module (Inventory + Dispatch) when v0.2 promotes the
// flag to a rule.
package drugschedule

import (
	"errors"
	"fmt"
)

// ErrInvalid is the sentinel returned by [Parse] for unknown values.
var ErrInvalid = errors.New("drugschedule: invalid")

// Schedule is the validated classification.
type Schedule string

// Closed catalogue. Wire-stable lowercase strings — the storage
// migration's CHECK constraint pins exactly these six values.
const (
	OTC           Schedule = "otc"
	ScheduleH     Schedule = "schedule_h"
	ScheduleH1    Schedule = "schedule_h1"
	ScheduleX     Schedule = "schedule_x"
	ScheduleC     Schedule = "schedule_c"
	NotApplicable Schedule = "not_applicable"
)

// String returns the wire form.
func (s Schedule) String() string { return string(s) }

// IsValid reports whether s is a known catalogue entry.
func (s Schedule) IsValid() bool {
	switch s {
	case OTC, ScheduleH, ScheduleH1, ScheduleX, ScheduleC, NotApplicable:
		return true
	}
	return false
}

// RequiresPrescription reports whether the schedule mandates a
// prescription at the point of sale. ScheduleH + ScheduleH1 + ScheduleX
// all require one; ScheduleC depends on the specific biological (cold
// chain handling is the operational distinction, not the prescription
// requirement) so we conservatively flag TRUE — the Dispatch warning
// surface treats prescription-required as a positive signal.
func (s Schedule) RequiresPrescription() bool {
	switch s {
	case ScheduleH, ScheduleH1, ScheduleX, ScheduleC:
		return true
	}
	return false
}

// RequiresColdChain reports whether the schedule mandates cold-chain
// handling per BRD §C.2 + India's Pharmacy Council guidelines. Only
// ScheduleC (biologicals — insulin, vaccines) triggers the cold-chain
// flag at v0.2. Dispatch SHOULD reject creating a ConsignmentNote
// without cold-chain carrier metadata for any line item with
// RequiresColdChain=true.
func (s Schedule) RequiresColdChain() bool {
	return s == ScheduleC
}

// IsNarcotic reports whether the schedule covers narcotics /
// psychotropics requiring Form 20F/20G. ScheduleX is the only entry
// today; v0.3 may split into 20F vs 20G as separate values when the
// regulatory team needs the distinction.
func (s Schedule) IsNarcotic() bool {
	return s == ScheduleX
}

// Parse turns an untrusted string into a Schedule or returns
// [ErrInvalid]. Case-sensitive — wire callers MUST send the canonical
// lowercase form.
func Parse(raw string) (Schedule, error) {
	s := Schedule(raw)
	if !s.IsValid() {
		return "", fmt.Errorf("%w: schedule %q not in catalogue", ErrInvalid, raw)
	}
	return s, nil
}

// All returns a defensive copy of the catalogue — useful for HTTP
// reference-data endpoints + admin dropdown menus.
func All() []Schedule {
	return []Schedule{OTC, ScheduleH, ScheduleH1, ScheduleX, ScheduleC, NotApplicable}
}
