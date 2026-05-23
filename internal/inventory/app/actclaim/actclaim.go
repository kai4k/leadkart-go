// Package actclaim carries the RFC 8693 §4.1 actor claim across the
// HTTP-handler → command-handler → repository → outbox boundary via
// context.Context. Local copy of the identity actclaim package per
// CLAUDE.md "Architecture rule 1: modules NEVER reference each other's
// domain/app/ports/adapters" + ADR 0051 "single-module rule".
//
// Why duplicate rather than lift to internal/common/: only TWO consumers
// today (identity + inventory). Per the "rule of three" canon (Fowler
// "Refactoring" ch.12 §"Three strikes and you refactor"), early
// duplication beats premature abstraction. When a THIRD bounded context
// needs act-claim propagation, lift this to internal/common/actclaim/
// and switch both modules to the shared package — see ADR 0061 §5
// (amendment).
//
// Shape is byte-identical to internal/identity/app/actclaim/ so the lift
// will be a mechanical import-path rewrite.
//
// Doctrine sources: RFC 8693 §4.1 actor claim canon; OpenTelemetry
// Baggage as the analogous "cross-cutting context propagation" design;
// Khorikov §10 "Application services as orchestrators of boundary
// translations".
package actclaim

// Claim is the boundary-clean shape of the RFC 8693 act claim — three
// strings matching jwt.ActClaim 1:1. Empty Claim (zero value) means
// "no impersonation context" — outbox writer treats it the same as
// ctx-absent.
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
