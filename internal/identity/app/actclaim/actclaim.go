// Package actclaim carries the RFC 8693 §4.1 actor claim across the
// HTTP-handler → command-handler → repository → outbox boundary via
// context.Context. Per ADR 0056 (Wave 9.2c) — the impersonation
// context must reach the outbox writer so the per-row act_* columns
// can be populated; the forwarder then propagates them onto the
// Watermill message metadata; the subscriber-side AuditMiddleware
// reads them back when populating audit_log_entry.act_* (ADR 0045).
//
// Design choice: a dedicated app-tier package (not authn's ctx key)
// because:
//
//   - authn lives in ports/ — adapters/ MUST NOT import ports/ per
//     TDL hexagonal layering (ADR 0047).
//   - app/ is the boundary-clean pivot: ports/authn populates this ctx
//     key after JWT verification; adapters/outbox_writer reads it
//     when assembling each outbox row.
//   - The shape is a tiny structural copy of *jwt.ActClaim (not the
//     pointer itself) so consumers of this package don't need to
//     import jwt — keeps the dependency arrow shallow.
//
// Doctrine sources: RFC 8693 §4.1 actor claim canon; OpenTelemetry
// Baggage as the analogous "cross-cutting context propagation"
// design; Khorikov §10 "Application services as orchestrators of
// boundary translations".
package actclaim

// Claim is the boundary-clean shape of the RFC 8693 act claim — three
// strings matching jwt.ActClaim 1:1. Kept as a separate type so the
// adapters package can read the propagated context without importing
// the app/jwt package (which would otherwise force adapters → app/jwt
// → domain). Stripe / Auth0 canon: cross-cutting context VOs travel
// as plain structs, not as the wire-format type.
//
// Empty Claim (zero value) means "no impersonation context" — outbox
// writer treats it the same as ctx-absent.
type Claim struct {
	OperatorID string // RFC 8693 act.sub — operator's PersonID
	SessionID  string // impersonation session ID
	Reason     string // operator-supplied reason (denormalised from the session)
}

// IsZero reports whether the Claim carries no impersonation context.
// True when ALL three fields are empty — a partial Claim (e.g. only
// OperatorID set) is treated as zero per defensive-coding canon: half-
// populated metadata is a propagation bug, NOT a useful audit row.
func (c Claim) IsZero() bool {
	return c.OperatorID == "" && c.SessionID == "" && c.Reason == ""
}
