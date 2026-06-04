package reminder

import "fmt"

// Type is the source classification for a [Reminder] per BRD §4.6.
//
// Three concrete sources at slice 1:
//
//   - [TypeCallback]    — auto-created by the CallLogged subscriber when
//     the caller stamped a callback window on the call (BRD §4.5).
//   - [TypeMatureLead]  — auto-created by the mature-lead daily scan
//     (BRD §4.7) for converted leads with no reorder activity in the
//     last 3 months.
//   - [TypeManual]      — created by a sales executive / manager via
//     POST /api/v1/crm/leads/{leadId}/reminders (HTTP path).
//
// Wire-stable lowercase strings — match the CHECK constraint on
// crm.reminders.type in the slice-1 migration.
type Type string

const (
	// TypeCallback denotes a reminder minted by the CallLogged
	// subscriber from a call that requested a callback window.
	TypeCallback Type = "callback"

	// TypeMatureLead denotes a reminder minted by the mature-lead
	// scheduler (3-month no-reorder rule per BRD §4.7).
	TypeMatureLead Type = "mature_lead"

	// TypeManual denotes a reminder created directly by a user from
	// the HTTP surface.
	TypeManual Type = "manual"
)

// String returns the wire form.
func (t Type) String() string { return string(t) }

// IsValid reports whether t is a known catalogue entry.
func (t Type) IsValid() bool {
	switch t {
	case TypeCallback, TypeMatureLead, TypeManual:
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
