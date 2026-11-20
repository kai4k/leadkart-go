package command

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/google/uuid"

	"github.com/leadkart/leadkart-go/internal/common/pg"
	"github.com/leadkart/leadkart-go/internal/identity/domain/authority"
	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/identity/domain/permission"
	"github.com/leadkart/leadkart-go/internal/identity/domain/permissionrequest"
	"github.com/leadkart/leadkart-go/internal/identity/domain/person"
	"github.com/leadkart/leadkart-go/internal/identity/domain/role"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)

// Wave 9.1e — Permission-elevation approval workflow command handlers.
// Per ADR 0055. Each handler orchestrates the cross-aggregate flow
// (Request lifecycle + same-tx Membership override grant on Approve) under
// the TDL "handler IS the orchestrator" canon. Approve and Deny decide through
// the authority policy (ADR 0069) under the tenant authority lock.

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

// ErrPermissionRequestRequesterInactive surfaces when an approval finds the
// requester no longer able to receive the grant (membership deactivated, person
// suspended, locked or anonymised since the request was filed). 409; the
// request stays pending and can be denied or cancelled.
var ErrPermissionRequestRequesterInactive = errors.New("permission_request: requester is no longer active")

// ----- RequestPermissionElevation ------------------------------------------

// RequestPermissionElevationCommand carries the validated submission
// input. TenantID + RequesterMembershipID are populated by the HTTP
// layer from the JWT claims — caller never supplies them from the body.
// Per ADR 0062 the TenantID flows explicitly through repo methods.
//
// DurationDays = 0 triggers the [permissionrequest.DefaultDurationDays]
// fallback so the wire DTO can omit the field for the common case.
type RequestPermissionElevationCommand struct {
	TenantID              tenant.ID
	RequesterMembershipID membership.ID
	Permission            *permission.Permission
	DurationDays          int
	Reason                string
	// Actor is the requester's own actor, built once by the port via
	// authn.AuthorityFrom; the zero value denies and an impersonation session
	// cannot submit (ErrImpersonationNotAllowed).
	Actor authority.Actor
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
	requests     permissionrequest.Repository
	memberships  membership.Repository
	now          func() time.Time
	newRequestID func() permissionrequest.ID
}

// NewRequestPermissionElevationHandler wires the handler.
//
// newRequestID is the Request-aggregate-ID factory per the
// `TestArch_HandlersInjectIDFactory` discipline. Production passes
// `func() permissionrequest.ID { return permissionrequest.ID(ids.NewV7().String()) }`;
// tests inject a deterministic counter so the minted ID is pinnable.
func NewRequestPermissionElevationHandler(
	requests permissionrequest.Repository,
	memberships membership.Repository,
	now func() time.Time,
	newRequestID func() permissionrequest.ID,
) RequestPermissionElevationHandler {
	if requests == nil || memberships == nil {
		panic("command: NewRequestPermissionElevationHandler all dependencies required")
	}
	if newRequestID == nil {
		panic("command: NewRequestPermissionElevationHandler newRequestID required")
	}
	if now == nil {
		now = time.Now
	}
	return RequestPermissionElevationHandler{
		requests: requests, memberships: memberships, now: now, newRequestID: newRequestID,
	}
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
	if cmd.TenantID.IsZero() {
		return RequestPermissionElevationResult{},
			errors.New("request_permission_elevation: tenant id required")
	}
	if cmd.RequesterMembershipID.IsZero() {
		return RequestPermissionElevationResult{},
			errors.New("request_permission_elevation: requester membership id required")
	}
	if cmd.Permission == nil {
		return RequestPermissionElevationResult{},
			errors.New("request_permission_elevation: permission required")
	}
	if !cmd.Actor.Valid() {
		return RequestPermissionElevationResult{}, authority.ErrNoAuthority
	}
	if cmd.Actor.IsImpersonation() {
		return RequestPermissionElevationResult{}, authority.ErrImpersonationNotAllowed
	}
	duration := cmd.DurationDays
	if duration == 0 {
		duration = permissionrequest.DefaultDurationDays
	}

	requester, err := h.memberships.GetByID(ctx, cmd.TenantID, cmd.RequesterMembershipID)
	if err != nil {
		if errors.Is(err, membership.ErrNotFound) {
			return RequestPermissionElevationResult{}, ErrUserNotFound
		}
		return RequestPermissionElevationResult{},
			fmt.Errorf("request_permission_elevation: load requester: %w", err)
	}

	now := h.now()
	req, err := permissionrequest.New(
		h.newRequestID(),
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

// ApprovePermissionRequestCommand carries the approval input. The caller is
// the Actor, built once by the port (authn.AuthorityFrom); the zero Actor is
// refused before anything is locked or loaded. There is no "operator" flag:
// the policy's operator rule (exempt only across tenants) decides, so in the
// platform tenant an operator follows rank like anyone else.
//
// TenantID is the caller's tenant scope (injected from JWT context by the
// HTTP layer) and flows explicitly through repository methods per ADR 0062.
type ApprovePermissionRequestCommand struct {
	TenantID       tenant.ID
	RequestID      permissionrequest.ID
	DecisionReason string
	// Actor is built once by the port via authn.AuthorityFrom; the zero value denies.
	Actor authority.Actor
}

// ApprovePermissionRequestHandler runs the approve flow.
//
// Per ADR 0055 + 0069 approval is a multi-step state machine in ONE
// transaction under the tenant authority lock:
//
//  1. Load the STORED request (never trust the body for the permission).
//  2. Load the requester as an authority.Member.
//  3. Decide authority.Actor.MayApprove(requester, stored permission).
//  4. Only then the state checks (pending, requestable, requester active).
//  5. Mutate the request (Pending -> Approved) and confer exactly that
//     permission on the requester for the stored duration, through
//     Membership.ReplacePermissionOverrides (the policy's snapshot is the
//     aggregate's input).
//
// Both writes share the SAME tx so either both succeed or both fail.
type ApprovePermissionRequestHandler struct {
	w             authorityWriter
	requests      permissionrequest.Repository
	newOverrideID func() uuid.UUID
}

// NewApprovePermissionRequestHandler wires the handler.
//
// newOverrideID is the membership_permission_overrides row-ID factory
// per the `TestArch_HandlersInjectIDFactory` discipline. Production
// passes `func() uuid.UUID { return ids.NewV7() }`; tests inject a
// deterministic counter so the minted ID is pinnable.
func NewApprovePermissionRequestHandler(
	uow pg.UnitOfWork,
	lock AuthorityLocker,
	requests permissionrequest.Repository,
	m membership.Repository,
	r role.Repository,
	p person.Repository,
	now func() time.Time,
	newOverrideID func() uuid.UUID,
) ApprovePermissionRequestHandler {
	if requests == nil {
		panic("command: NewApprovePermissionRequestHandler requests repository required")
	}
	if newOverrideID == nil {
		panic("command: NewApprovePermissionRequestHandler newOverrideID required")
	}
	return ApprovePermissionRequestHandler{
		w:        newAuthorityWriter("NewApprovePermissionRequestHandler", uow, lock, m, r, p, now),
		requests: requests, newOverrideID: newOverrideID,
	}
}

// loadForDecision loads the stored request and its requester under the held
// lock. A missing request or requester is ErrPermissionRequestNotFound. The
// request is returned even when the requester's roles do not load consistently
// (the error is then ErrMemberRolesInconsistent), so the caller can decide who
// may be told.
func (w authorityWriter) loadForDecision(ctx context.Context, requests permissionrequest.Repository, tenantID tenant.ID,
	id permissionrequest.ID, now time.Time) (*permissionrequest.Request, *membership.Membership, authority.Member, error) {
	req, err := requests.GetByID(ctx, tenantID, id)
	if err != nil {
		if errors.Is(err, permissionrequest.ErrNotFound) {
			return nil, nil, authority.Member{}, ErrPermissionRequestNotFound
		}
		return nil, nil, authority.Member{}, fmt.Errorf("load request: %w", err)
	}
	m, requester, err := w.facts.memberByID(ctx, tenantID, req.RequesterMembershipID(), now)
	if err != nil {
		if errors.Is(err, membership.ErrNotFound) {
			// The requester membership is gone: collapse to "not found".
			return nil, nil, authority.Member{}, ErrPermissionRequestNotFound
		}
		return req, nil, authority.Member{}, err
	}
	return req, m, requester, nil
}

// eligibleToDecide is the gate in front of every approval and denial: a caller
// who is not the requester and may not even deny the request is told it does
// not exist (404), exactly like the read route, so the response is no oracle
// for the request or for relative rank. The requester themself keeps the
// self-target answer. A requester whose roles do not load (a 409 for those who
// could decide) is a 404 for everyone else.
func eligibleToDecide(actor authority.Actor, tenantID tenant.ID, req *permissionrequest.Request, requester authority.Member, loadErr error) error {
	if req != nil && req.RequesterMembershipID() == actor.MembershipID() {
		return authority.ErrSelfTarget
	}
	if errors.Is(loadErr, ErrMemberRolesInconsistent) {
		if actor.MayDecideAny(tenantID) {
			return loadErr
		}
		return ErrPermissionRequestNotFound
	}
	if loadErr != nil {
		return loadErr
	}
	if actor.MayDeny(requester) != nil {
		return ErrPermissionRequestNotFound
	}
	return nil
}

// Handle approves the request + grants the time-bound permission. Check
// order: existence and tenant (404) -> eligibility to decide at all (404 for a
// caller who may not even deny it) -> authority over this permission (403) ->
// state and requestability (409 / 422) -> requester still active (409). The
// grant ends no later than the approver's own grant when the approver holds the
// permission only through a time-bound override.
func (h ApprovePermissionRequestHandler) Handle(ctx context.Context, cmd ApprovePermissionRequestCommand) error {
	if cmd.TenantID.IsZero() {
		return errors.New("approve_permission_request: tenant id required")
	}
	if cmd.RequestID.IsZero() {
		return errors.New("approve_permission_request: request id required")
	}
	if !cmd.Actor.Valid() {
		return authority.ErrNoAuthority
	}
	if cmd.Actor.IsImpersonation() {
		// About the session, not the request, so it is answered before anything is
		// read and tells a session nothing about which requests exist.
		return authority.ErrImpersonationNotAllowed
	}
	now := h.w.now()
	err := h.w.locked(ctx, cmd.TenantID, cmd.Actor, now, func(ctx context.Context, actor authority.Actor) error {
		req, requesterM, requester, err := h.w.loadForDecision(ctx, h.requests, cmd.TenantID, cmd.RequestID, now)
		if err != nil && req == nil {
			return err
		}
		expiresAt, err := authorizeApproval(actor, cmd.TenantID, req, requesterM, requester, err, now)
		if err != nil {
			return err
		}
		perm := req.Permission()
		// A stored row from before requestability was enforced is decided by the
		// eligibility above and can only be denied or cancelled (422 below).
		if req.State() != permissionrequest.StatePending {
			return ErrPermissionRequestNotPending
		}
		if !permission.IsRequestable(perm.Name()) {
			return fmt.Errorf("%w: %q", permissionrequest.ErrPermissionNotRequestable, perm.Name())
		}
		if !requester.IsActive() {
			return ErrPermissionRequestRequesterInactive
		}
		overrideID := h.newOverrideID()
		if err := h.requests.UpdateByID(ctx, cmd.TenantID, cmd.RequestID, func(loaded *permissionrequest.Request) (bool, error) {
			return true, loaded.Approve(cmd.Actor.MembershipID(), cmd.DecisionReason, overrideID, expiresAt, now)
		}); err != nil {
			return err
		}
		// Written on the row as it is NOW (locked): only the approved name changes,
		// everything else, expired history included, is written back as it was.
		return h.w.facts.memberships.UpdateByID(ctx, cmd.TenantID, req.RequesterMembershipID(), func(m *membership.Membership) (bool, error) {
			granted, revoked := approvedOverrides(m.GrantedPermissions(), m.RevokedPermissions(), perm, expiresAt)
			if err := m.ReplacePermissionOverrides(granted, revoked, now); err != nil {
				return false, err
			}
			return true, h.w.rotateStampIfChanged(ctx, m, "permission_approved", now)
		})
	})
	switch {
	case err == nil:
		return nil
	case errors.Is(err, permissionrequest.ErrNotPending):
		return ErrPermissionRequestNotPending
	case errors.Is(err, permissionrequest.ErrNotFound):
		return ErrPermissionRequestNotFound
	case errors.Is(err, membership.ErrNotFound):
		return ErrPermissionRequestNotFound
	}
	return fmt.Errorf("approve_permission_request: %w", err)
}

// authorizeApproval is the authority phase of an approval, before any state is
// looked at: eligibility to decide at all (404), then authority over this
// permission (403), then the expiry the approval may have (the stored duration,
// capped at the approver's own grant), then the un-revoke rule when the name is
// on the requester's revoked list. It returns the instant the grant ends.
func authorizeApproval(actor authority.Actor, tenantID tenant.ID, req *permissionrequest.Request, requesterM *membership.Membership,
	requester authority.Member, loadErr error, now time.Time) (time.Time, error) {
	if err := eligibleToDecide(actor, tenantID, req, requester, loadErr); err != nil {
		return time.Time{}, err
	}
	perm := req.Permission()
	expiresAt := now.Add(time.Duration(req.DurationDays()) * 24 * time.Hour)
	if !permission.IsRequestable(perm.Name()) {
		return expiresAt, nil // a stored legacy row: eligible to decide, only deny or cancel (422 later)
	}
	if err := actor.MayApprove(requester, perm.Name()); err != nil {
		return time.Time{}, err
	}
	expiresAt = actor.CapApprovalExpiry(requester, perm.Name(), expiresAt)
	// A name on the requester's REVOKED list is a deliberate "this member must
	// not have it": approving it is an un-revoke and needs the authority to
	// change their overrides (Admin tier or above, hold the name under the
	// ceiling rules) on top of the approval rule. Still an authority decision,
	// so it precedes the state checks.
	if slices.Contains(requester.Overrides().Revoked, perm.Name()) {
		if err := mayUnrevoke(actor, requester, requesterM, perm, expiresAt, now); err != nil {
			return time.Time{}, err
		}
	}
	return expiresAt, nil
}

// mayUnrevoke decides the un-revoke an approval of a revoked name amounts to:
// the override state approvedOverrides would write (live entries only, the way
// the policy sees a member), put to [authority.Actor.MayChangeOverrides] exactly
// like an overrides replace.
func mayUnrevoke(actor authority.Actor, requester authority.Member, requesterM *membership.Membership, perm *permission.Permission, expiresAt, now time.Time) error {
	granted, revoked := approvedOverrides(requesterM.GrantedPermissions(), requesterM.RevokedPermissions(), perm, expiresAt)
	after := authority.Overrides{Granted: make([]authority.Override, 0, len(granted)), Revoked: authority.PermissionNames(revoked)}
	for _, g := range granted {
		if g.ExpiresAt.IsZero() || now.Before(g.ExpiresAt) {
			after.Granted = append(after.Granted, authority.Override{Name: g.Permission.Name(), ExpiresAt: g.ExpiresAt})
		}
	}
	return actor.MayChangeOverrides(requester, after)
}

// approvedOverrides is the requester's override state after the approval, as
// the exact input of Membership.ReplacePermissionOverrides, built from the rows
// as they are stored: every grant keeps its row and expiry (expired ones too, so
// someone else's approval does not erase history), the approved permission is
// added (a permanent grant stays permanent, a timed one is extended to the later
// expiry, never shortened), and the permission leaves the revoked list (a grant
// overrides an earlier override-revocation, as it always did).
func approvedOverrides(current []membership.GrantedOverride, currentRevoked []*permission.Permission, perm *permission.Permission, expiresAt time.Time) ([]membership.GrantedOverride, []*permission.Permission) {
	granted := make([]membership.GrantedOverride, 0, len(current)+1)
	have := false
	for _, g := range current {
		if g.Permission != nil && g.Permission.Name() == perm.Name() {
			have = true
			if !g.ExpiresAt.IsZero() && g.ExpiresAt.Before(expiresAt) {
				g.ExpiresAt = expiresAt
			}
		}
		granted = append(granted, g)
	}
	if !have {
		granted = append(granted, membership.GrantedOverride{Permission: perm, ExpiresAt: expiresAt})
	}
	revoked := make([]*permission.Permission, 0, len(currentRevoked))
	for _, r := range currentRevoked {
		if r != nil && r.Name() != perm.Name() {
			revoked = append(revoked, r)
		}
	}
	return granted, revoked
}

// ----- DenyPermissionRequest -----------------------------------------------

// DenyPermissionRequestCommand carries the denial input. DecisionReason
// is REQUIRED on Deny per ADR 0055 (audit canon). The caller is the Actor.
type DenyPermissionRequestCommand struct {
	TenantID       tenant.ID
	RequestID      permissionrequest.ID
	DecisionReason string
	// Actor is built once by the port via authn.AuthorityFrom; the zero value denies.
	Actor authority.Actor
}

// DenyPermissionRequestHandler runs the deny flow.
type DenyPermissionRequestHandler struct {
	w        authorityWriter
	requests permissionrequest.Repository
}

// NewDenyPermissionRequestHandler wires the handler.
func NewDenyPermissionRequestHandler(
	uow pg.UnitOfWork,
	lock AuthorityLocker,
	requests permissionrequest.Repository,
	m membership.Repository,
	r role.Repository,
	p person.Repository,
	now func() time.Time,
) DenyPermissionRequestHandler {
	if requests == nil {
		panic("command: NewDenyPermissionRequestHandler requests repository required")
	}
	return DenyPermissionRequestHandler{
		w:        newAuthorityWriter("NewDenyPermissionRequestHandler", uow, lock, m, r, p, now),
		requests: requests,
	}
}

// Handle rejects the request. Same facts and check order as Approve, decided by
// [authority.Actor.MayDeny] (eligibility without "holds the permission"); a
// caller who may not deny it is told it does not exist.
func (h DenyPermissionRequestHandler) Handle(ctx context.Context, cmd DenyPermissionRequestCommand) error {
	if cmd.TenantID.IsZero() {
		return errors.New("deny_permission_request: tenant id required")
	}
	if cmd.RequestID.IsZero() {
		return errors.New("deny_permission_request: request id required")
	}
	if !cmd.Actor.Valid() {
		return authority.ErrNoAuthority
	}
	if cmd.Actor.IsImpersonation() {
		return authority.ErrImpersonationNotAllowed
	}
	now := h.w.now()
	err := h.w.locked(ctx, cmd.TenantID, cmd.Actor, now, func(ctx context.Context, actor authority.Actor) error {
		req, _, requester, err := h.w.loadForDecision(ctx, h.requests, cmd.TenantID, cmd.RequestID, now)
		if err != nil && req == nil {
			return err
		}
		if err := eligibleToDecide(actor, cmd.TenantID, req, requester, err); err != nil {
			return err
		}
		if req.State() != permissionrequest.StatePending {
			return ErrPermissionRequestNotPending
		}
		return h.requests.UpdateByID(ctx, cmd.TenantID, cmd.RequestID, func(loaded *permissionrequest.Request) (bool, error) {
			return true, loaded.Deny(cmd.Actor.MembershipID(), cmd.DecisionReason, now)
		})
	})
	switch {
	case err == nil:
		return nil
	case errors.Is(err, permissionrequest.ErrNotPending):
		return ErrPermissionRequestNotPending
	case errors.Is(err, permissionrequest.ErrNotFound):
		return ErrPermissionRequestNotFound
	case errors.Is(err, permissionrequest.ErrInvalidRequest):
		return err
	}
	return fmt.Errorf("deny_permission_request: %w", err)
}

// ----- CancelPermissionRequest ---------------------------------------------

// CancelPermissionRequestCommand carries the cancellation input. Only
// the requester themselves can cancel; the handler verifies caller ==
// requester.
//
// TenantID is the caller's tenant scope (injected from JWT context by
// the HTTP layer) — flows explicitly through repository methods per
// ADR 0062 (no ctx-tenancy GUC smuggling).
type CancelPermissionRequestCommand struct {
	TenantID              tenant.ID
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
	if cmd.TenantID.IsZero() {
		return errors.New("cancel_permission_request: tenant id required")
	}
	if cmd.RequestID.IsZero() {
		return errors.New("cancel_permission_request: request id required")
	}
	if cmd.RequesterMembershipID.IsZero() {
		return errors.New("cancel_permission_request: requester membership id required")
	}

	req, err := h.requests.GetByID(ctx, cmd.TenantID, cmd.RequestID)
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
	if err := h.requests.UpdateByID(ctx, cmd.TenantID, cmd.RequestID, func(loaded *permissionrequest.Request) (bool, error) {
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
