package workitem

import "fmt"

// Priority is the urgency band of a [WorkItem] per BRD §6.8.
//
// Closed catalogue. Wire-stable lowercase strings — match the CHECK
// constraint on tasks.work_items.priority in migration 20260604000001.
type Priority string

const (
	// PriorityLow — background / catch-up work.
	PriorityLow Priority = "low"
	// PriorityMedium — default for ad-hoc tasks.
	PriorityMedium Priority = "medium"
	// PriorityHigh — needs same-day attention.
	PriorityHigh Priority = "high"
	// PriorityUrgent — drop-everything.
	PriorityUrgent Priority = "urgent"
)

// String returns the wire form.
func (p Priority) String() string { return string(p) }

// IsValid reports whether p is a known catalogue entry.
func (p Priority) IsValid() bool {
	switch p {
	case PriorityLow, PriorityMedium, PriorityHigh, PriorityUrgent:
		return true
	}
	return false
}

// ParsePriority turns an untrusted string into a [Priority] or returns
// [ErrInvalid] wrapped with the bad value. Empty input defaults to
// [PriorityMedium] (the most-common create-flow shape).
func ParsePriority(raw string) (Priority, error) {
	if raw == "" {
		return PriorityMedium, nil
	}
	p := Priority(raw)
	if !p.IsValid() {
		return "", fmt.Errorf("%w: priority %q not in catalogue", ErrInvalid, raw)
	}
	return p, nil
}
