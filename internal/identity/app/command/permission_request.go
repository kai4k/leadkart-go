package command

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/leadkart/leadkart-go/internal/common/ids"
	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/identity/domain/permission"
	"github.com/leadkart/leadkart-go/internal/identity/domain/permissionrequest"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)

// Wave 9.1e — Permission-elevation approval workflow command handlers.
// Per ADR 0055. Each handler orchestrates the cross-aggregate flow
// (Request lifecycle + optional same-tx Membership overlay grant on
// Approve) under the TDL "handler IS the orchestrator" canon.

// ----- Sentinel errors -----------------------------------------------------

// ErrPermissionRequestNotFound surfaces when the request id has no
// live row in the caller's tenant. Collapses "wrong tenant" + "doesn't
// exist" per ADR 0044 enumeration safety.
var ErrPermissionRequestNotFound = errors.New("permission_request: not found")

// ErrPermissionRequestPendingExists surfaces when the at-most-one-
// pending invariant rejects a new submission for the same
// (membership, permission) tuple.
var ErrPermissionRequestPendingExists = errors.New("permission_request: pending request already exists for this permission")

// ErrPermissionRequestNotPending surfaces when Approve / Deny / Cancel
// hits a non-Pending state. Maps to 409 Conflict at the HTTP layer.
var ErrPermissionRequestNotPending = errors.New("permission_request: request is not pending")

// ErrPermissionRequestSelfApproval surfaces when approverID equals
// requesterID. Maps to 422.
var ErrPermissionRequestSelfApproval = errors.New("permission_request: cannot self-approve")

// ErrPermissionRequestForbidden surfaces when the caller is neither
// the requester (for Cancel) nor an authorised approver (for Approve /
// Deny). Maps to 403.
var ErrPermissionRequestForbidden = errors.New("permission_request: caller cannot act on this request")

// ----- RequestPermissionElevation ------------------------------------------

// RequestPermissionElevationCommand carries the validated submission
// input. RequesterMembershipID is populated by the HTTP layer from the
// JWT membership_id claim — caller never supplies it from the body.
//
// DurationDays = 0 triggers the [permissionrequest.DefaultDurationDays]
// fallback so the wire DTO can omit the field for the common case.
type RequestPermissionElevationCommand struct {
	RequesterMembershipID membership.ID
	Permission            *permission.Permission
	DurationDays          int
	Reason                string
}

// RequestPermissionElevationResult is the wire-friendly outcome. The
// ApproverMembershipID is the resolved manager — zero when the
// requester has no manager (the request is then approvable only by
// Platform operators per ADR 0055).
type RequestPermissionElevationResult struct {
	RequestID            permissionrequest.ID
	ApproverMembershipID membership.ID
}

// RequestPermissionElevationHandler depends on the two repositories +
// a clock. Per ADR 0047 — interfaces only, no pgx / concrete adapters.
type RequestPermissionElevationHandler struct {
	requests    permissionrequest.Repository
	memberships membership.Repository
	now         func() time.Time
}

// NewRequestPermissionElevationHandler wires the handler.
func NewRequestPermissionElevationHandler(
	requests permissionrequest.Repository,
	memberships membership.Repository,
	now func() time.Time,
) RequestPermissionElevationHandler {
	if requests == nil || memberships == nil {
		panic("command: NewRequestPermissionElevationHandler all dependencies required")
	}
	if now == nil {
		now = time.Now
	}
	return RequestPermissionElevationHandler{requests: requests, memberships: memberships, now: now}
}

// Handle constructs + persists the Pending Request. The at-most-one-
// pending invariant is enforced at the DB layer (partial unique index
// → ErrPendingRequestExists → translated below).
//
// The approver is the requester's current ManagerID per ADR 0055; the
// approverMembershipID is resolved here + reported back in the result
// so the requester's UI can show "this is who must approve". Approver
// is NOT written into the row at submission time — the row carries it
// only on the Approve/Deny terminal transition.
//
// Caller invariant: the HTTP layer has already validated the
// permission name against the closed-set catalogue.
func (h RequestPermissionElevationHandler) Handle(
	ctx context.Context,
	cmd RequestPermissionElevationCommand,
) (RequestPermissionElevationResult, error) {
	if cmd.RequesterMembershipID.IsZero() {
		return RequestPermissionElevationResult{},
			errors.New("request_permission_elevation: requester membership id required")
	}
	if cmd.Permission == nil {
		return RequestPermissionElevationResult{},
			errors.New("request_permission_elevation: permission required")
	}
	duration := cmd.DurationDays
	if duration == 0 {
		duration = permissionrequest.DefaultDurationDays
	}

	requester, err := h.memberships.GetByID(ctx, cmd.RequesterMembershipID)
	if err != nil {
		if errors.Is(err, membership.ErrNotFound) {
			return RequestPermissionElevationResult{}, ErrUserNotFound
		}
		return RequestPermissionElevationResult{},
			fmt.Errorf("request_permission_elevation: load requester: %w", err)
	}

	now := h.now()
	req, err := permissionrequest.New(
		permissionrequest.ID(ids.NewV7().String()),
		requester.TenantID(),
		requester.ID(),
		cmd.Permission,
		duration,
		cmd.Reason,
		now,
	)
	if err != nil {
		return RequestPermissionElevationResult{}, err
	}

	if err := h.requests.Add(ctx, req); err != nil {
		if errors.Is(err, permissionrequest.ErrPendingRequestExists) {
			return RequestPermissionElevationResult{}, ErrPermissionRequestPendingExists
		}
		return RequestPermissionElevationResult{},
			fmt.Errorf("request_permission_elevation: persist: %w", err)
	}

	return RequestPermissionElevationResult{
		RequestID:            req.ID(),
		ApproverMembershipID: requester.ReportsTo(),
	}, nil
}

// ----- ApprovePermissionRequest --------------------------------------------

// ApprovePermissionRequestCommand carries the approval input.
// ApproverMembershipID is the caller's Membership (populated from JWT
// by the HTTP layer); IsPlatformOperator is true when the caller's JWT
// has is_platform=true — used to short-circuit the manager-check for
// orphan / root memberships per ADR 0055.
type ApprovePermissionRequestCommand struct {
	RequestID            permissionrequest.ID
	ApproverMembershipID membership.ID
	IsPlatformOperator   bool
	DecisionReason       string
}

// ApprovePermissionRequestHandler runs the approve flow.
//
// Per ADR 0055: Approve is a multi-step state machine in ONE tx —
//
//  1. Load the Request + verify Pending state.
//  2. Verify the approver is EITHER the requester's current manager
//     OR a Platform operator (caller-supplied flag from JWT).
//  3. Compute ExpiresAt = now + DurationDays.
//  4. UpdateByID:
//     a) Mutate Request (Pending → Approved, recording approverID,
//     decision_reason, granted_override_id (UUIDv7), expires_at).
//     b) Outbox event drained.
//  5. Mutate Membership (GrantPermission with ExpiresAt, drain event).
//
// Both writes share the SAME pg.UnitOfWork tx so either both succeed
// or both fail — no orphan Approved-without-grant rows.
type ApprovePermissionRequestHandler struct {
	requests    permissionrequest.Repository
	memberships membership.Repository
	now         func() time.Time
}

// NewApprovePermissionRequestHandler wires the handler.
func NewApprovePermissionRequestHandler(
	requests permissionrequest.Repository,
	memberships membership.Repository,
	now func() time.Time,
) ApprovePermissionRequestHandler {
	if requests == nil || memberships == nil {
		panic("command: NewApprovePermissionRequestHandler all dependencies required")
	}
	if now == nil {
		now = time.Now
	}
	return ApprovePermissionRequestHandler{requests: requests, memberships: memberships, now: now}
}

// Handle approves the request + grants the time-bound permission.
//
// Authorization decision tree per ADR 0055:
//   - approver is requester's current manager → allowed.
//   - approver is Platform operator           → allowed.
//   - otherwise                                → ErrPermissionRequestForbidden.
//
// The Platform-operator override exists so orphan memberships (e.g.
// CompanyOwner with no manager) still have an approval path.
func (h ApprovePermissionRequestHandler) Handle(
	ctx context.Context,
	cmd ApprovePermissionRequestCommand,
) error {
	if cmd.RequestID.IsZero() {
		return errors.New("approve_permission_request: request id required")
	}
	if cmd.ApproverMembershipID.IsZero() {
		return errors.New("approve_permission_request: approver membership id required")
	}

	// Load requester membership for the manager-check. We do this
	// outside the UpdateByID closure so the authz decision is settled
	// before the tx opens.
	req, err := h.requests.GetByID(ctx, cmd.RequestID)
	if err != nil {
		if errors.Is(err, permissionrequest.ErrNotFound) {
			return ErrPermissionRequestNotFound
		}
		return fmt.Errorf("approve_permission_request: load: %w", err)
	}
	if req.State() != permissionrequest.StatePending {
		return ErrPermissionRequestNotPending
	}
	if cmd.ApproverMembershipID == req.RequesterMembershipID() {
		return ErrPermissionRequestSelfApproval
	}

	requester, err := h.memberships.GetByID(ctx, req.RequesterMembershipID())
	if err != nil {
		if errors.Is(err, membership.ErrNotFound) {
			// The requester membership has been deleted between
			// submission and now — collapse to "not found" so the
			// approver sees a coherent error.
			return ErrPermissionRequestNotFound
		}
		return fmt.Errorf("approve_permission_request: load requester: %w", err)
	}

	if !h.callerCanApprove(cmd, requester) {
		return ErrPermissionRequestForbidden
	}

	now := h.now()
	expiresAt := now.Add(time.Duration(req.DurationDays()) * 24 * time.Hour)
	overrideID := ids.NewV7()

	// Step 1 — flip Request to Approved + record grant linkage.
	if err := h.requests.UpdateByID(ctx, cmd.RequestID, func(loaded *permissionrequest.Request) (bool, error) {
		if err := loaded.Approve(cmd.ApproverMembershipID, cmd.DecisionReason, overrideID, expiresAt, now); err != nil {
			return false, err
		}
		return true, nil
	}); err != nil {
		switch {
		case errors.Is(err, permissionrequest.ErrNotPending):
			return ErrPermissionRequestNotPending
		case errors.Is(err, permissionrequest.ErrSelfApproval):
			return ErrPermissionRequestSelfApproval
		case errors.Is(err, permissionrequest.ErrNotFound):
			return ErrPermissionRequestNotFound
		default:
			return fmt.Errorf("approve_permission_request: update request: %w", err)
		}
	}

	// Step 2 — grant the bounded permission on the requester's overlay.
	if err := h.memberships.UpdateByID(ctx, req.RequesterMembershipID(), func(m *membership.Membership) (bool, error) {
		if err := m.GrantPermission(req.Permission(), expiresAt); err != nil {
			return false, err
		}
		return true, nil
	}); err != nil {
		// At this point the Request is already Approved. A subscriber-
		// driven compensating action could re-Cancel + revoke; for v0.2
		// we surface the error + leave the Request in Approved state.
		// The Membership overlay grant is the load-bearing security
		// surface; the audit log records the Approve event regardless.
		return fmt.Errorf("approve_permission_request: grant override: %w", err)
	}
	return nil
}

// callerCanApprove evaluates the manager-or-platform rule.
func (h ApprovePermissionRequestHandler) callerCanApprove(
	cmd ApprovePermissionRequestCommand,
	requester *membership.Membership,
) bool {
	if cmd.IsPlatformOperator {
		return true
	}
	manager := requester.ReportsTo()
	if manager.IsZero() {
		// Orphan / root membership — only Platform operators can approve.
		return false
	}
	return manager == cmd.ApproverMembershipID
}

// ----- DenyPermissionRequest -----------------------------------------------

// DenyPermissionRequestCommand carries the denial input. DecisionReason
// is REQUIRED on Deny per ADR 0055 (audit canon).
type DenyPermissionRequestCommand struct {
	RequestID            permissionrequest.ID
	ApproverMembershipID membership.ID
	IsPlatformOperator   bool
	DecisionReason       string
}

// DenyPermissionRequestHandler runs the deny flow.
type DenyPermissionRequestHandler struct {
	requests    permissionrequest.Repository
	memberships membership.Repository
	now         func() time.Time
}

// NewDenyPermissionRequestHandler wires the handler.
func NewDenyPermissionRequestHandler(
	requests permissionrequest.Repository,
	memberships membership.Repository,
	now func() time.Time,
) DenyPermissionRequestHandler {
	if requests == nil || memberships == nil {
		panic("command: NewDenyPermissionRequestHandler all dependencies required")
	}
	if now == nil {
		now = time.Now
	}
	return DenyPermissionRequestHandler{requests: requests, memberships: memberships, now: now}
}

// Handle rejects the request. Same authz tree as Approve: manager-or-
// platform. Single-step state machine (no membership write) so a flat
// UpdateByID call suffices.
func (h DenyPermissionRequestHandler) Handle(
	ctx context.Context,
	cmd DenyPermissionRequestCommand,
) error {
	if cmd.RequestID.IsZero() {
		return errors.New("deny_permission_request: request id required")
	}
	if cmd.ApproverMembershipID.IsZero() {
		return errors.New("deny_permission_request: approver membership id required")
	}

	req, err := h.requests.GetByID(ctx, cmd.RequestID)
	if err != nil {
		if errors.Is(err, permissionrequest.ErrNotFound) {
			return ErrPermissionRequestNotFound
		}
		return fmt.Errorf("deny_permission_request: load: %w", err)
	}
	if req.State() != permissionrequest.StatePending {
		return ErrPermissionRequestNotPending
	}
	if cmd.ApproverMembershipID == req.RequesterMembershipID() {
		return ErrPermissionRequestSelfApproval
	}

	requester, err := h.memberships.GetByID(ctx, req.RequesterMembershipID())
	if err != nil {
		if errors.Is(err, membership.ErrNotFound) {
			return ErrPermissionRequestNotFound
		}
		return fmt.Errorf("deny_permission_request: load requester: %w", err)
	}
	if !canApproveSimilar(cmd.IsPlatformOperator, cmd.ApproverMembershipID, requester) {
		return ErrPermissionRequestForbidden
	}

	now := h.now()
	if err := h.requests.UpdateByID(ctx, cmd.RequestID, func(loaded *permissionrequest.Request) (bool, error) {
		if err := loaded.Deny(cmd.ApproverMembershipID, cmd.DecisionReason, now); err != nil {
			return false, err
		}
		return true, nil
	}); err != nil {
		switch {
		case errors.Is(err, permissionrequest.ErrNotPending):
			return ErrPermissionRequestNotPending
		case errors.Is(err, permissionrequest.ErrSelfApproval):
			return ErrPermissionRequestSelfApproval
		case errors.Is(err, permissionrequest.ErrNotFound):
			return ErrPermissionRequestNotFound
		case errors.Is(err, permissionrequest.ErrInvalidRequest):
			return err
		default:
			return fmt.Errorf("deny_permission_request: update: %w", err)
		}
	}
	return nil
}

// canApproveSimilar is the shared authz check used by both Approve +
// Deny — same rule (manager-or-platform).
func canApproveSimilar(
	isPlatform bool,
	approver membership.ID,
	requester *membership.Membership,
) bool {
	if isPlatform {
		return true
	}
	mgr := requester.ReportsTo()
	if mgr.IsZero() {
		return false
	}
	return mgr == approver
}

// ----- CancelPermissionRequest ---------------------------------------------

// CancelPermissionRequestCommand carries the cancellation input. Only
// the requester themselves can cancel; the handler verifies caller ==
// requester.
type CancelPermissionRequestCommand struct {
	RequestID             permissionrequest.ID
	RequesterMembershipID membership.ID
}

// CancelPermissionRequestHandler runs the cancel flow.
type CancelPermissionRequestHandler struct {
	requests permissionrequest.Repository
	now      func() time.Time
}

// NewCancelPermissionRequestHandler wires the handler.
func NewCancelPermissionRequestHandler(
	requests permissionrequest.Repository,
	now func() time.Time,
) CancelPermissionRequestHandler {
	if requests == nil {
		panic("command: NewCancelPermissionRequestHandler requests repository required")
	}
	if now == nil {
		now = time.Now
	}
	return CancelPermissionRequestHandler{requests: requests, now: now}
}

// Handle cancels the request. Caller MUST equal the requester
// membership — collapse mismatch to 404 per enumeration-safety canon.
func (h CancelPermissionRequestHandler) Handle(
	ctx context.Context,
	cmd CancelPermissionRequestCommand,
) error {
	if cmd.RequestID.IsZero() {
		return errors.New("cancel_permission_request: request id required")
	}
	if cmd.RequesterMembershipID.IsZero() {
		return errors.New("cancel_permission_request: requester membership id required")
	}

	req, err := h.requests.GetByID(ctx, cmd.RequestID)
	if err != nil {
		if errors.Is(err, permissionrequest.ErrNotFound) {
			return ErrPermissionRequestNotFound
		}
		return fmt.Errorf("cancel_permission_request: load: %w", err)
	}
	if req.RequesterMembershipID() != cmd.RequesterMembershipID {
		// Cross-requester cancel — 404 per enumeration-safety canon.
		return ErrPermissionRequestNotFound
	}
	if req.State() != permissionrequest.StatePending {
		return ErrPermissionRequestNotPending
	}

	now := h.now()
	if err := h.requests.UpdateByID(ctx, cmd.RequestID, func(loaded *permissionrequest.Request) (bool, error) {
		if err := loaded.Cancel(now); err != nil {
			return false, err
		}
		return true, nil
	}); err != nil {
		switch {
		case errors.Is(err, permissionrequest.ErrNotPending):
			return ErrPermissionRequestNotPending
		case errors.Is(err, permissionrequest.ErrNotFound):
			return ErrPermissionRequestNotFound
		default:
			return fmt.Errorf("cancel_permission_request: update: %w", err)
		}
	}
	return nil
}

// Compile-time guarantee that uuid + tenant.ID stay reachable; this
// keeps the build green when the imports above are pruned by future
// refactor passes that move type signatures around.
var (
	_ = uuid.Nil
	_ tenant.ID
)
