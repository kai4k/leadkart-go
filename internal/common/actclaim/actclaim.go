// Package actclaim carries the RFC 8693 §4.1 actor claim across the
// HTTP-handler → command-handler → repository → outbox boundary via
// context.Context. Per ADR 0056 (Wave 9.2c) the impersonation context
// must reach the outbox writer so the per-message act_* metadata can be
// populated; the forwarder propagates it onto the Watermill message; the
// subscriber-side AuditMiddleware reads it back when populating
// audit_log_entry.act_* (ADR 0045).
//
// Lives in internal/common/ — it is cross-cutting context propagation
// (the analog of common/tenancy), consumed by identity + inventory + the
// shared messaging outbox helper. It was briefly duplicated per-module
// (rule-of-three, Fowler "Refactoring" ch.12); the third consumer (the
// shared messaging.PublishOutbox helper) triggered the lift to one shared
// package per ADR 0067.
//
// The shape is a tiny structural copy of jwt.ActClaim (three strings) so
// consumers don't import the jwt package — keeps the dependency arrow
// shallow. Doctrine: RFC 8693 §4.1 actor claim; OpenTelemetry Baggage as
// the analogous cross-cutting propagation design; Khorikov §10.
package actclaim

// Claim is the boundary-clean shape of the RFC 8693 act claim — three
// strings matching jwt.ActClaim 1:1. Empty Claim (zero value) means "no
// impersonation context" — the outbox writer treats it the same as
// ctx-absent.
type Claim struct {
	OperatorID string // RFC 8693 act.sub — operator's PersonID
	SessionID  string // impersonation session ID
	Reason     string // operator-supplied reason (denormalised from the session)
}

// IsZero reports whether the Claim carries no impersonation context. True
// when ALL three fields are empty — a partial Claim (e.g. only OperatorID
// set) is treated as zero per defensive-coding canon: half-populated
// metadata is a propagation bug, NOT a useful audit row.
func (c Claim) IsZero() bool {
	return c.OperatorID == "" && c.SessionID == "" && c.Reason == ""
}
