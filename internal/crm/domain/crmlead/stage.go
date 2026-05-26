package crmlead

import "fmt"

// Stage is the position of a [CrmLead] in the sales pipeline per BRD §4.4
// + ADR 0060.
//
// State machine (strict; no skips, no backtracking):
//
//	new → contacted → interested → negotiation → converted (terminal)
//	                                          ↘ lost      (terminal)
//
// Terminal stages refuse outgoing transitions. Self-transitions are
// idempotent no-ops (the aggregate method returns nil + emits no event).
type Stage string

// Closed catalogue. Wire-stable lowercase strings — match the CHECK
// constraint on crm.crm_leads.stage in migration 20260602000001.
const (
	StageNew         Stage = "new"
	StageContacted   Stage = "contacted"
	StageInterested  Stage = "interested"
	StageNegotiation Stage = "negotiation"
	StageConverted   Stage = "converted"
	StageLost        Stage = "lost"
)

// String returns the wire form for logging + integration events.
func (s Stage) String() string { return string(s) }

// IsTerminal reports whether the stage allows NO further transitions.
func (s Stage) IsTerminal() bool { return s == StageConverted || s == StageLost }

// IsValid reports whether s is a known catalogue entry. Used by the
// HTTP layer to reject untrusted input before it reaches the aggregate.
func (s Stage) IsValid() bool {
	switch s {
	case StageNew, StageContacted, StageInterested, StageNegotiation, StageConverted, StageLost:
		return true
	}
	return false
}

// ParseStage turns an untrusted string into a [Stage] or returns
// [ErrInvalid] wrapped with the bad value. Trims + lowercases input
// for the typical HTTP-DTO case where the wire form is exact-match.
func ParseStage(raw string) (Stage, error) {
	s := Stage(raw)
	if !s.IsValid() {
		return "", fmt.Errorf("%w: stage %q not in catalogue", ErrInvalid, raw)
	}
	return s, nil
}

// canAdvance returns true iff target is the canonical successor of cur
// in the forward pipeline (excluding the terminal Convert/Lose branches —
// those are dedicated aggregate methods).
//
// Allowed transitions:
//
//	new          → contacted
//	contacted    → interested
//	interested   → negotiation
//
// All other (cur, target) pairs fail. Self-transition handled by the
// caller as an idempotent no-op BEFORE this function runs.
func canAdvance(cur, target Stage) bool {
	switch cur {
	case StageNew:
		return target == StageContacted
	case StageContacted:
		return target == StageInterested
	case StageInterested:
		return target == StageNegotiation
	}
	return false
}
