// Package rolehierarchy is the aggregate that owns the parent→child
// relationships between roles within a tenant. Per ADR 0058 (Wave
// 9.4 — supersedes ADR 0054).
//
// Why an aggregate (not a column on role): per Vernon IDDD ch.7 +
// Khorikov "Pragmatic Clean Architecture" §11 — relationships with
// their own lifecycle, audit trail, or potential extensions
// (time-bound edges, approver chains, multi-parent later) deserve
// their own aggregate. The Wave 9.1d shape stashed the parent on the
// Role itself + required a SECURITY DEFINER trigger to enforce
// cross-tenant safety under RLS; the join-table aggregate makes
// cross-tenant declarative (composite FK) + audit first-class.
//
// Each Edge is ONE directed parent→child link. The single-parent
// invariant ("a child has at most one ACTIVE parent") is enforced by
// the partial unique index on (tenant_id, child_role_id) WHERE
// removed_at IS NULL.
//
// Lifecycle: Active → Removed (soft-delete). Removed edges stay
// queryable for audit — the Active/Removed shape parallels
// permissionrequest.Request's Pending/terminal split (ADR 0055).
// There is NO "re-activate"; if a removed edge is needed again,
// create a brand-new Edge (separate audit row).
//
// Industry-canon sources:
//   - Eric Evans, "Domain-Driven Design" — aggregate invariants.
//   - Vaughn Vernon, "Implementing Domain-Driven Design" ch.7 — relationships as aggregates.
//   - Vladimir Khorikov, "Pragmatic Clean Architecture" §11 — invariant layering.
//   - Stripe multi-tenant FK pattern — composite key for declarative tenant isolation.
//   - Microsoft Entra ID + Salesforce Role Hierarchy — single-parent organizational tree.
package rolehierarchy

import (
	"fmt"
	"strings"
	"time"

	"github.com/leadkart/leadkart-go/internal/common/errs"
	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/identity/domain/role"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)

// Reason length bounds — exported so HTTP DTO validators reuse the
// same numbers without redefining (no magic numbers per
// `coding-standards.md`). Mirror of the DB CHECK in migration
// 20260523000007.
const (
	// MinReasonLength matches the impersonation + permission_request
	// floor — anything shorter doesn't carry useful audit context per
	// DPDP §12 + SOC2 CC4.1.
	MinReasonLength = 10
	// MaxReasonLength matches the DB CHECK.
	MaxReasonLength = 1024
)

// ----- Sentinel errors ------------------------------------------------------

// ErrInvalidEdge is the construction-time invariant violation
// sentinel (missing id, blank actor when supplied wrong, etc).
var ErrInvalidEdge = errs.New(errs.KindInvalidInput, "rolehierarchy", "invalid hierarchy edge")

// ErrEdgeAlreadyExists is returned by the adapter when the partial
// unique index uq_role_hierarchy_active_edge_per_child refuses an
// INSERT (the child already has an active parent edge). HTTP 409.
var ErrEdgeAlreadyExists = errs.New(errs.KindAlreadyExists, "rolehierarchy", "child already has an active parent edge")

// ErrEdgeNotFound is returned by GetActiveByChild / UpdateByID when
// no matching row is visible (RLS-hidden rows are indistinguishable
// from non-existent rows per ADR 0044 enumeration safety). HTTP 404.
var ErrEdgeNotFound = errs.New(errs.KindNotFound, "rolehierarchy", "edge not found")

// ErrSelfReference is returned by New + the adapter (translating the
// chk_edge_no_self_loop CHECK) when a child is set as its own parent.
// HTTP 400.
var ErrSelfReference = errs.New(errs.KindInvalidInput, "rolehierarchy", "child cannot be its own parent")

// ErrCycle is returned by the adapter when the DB trigger
// edge_check_cycle refuses an insert that would close a multi-hop
// loop. HTTP 422.
var ErrCycle = errs.New(errs.KindInvalidInput, "rolehierarchy", "edge would create a cycle")

// ErrCrossTenant is returned by the adapter when the composite FK
// (fk_edges_*_same_tenant) refuses an insert because child + parent
// live in different tenants. HTTP 422.
var ErrCrossTenant = errs.New(errs.KindInvalidInput, "rolehierarchy", "child and parent belong to different tenants")

// ErrInvalidReason is returned when a supplied reason is non-empty
// but outside [MinReasonLength, MaxReasonLength]. HTTP 422.
var ErrInvalidReason = errs.New(errs.KindInvalidInput, "rolehierarchy", "reason must be 10-1024 chars when supplied")

// ErrNotActive is returned when Remove is called on an already-
// removed edge — fail-loud over silent no-op so callers know their
// state is stale.
var ErrNotActive = errs.New(errs.KindConflict, "rolehierarchy", "edge is already removed")

// ----- Identifier -----------------------------------------------------------

// ID is the Edge primary key (UUIDv7 string form).
type ID string

// IsZero reports whether the ID is unset.
func (i ID) IsZero() bool { return i == "" }

// String returns the underlying UUID string.
func (i ID) String() string { return string(i) }

// ----- Aggregate ------------------------------------------------------------

// Edge is the aggregate root. One row per directed parent→child link.
// Tenant-scoped via tenantID; RLS+FORCE scopes reads/writes.
type Edge struct {
	id                        ID
	tenantID                  tenant.ID
	childRoleID               role.ID
	parentRoleID              role.ID
	establishedAt             time.Time
	establishedByMembershipID membership.ID // zero = system / migration
	reason                    string        // empty allowed; if set, [MinReasonLength, MaxReasonLength]

	removedAt             time.Time     // zero = active
	removedByMembershipID membership.ID // zero when not removed
	removalReason         string        // empty when not removed

	events []Event
}

// New constructs a brand-new active Edge.
//
// `establishedBy` MAY be zero — internal-system edges (operator-set
// during onboarding, migrated rows) carry no actor.
//
// `reason` is OPTIONAL; when supplied it must be in
// [MinReasonLength, MaxReasonLength] (trimmed). Empty after trim =
// no reason.
//
// Direct self-reference (childRoleID == parentRoleID) is rejected
// here with [ErrSelfReference] — the DB CHECK
// chk_edge_no_self_loop is the strict-gate fallback. Multi-hop
// cycle + cross-tenant rejection happen at persist time (cycle
// trigger + composite FK).
func New(
	id ID,
	tenantID tenant.ID,
	childRoleID, parentRoleID role.ID,
	establishedBy membership.ID,
	reason string,
	now time.Time,
) (*Edge, error) {
	if id.IsZero() {
		return nil, fmt.Errorf("%w: id required", ErrInvalidEdge)
	}
	if tenantID.IsZero() {
		return nil, fmt.Errorf("%w: tenantID required", ErrInvalidEdge)
	}
	if childRoleID.IsZero() {
		return nil, fmt.Errorf("%w: childRoleID required", ErrInvalidEdge)
	}
	if parentRoleID.IsZero() {
		return nil, fmt.Errorf("%w: parentRoleID required", ErrInvalidEdge)
	}
	if childRoleID == parentRoleID {
		return nil, fmt.Errorf("%w: child=%s", ErrSelfReference, childRoleID)
	}
	trimmed, err := validateReason(reason)
	if err != nil {
		return nil, err
	}

	e := &Edge{
		id:                        id,
		tenantID:                  tenantID,
		childRoleID:               childRoleID,
		parentRoleID:              parentRoleID,
		establishedAt:             now.UTC(),
		establishedByMembershipID: establishedBy,
		reason:                    trimmed,
	}
	e.recordEvent(EstablishedEvent{
		ID:                        id,
		TenantID:                  tenantID,
		ChildRoleID:               childRoleID,
		ParentRoleID:              parentRoleID,
		EstablishedByMembershipID: establishedBy,
		Reason:                    trimmed,
		At:                        now.UTC(),
	})
	return e, nil
}

// ----- Getters --------------------------------------------------------------

// ID returns the Edge primary key.
func (e *Edge) ID() ID { return e.id }

// TenantID returns the tenant scope.
func (e *Edge) TenantID() tenant.ID { return e.tenantID }

// ChildRoleID returns the child role.
func (e *Edge) ChildRoleID() role.ID { return e.childRoleID }

// ParentRoleID returns the parent role.
func (e *Edge) ParentRoleID() role.ID { return e.parentRoleID }

// EstablishedAt returns the immutable creation timestamp.
func (e *Edge) EstablishedAt() time.Time { return e.establishedAt }

// EstablishedByMembershipID returns the actor that created the edge.
// Zero for system / migration edges.
func (e *Edge) EstablishedByMembershipID() membership.ID { return e.establishedByMembershipID }

// Reason returns the establishment justification. Empty when none
// was supplied.
func (e *Edge) Reason() string { return e.reason }

// RemovedAt returns the soft-delete timestamp. Zero while active.
func (e *Edge) RemovedAt() time.Time { return e.removedAt }

// RemovedByMembershipID returns the actor that removed the edge.
// Zero when active OR when a system path removed it.
func (e *Edge) RemovedByMembershipID() membership.ID { return e.removedByMembershipID }

// RemovalReason returns the removal justification. Empty while
// active OR when none was supplied.
func (e *Edge) RemovalReason() string { return e.removalReason }

// IsActive reports whether the edge is currently in force.
func (e *Edge) IsActive() bool { return e.removedAt.IsZero() }

// ----- State transitions ---------------------------------------------------

// Remove soft-deletes the edge. Returns [ErrNotActive] if already
// removed (fail-loud — re-Remove is a programmer error, not
// idempotent; mirrors permissionrequest.Cancel's discipline).
//
// `removedBy` MAY be zero — system paths (cascade subscribers,
// tenant-tear-down) carry no actor.
//
// `reason` is OPTIONAL; same bounds as construction-time reason.
func (e *Edge) Remove(removedBy membership.ID, reason string, now time.Time) error {
	if !e.IsActive() {
		return fmt.Errorf("%w: removed_at=%s", ErrNotActive, e.removedAt)
	}
	trimmed, err := validateReason(reason)
	if err != nil {
		return err
	}
	e.removedAt = now.UTC()
	e.removedByMembershipID = removedBy
	e.removalReason = trimmed
	e.recordEvent(RemovedEvent{
		ID:                    e.id,
		TenantID:              e.tenantID,
		ChildRoleID:           e.childRoleID,
		ParentRoleID:          e.parentRoleID,
		RemovedByMembershipID: removedBy,
		Reason:                trimmed,
		At:                    now.UTC(),
	})
	return nil
}

// ----- Persistence DTO -----------------------------------------------------

// Snapshot is the persistence DTO consumed by [UnmarshalFromDB].
// Mirror of the identity.role_hierarchy_edges column shape.
type Snapshot struct {
	ID                        ID
	TenantID                  tenant.ID
	ChildRoleID               role.ID
	ParentRoleID              role.ID
	EstablishedAt             time.Time
	EstablishedByMembershipID membership.ID
	Reason                    string
	RemovedAt                 time.Time
	RemovedByMembershipID     membership.ID
	RemovalReason             string
}

// UnmarshalFromDB rehydrates an Edge from persistence. Repository-only
// path; does NOT re-validate (TDL canon — DB-stored data is already
// invariant-checked at write time). Does NOT emit domain events.
func UnmarshalFromDB(s Snapshot) *Edge {
	return &Edge{
		id:                        s.ID,
		tenantID:                  s.TenantID,
		childRoleID:               s.ChildRoleID,
		parentRoleID:              s.ParentRoleID,
		establishedAt:             s.EstablishedAt,
		establishedByMembershipID: s.EstablishedByMembershipID,
		reason:                    s.Reason,
		removedAt:                 s.RemovedAt,
		removedByMembershipID:     s.RemovedByMembershipID,
		removalReason:             s.RemovalReason,
	}
}

// ----- Event handling ------------------------------------------------------

// PullEvents drains recorded domain events. Repository calls this
// after a successful persist, then writes each event into the
// outbox in the same transaction (TDL UpdateFn pattern per
// ADR 0004 + 0008).
func (e *Edge) PullEvents() []Event {
	if len(e.events) == 0 {
		return nil
	}
	out := e.events
	e.events = nil
	return out
}

func (e *Edge) recordEvent(ev Event) {
	e.events = append(e.events, ev)
}

// validateReason trims + bounds-checks an optional reason. Returns
// trimmed string OR [ErrInvalidReason] (wrapped). Empty trim = empty
// trimmed (legal — reason is nullable).
func validateReason(raw string) (string, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", nil
	}
	if len(trimmed) < MinReasonLength || len(trimmed) > MaxReasonLength {
		return "", fmt.Errorf("%w: length %d not in [%d, %d]",
			ErrInvalidReason, len(trimmed), MinReasonLength, MaxReasonLength)
	}
	return trimmed, nil
}
