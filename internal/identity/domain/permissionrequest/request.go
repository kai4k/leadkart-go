// Package permissionrequest defines the [Request] aggregate per ADR
// 0055 — the permission-elevation approval workflow.
//
// Lifecycle:
//
//	Pending → Approved (success)
//	        → Denied   (rejected by approver)
//	        → Cancelled (withdrawn by requester)
//
// Approved is terminal from the workflow's POV; the actual permission
// grant lives as a time-bound [membership.GrantedOverride] entry on
// the requester's membership overlay. The aggregate keeps Approved
// (NOT "Expired") even after the grant has elapsed — the Request row
// stays as audit history per Vernon IDDD ch. 7.
//
// Invariants:
//   - id, tenantID, requesterMembershipID, permission all non-zero.
//   - durationDays in [1, MaxDurationDays].
//   - reason length ≥ MinReasonLength.
//   - At most one Pending Request per (requesterMembership, permission)
//     tuple — DB-level partial unique index, surfaced as
//     [ErrPendingRequestExists] by the adapter.
//   - Self-approval forbidden — approverMembershipID MUST differ from
//     requesterMembershipID at [Request.Approve] time.
//
// Industry-canon sources for shape + thresholds:
//   - AWS IAM — session policies + STS AssumeRole (time-bound elevation).
//   - Microsoft Entra ID PIM — Just-In-Time access (time-bound grants,
//     approver required, audit trail).
//   - Okta Workflow — manager-approval pattern.
//   - OWASP Authentication / Authorization Cheat Sheet 2025 — bounded
//     privilege escalation requires an audit trail.
package permissionrequest

import (
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/leadkart/leadkart-go/internal/common/errs"
	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/identity/domain/permission"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)

// Reason length + duration bounds — exported so admin UIs + HTTP DTO
// validators reuse the same numbers without redefining (no magic
// strings / numbers per `coding-standards.md`).
const (
	// MinReasonLength matches the impersonation session reason floor —
	// "≥10 chars" is the threshold below which "click click click
	// elevate" notes don't carry useful context per DPDP §12 audit
	// guidance.
	MinReasonLength = 10
	// MaxReasonLength matches the DB CHECK in migration 20260523000003.
	MaxReasonLength = 1024
	// MaxDecisionReasonLength matches the DB CHECK for decision_reason.
	MaxDecisionReasonLength = 1024
	// MinDurationDays is the smallest grant window the approval flow
	// supports — anything shorter would force re-requests too frequently
	// and erode the audit value.
	MinDurationDays = 1
	// MaxDurationDays caps the JIT window per AWS IAM session-policy
	// canon (max 12h on STS; we choose 90 days as the upper bound here
	// since the SaaS world is less hostile than IAM root-account world).
	MaxDurationDays = 90
	// DefaultDurationDays is what handlers pass when the caller omits
	// the field. One business week — short enough that operators stay
	// vigilant, long enough that callers don't constantly re-request.
	DefaultDurationDays = 7
)

// ----- Sentinel errors ------------------------------------------------------

// ErrInvalidRequest is the sentinel for construction-time invariant
// violations (missing id, short reason, out-of-bounds duration).
var ErrInvalidRequest = errs.New(errs.KindInvalidInput, "permission_request", "invalid permission request")

// ErrNotPending surfaces when a state-transition method is called on a
// non-Pending request (re-Approve, Approve-after-Deny, Cancel-after-
// Approve, etc.). HTTP layer maps to 409 Conflict.
var ErrNotPending = errs.New(errs.KindConflict, "permission_request", "permission request is not pending")

// ErrSelfApproval surfaces when an approver tries to approve a request
// they submitted themselves. Approver IDs are caller-supplied at the
// handler boundary — domain enforces the invariant. 422.
var ErrSelfApproval = errs.New(errs.KindInvalidInput, "permission_request", "approver cannot equal requester (self-approval forbidden)")

// ErrPendingRequestExists is returned by the adapter when the partial
// unique index on (requester_membership_id, permission_constant)
// WHERE state='pending' refuses an INSERT. 409 Conflict.
var ErrPendingRequestExists = errs.New(errs.KindAlreadyExists, "permission_request", "a pending request for this permission already exists")

// ErrNotFound surfaces when GetByID / UpdateByID can't locate the row
// in the caller's tenant scope (RLS-hidden rows are indistinguishable
// from non-existent rows per ADR 0044 enumeration safety).
var ErrNotFound = errs.New(errs.KindNotFound, "permission_request", "permission request not found")

// ErrInvalidPermission surfaces when [New] is supplied a permission
// pointer that's not in the closed-set catalogue. Belt-and-suspenders
// alongside the HTTP-layer validation via [permission.TryFromConstant].
var ErrInvalidPermission = errs.New(errs.KindInvalidInput, "permission_request", "permission is not in the closed-set catalogue")

// ErrInvalidDuration surfaces when durationDays is outside
// [MinDurationDays, MaxDurationDays].
var ErrInvalidDuration = errs.New(errs.KindInvalidInput, "permission_request", "duration_days out of [1, 90]")

// ----- Identifier -----------------------------------------------------------

// ID is the Request primary key (UUIDv7 string form).
type ID string

// IsZero reports whether the ID is unset.
func (i ID) IsZero() bool { return i == "" }

// String returns the underlying UUID string.
func (i ID) String() string { return string(i) }

// ----- State machine -------------------------------------------------------

// State is the workflow state per ADR 0055.
type State string

// State constants. Mirror of the DB CHECK constraint in migration
// 20260523000003 — keep in lockstep.
const (
	StatePending   State = "pending"
	StateApproved  State = "approved"
	StateDenied    State = "denied"
	StateCancelled State = "cancelled"
)

// String renders for log + error messages.
func (s State) String() string { return string(s) }

// IsTerminal reports whether the state admits no further transitions.
// Approved + Denied + Cancelled are all terminal; only Pending mutates.
func (s State) IsTerminal() bool {
	return s == StateApproved || s == StateDenied || s == StateCancelled
}

// ----- Aggregate ------------------------------------------------------------

// Request is the aggregate root. Tenant-scoped via tenantID; RLS
// scopes reads/writes to the bound tenant unless platform-bypass set.
type Request struct {
	id                    ID
	tenantID              tenant.ID
	requesterMembershipID membership.ID
	permission            *permission.Permission
	durationDays          int
	reason                string

	state                State
	approverMembershipID membership.ID
	decidedAt            time.Time
	decisionReason       string
	grantedOverrideID    uuid.UUID
	expiresAt            time.Time

	createdAt time.Time
	updatedAt time.Time

	events []Event
}

// New constructs a brand-new Pending Request. Returns [ErrInvalidRequest]
// (wrapped) on missing identifiers / short reason; [ErrInvalidDuration]
// on out-of-bounds duration; [ErrInvalidPermission] on a catalogue
// miss.
//
// `now` is the wall-clock used for createdAt / updatedAt — caller
// (handler) injects so tests can pin via fake clock.
func New(
	id ID,
	tenantID tenant.ID,
	requesterMembershipID membership.ID,
	perm *permission.Permission,
	durationDays int,
	reason string,
	now time.Time,
) (*Request, error) {
	if id.IsZero() {
		return nil, fmt.Errorf("%w: id required", ErrInvalidRequest)
	}
	if tenantID.IsZero() {
		return nil, fmt.Errorf("%w: tenantID required", ErrInvalidRequest)
	}
	if requesterMembershipID.IsZero() {
		return nil, fmt.Errorf("%w: requesterMembershipID required", ErrInvalidRequest)
	}
	if perm == nil {
		return nil, fmt.Errorf("%w: permission required", ErrInvalidRequest)
	}
	if !permission.IsKnown(perm.Name()) {
		return nil, fmt.Errorf("%w: %q", ErrInvalidPermission, perm.Name())
	}
	if durationDays < MinDurationDays || durationDays > MaxDurationDays {
		return nil, fmt.Errorf("%w: %d not in [%d, %d]",
			ErrInvalidDuration, durationDays, MinDurationDays, MaxDurationDays)
	}
	trimmed := strings.TrimSpace(reason)
	if len(trimmed) < MinReasonLength {
		return nil, fmt.Errorf("%w: reason length %d below floor %d",
			ErrInvalidRequest, len(trimmed), MinReasonLength)
	}
	if len(trimmed) > MaxReasonLength {
		return nil, fmt.Errorf("%w: reason length %d above ceiling %d",
			ErrInvalidRequest, len(trimmed), MaxReasonLength)
	}

	r := &Request{
		id:                    id,
		tenantID:              tenantID,
		requesterMembershipID: requesterMembershipID,
		permission:            perm,
		durationDays:          durationDays,
		reason:                trimmed,
		state:                 StatePending,
		createdAt:             now.UTC(),
		updatedAt:             now.UTC(),
	}
	r.recordEvent(RequestedEvent{
		RequestID:             id,
		TenantID:              tenantID,
		RequesterMembershipID: requesterMembershipID,
		Permission:            perm.Name(),
		DurationDays:          durationDays,
		Reason:                trimmed,
		At:                    now.UTC(),
	})
	return r, nil
}

// ----- Getters -------------------------------------------------------------

// ID returns the Request primary key.
func (r *Request) ID() ID { return r.id }

// TenantID returns the tenant scope.
func (r *Request) TenantID() tenant.ID { return r.tenantID }

// RequesterMembershipID returns the Membership that submitted the request.
func (r *Request) RequesterMembershipID() membership.ID { return r.requesterMembershipID }

// Permission returns the closed-set catalogue permission being requested.
func (r *Request) Permission() *permission.Permission { return r.permission }

// DurationDays returns the JIT window length the requester asked for.
func (r *Request) DurationDays() int { return r.durationDays }

// Reason returns the requester's justification text.
func (r *Request) Reason() string { return r.reason }

// State returns the current workflow state.
func (r *Request) State() State { return r.state }

// IsTerminal reports whether the workflow has reached a terminal state.
func (r *Request) IsTerminal() bool { return r.state.IsTerminal() }

// ApproverMembershipID returns the approver Membership ID. Zero until
// Approve / Deny / Cancel writes a value (Cancel leaves it zero).
func (r *Request) ApproverMembershipID() membership.ID { return r.approverMembershipID }

// DecidedAt returns the wall-clock of the terminal-state transition.
// Zero while Pending.
func (r *Request) DecidedAt() time.Time { return r.decidedAt }

// DecisionReason returns the approver-supplied reason (REQUIRED on
// Deny, optional on Approve, empty on Cancel).
func (r *Request) DecisionReason() string { return r.decisionReason }

// GrantedOverrideID returns the membership_permission_overrides row id
// created on Approve. Zero when not yet Approved. The override's
// expires_at column mirrors [Request.ExpiresAt].
func (r *Request) GrantedOverrideID() uuid.UUID { return r.grantedOverrideID }

// ExpiresAt returns the timestamp the bounded grant expires. Zero
// outside the Approved state.
func (r *Request) ExpiresAt() time.Time { return r.expiresAt }

// CreatedAt returns the immutable creation timestamp.
func (r *Request) CreatedAt() time.Time { return r.createdAt }

// UpdatedAt returns the last-mutation timestamp.
func (r *Request) UpdatedAt() time.Time { return r.updatedAt }

// ----- State transitions ---------------------------------------------------

// Approve transitions Pending → Approved. The application service is
// responsible for creating the membership overlay grant in the SAME
// transaction; `grantedOverrideID` + `expiresAt` are passed in so the
// aggregate records the linkage atomically.
//
// approverID MUST differ from r.requesterMembershipID (no self-
// approval). Empty decisionReason permitted on Approve (audit prefers
// it for context, but unlike Deny it isn't required).
//
// Returns [ErrNotPending] if the request is not in Pending; nil + no-op
// is NOT supported — re-Approve is a programmer error, not idempotent
// (industry: AWS IAM, Microsoft Entra PIM both reject double-approval).
func (r *Request) Approve(
	approverID membership.ID,
	decisionReason string,
	grantedOverrideID uuid.UUID,
	expiresAt time.Time,
	now time.Time,
) error {
	if r.state != StatePending {
		return fmt.Errorf("%w: %s", ErrNotPending, r.state)
	}
	if approverID.IsZero() {
		return fmt.Errorf("%w: approverID required", ErrInvalidRequest)
	}
	if approverID == r.requesterMembershipID {
		return fmt.Errorf("%w: requester %s", ErrSelfApproval, r.requesterMembershipID)
	}
	trimmed := strings.TrimSpace(decisionReason)
	if len(trimmed) > MaxDecisionReasonLength {
		return fmt.Errorf("%w: decision_reason length %d above ceiling %d",
			ErrInvalidRequest, len(trimmed), MaxDecisionReasonLength)
	}
	r.state = StateApproved
	r.approverMembershipID = approverID
	r.decidedAt = now.UTC()
	r.decisionReason = trimmed
	r.grantedOverrideID = grantedOverrideID
	r.expiresAt = expiresAt.UTC()
	r.updatedAt = now.UTC()
	r.recordEvent(ApprovedEvent{
		RequestID:            r.id,
		TenantID:             r.tenantID,
		ApproverMembershipID: approverID,
		ExpiresAt:            expiresAt.UTC(),
		At:                   now.UTC(),
	})
	return nil
}

// Deny transitions Pending → Denied. decisionReason is REQUIRED — a
// denial without context fails audit per DPDP §12 + SOC2 CC4.1.
//
// approverID MUST differ from r.requesterMembershipID (a requester
// "denying" their own request should Cancel instead — different audit
// semantic).
func (r *Request) Deny(
	approverID membership.ID,
	decisionReason string,
	now time.Time,
) error {
	if r.state != StatePending {
		return fmt.Errorf("%w: %s", ErrNotPending, r.state)
	}
	if approverID.IsZero() {
		return fmt.Errorf("%w: approverID required", ErrInvalidRequest)
	}
	if approverID == r.requesterMembershipID {
		return fmt.Errorf("%w: requester %s", ErrSelfApproval, r.requesterMembershipID)
	}
	trimmed := strings.TrimSpace(decisionReason)
	if trimmed == "" {
		return fmt.Errorf("%w: decision_reason required on Deny", ErrInvalidRequest)
	}
	if len(trimmed) > MaxDecisionReasonLength {
		return fmt.Errorf("%w: decision_reason length %d above ceiling %d",
			ErrInvalidRequest, len(trimmed), MaxDecisionReasonLength)
	}
	r.state = StateDenied
	r.approverMembershipID = approverID
	r.decidedAt = now.UTC()
	r.decisionReason = trimmed
	r.updatedAt = now.UTC()
	r.recordEvent(DeniedEvent{
		RequestID:            r.id,
		TenantID:             r.tenantID,
		ApproverMembershipID: approverID,
		Reason:               trimmed,
		At:                   now.UTC(),
	})
	return nil
}

// Cancel transitions Pending → Cancelled. Called by the requester to
// withdraw their own request. Approver fields stay zero (no approver
// was involved). Idempotent re-Cancel is rejected — fail-loud over
// silent no-op so callers know their UI is stale.
func (r *Request) Cancel(now time.Time) error {
	if r.state != StatePending {
		return fmt.Errorf("%w: %s", ErrNotPending, r.state)
	}
	r.state = StateCancelled
	r.decidedAt = now.UTC()
	r.updatedAt = now.UTC()
	r.recordEvent(CancelledEvent{
		RequestID: r.id,
		TenantID:  r.tenantID,
		At:        now.UTC(),
	})
	return nil
}

// ----- Persistence DTO -----------------------------------------------------

// Snapshot is the persistence DTO consumed by [UnmarshalFromDB]. Mirror
// of the database-row shape per identity.permission_requests columns.
type Snapshot struct {
	ID                    ID
	TenantID              tenant.ID
	RequesterMembershipID membership.ID
	Permission            *permission.Permission
	DurationDays          int
	Reason                string
	State                 State
	ApproverMembershipID  membership.ID
	DecidedAt             time.Time
	DecisionReason        string
	GrantedOverrideID     uuid.UUID
	ExpiresAt             time.Time
	CreatedAt             time.Time
	UpdatedAt             time.Time
}

// UnmarshalFromDB rehydrates a Request from persistence. Repository-
// only path; does NOT re-validate (TDL canon — DB-stored data is
// already invariant-checked at write time). Does NOT emit domain events.
func UnmarshalFromDB(s Snapshot) *Request {
	return &Request{
		id:                    s.ID,
		tenantID:              s.TenantID,
		requesterMembershipID: s.RequesterMembershipID,
		permission:            s.Permission,
		durationDays:          s.DurationDays,
		reason:                s.Reason,
		state:                 s.State,
		approverMembershipID:  s.ApproverMembershipID,
		decidedAt:             s.DecidedAt,
		decisionReason:        s.DecisionReason,
		grantedOverrideID:     s.GrantedOverrideID,
		expiresAt:             s.ExpiresAt,
		createdAt:             s.CreatedAt,
		updatedAt:             s.UpdatedAt,
	}
}

// ----- Event handling ------------------------------------------------------

// PullEvents drains recorded domain events. Repository calls this
// after a successful persist, then writes each event into the outbox
// in the same transaction (TDL UpdateFn pattern per ADR 0004 + 0008).
func (r *Request) PullEvents() []Event {
	if len(r.events) == 0 {
		return nil
	}
	out := r.events
	r.events = nil
	return out
}

func (r *Request) recordEvent(e Event) {
	r.events = append(r.events, e)
}
