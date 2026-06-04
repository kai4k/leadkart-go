package workitem

import "fmt"

// Type is the workflow category of a [WorkItem] per BRD §6.8.
//
// Closed catalogue. Wire-stable lowercase strings — match the CHECK
// constraint on tasks.work_items.type in migration 20260604000001.
type Type string

const (
	// TypeManual is a free-form task a user authors directly (no source
	// module). The default for "I need to remember to do X."
	TypeManual Type = "manual"

	// TypeCallbackReminder is auto-created when a CallLog records a
	// callback window per BRD §6.8. The associated CrmLead /
	// callback-window timestamp seeds the task's due_at.
	TypeCallbackReminder Type = "callback_reminder"

	// TypeReorderReminder is auto-created for mature accounts (e.g.
	// 90-day post-conversion follow-up). Sources: CRM lead_converted.v1
	// + future Orders module mature-order events.
	TypeReorderReminder Type = "reorder_reminder"

	// TypeFollowUp is the manual-but-categorised follow-up (the user
	// chose "this is a follow-up", not just a generic todo).
	TypeFollowUp Type = "follow_up"

	// TypeCustom is the extensibility hatch — tenant-specific workflows
	// can categorise without forcing a new catalogue entry. Treated
	// identically to TypeManual on the hot path.
	TypeCustom Type = "custom"
)

// String returns the wire form.
func (t Type) String() string { return string(t) }

// IsValid reports whether t is a known catalogue entry.
func (t Type) IsValid() bool {
	switch t {
	case TypeManual, TypeCallbackReminder, TypeReorderReminder, TypeFollowUp, TypeCustom:
		return true
	}
	return false
}

// ParseType turns an untrusted string into a [Type] or returns
// [ErrInvalid] wrapped with the bad value.
func ParseType(raw string) (Type, error) {
	t := Type(raw)
	if !t.IsValid() {
		return "", fmt.Errorf("%w: type %q not in catalogue", ErrInvalid, raw)
	}
	return t, nil
}
