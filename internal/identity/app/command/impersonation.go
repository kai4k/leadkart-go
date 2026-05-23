package command

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/leadkart/leadkart-go/internal/identity/app/jwt"
	"github.com/leadkart/leadkart-go/internal/identity/domain/permission"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
	"github.com/leadkart/leadkart-go/internal/identity/domain/impersonation"
)

// ----- CreateImpersonationSession -------------------------------------------

// CreateImpersonationSessionCommand opens a new operator session
// targeting a specific tenant. Reason MUST be ≥ 10 chars per the
// session VO + DPDP §12 / SOC2 CC4.1 audit requirement.
//
// OperatorID arrives from the verified JWT Subject; never from body.
// OperatorSecurityStamp arrives from the verified JWT security_stamp
// claim — the scoped impersonation token reuses it so the operator
// rotating their stamp (password change, etc.) kills the impersonation
// session naturally (existing RequireFreshStamp middleware enforces).
type CreateImpersonationSessionCommand struct {
	OperatorID            string
	OperatorSecurityStamp string
	TargetTenantID        tenant.ID
	Reason                string
	Duration              time.Duration // 0 = default 30min; capped at 4h
}

// CreateImpersonationSessionResult is the wire-friendly outcome.
//
// Per ADR 0045 — the scoped access token is bound to the session
// lifetime (no separate refresh token for impersonation; AWS STS
// AssumeRole canon — re-open the session if you need longer than 4h).
// Operator's NORMAL refresh-token family is unaffected; they continue
// using it for their platform-tier requests outside the session.
type CreateImpersonationSessionResult struct {
	SessionID               string
	ExpiresAtUTC            time.Time
	AccessToken             string    // NEW (Wave 4) — scoped JWT
	AccessTokenExpiresAtUTC time.Time // NEW (Wave 4) — same as session expiry
}

// ErrImpersonationInvalid surfaces from session-creation rejection
// (missing inputs, reason too short, duration too long).
var ErrImpersonationInvalid = errors.New("impersonation: invalid input")

// ErrImpersonationTargetMissing surfaces when the target tenant
// can't be resolved (slug fetch fails). Distinct from
// ErrImpersonationInvalid so the HTTP layer can return 404 vs 422.
var ErrImpersonationTargetMissing = errors.New("impersonation: target tenant not found")

// CreateImpersonationSessionHandler runs the create flow.
//
// Dependencies per ADR 0045:
//   - store: session record persistence (Redis-backed in prod)
//   - tenants: resolve target tenant for slug-anchored authz checks
//   - issuer: mint the scoped JWT pair
//   - now: clock injection for tests
type CreateImpersonationSessionHandler struct {
	store   impersonation.Store
	tenants tenant.Repository
	issuer  *jwt.Issuer
	now     func() time.Time
}

// NewCreateImpersonationSessionHandler wires the handler.
func NewCreateImpersonationSessionHandler(store impersonation.Store, tenants tenant.Repository, issuer *jwt.Issuer, now func() time.Time) CreateImpersonationSessionHandler {
	if store == nil {
		panic("command: NewCreateImpersonationSessionHandler store required")
	}
	if tenants == nil {
		panic("command: NewCreateImpersonationSessionHandler tenants required")
	}
	if issuer == nil {
		panic("command: NewCreateImpersonationSessionHandler issuer required")
	}
	if now == nil {
		now = time.Now
	}
	return CreateImpersonationSessionHandler{store: store, tenants: tenants, issuer: issuer, now: now}
}

// Handle constructs + persists the session AND mints the scoped JWT.
//
// Steps per ADR 0045 §"Token lifecycle":
//
//  1. Validate inputs (operator ID + target tenant ID present;
//     reason length + duration cap enforced by impersonation.NewSession).
//  2. Persist session record (existing Redis put).
//  3. Resolve target tenant for slug — needed so the scoped JWT
//     carries the correct tenant_slug for slug-anchored authz checks.
//  4. Derive synthetic membership_id from session_id (deterministic;
//     handlers tolerate the missing-membership lookup gracefully).
//  5. Mint scoped JWT via Issuer with:
//     - audience = ImpersonationAudienceClaim
//     - TTL = session lifetime
//     - is_platform = false, is_super_user = false (downgraded)
//     - permissions = [Meta.TenantAdmin] (target-tenant admin authority)
//     - security_stamp = operator's stamp (rotation kills session)
//     - act = {sub=operator, session_id, reason}
func (h CreateImpersonationSessionHandler) Handle(ctx context.Context, cmd CreateImpersonationSessionCommand) (CreateImpersonationSessionResult, error) {
	if cmd.OperatorID == "" {
		return CreateImpersonationSessionResult{}, errors.New("create_impersonation_session: operator id required")
	}
	if cmd.OperatorSecurityStamp == "" {
		return CreateImpersonationSessionResult{}, errors.New("create_impersonation_session: operator security_stamp required")
	}
	if cmd.TargetTenantID.IsZero() {
		return CreateImpersonationSessionResult{}, errors.New("create_impersonation_session: target tenant id required")
	}

	// Resolve target tenant first — if it doesn't exist, fail before
	// creating the session record.
	target, err := h.tenants.GetByID(ctx, cmd.TargetTenantID)
	if err != nil {
		if errors.Is(err, tenant.ErrNotFound) {
			return CreateImpersonationSessionResult{}, fmt.Errorf("%w: %s", ErrImpersonationTargetMissing, cmd.TargetTenantID)
		}
		return CreateImpersonationSessionResult{}, fmt.Errorf("create_impersonation_session: resolve target: %w", err)
	}

	now := h.now()
	sess, err := impersonation.NewSession(cmd.OperatorID, cmd.TargetTenantID.String(), cmd.Reason, cmd.Duration, now)
	if err != nil {
		return CreateImpersonationSessionResult{}, fmt.Errorf("%w: %w", ErrImpersonationInvalid, err)
	}
	if err := h.store.Put(ctx, sess); err != nil {
		return CreateImpersonationSessionResult{}, fmt.Errorf("create_impersonation_session: persist: %w", err)
	}

	// Synthetic membership_id — deterministic from session_id. The
	// handlers that look up Membership rows by this ID will get
	// ErrNotFound (synthetic, not in DB); they fall back to JWT
	// claims for non-membership-bound operations per ADR 0045.
	syntheticMembershipID := syntheticMembershipFromSession(sess.ID())

	// Token TTL = session lifetime (AWS STS AssumeRole pattern).
	tokenTTL := sess.ExpiresAt().Sub(now)

	accessToken, err := h.issuer.Issue(jwt.IssueArgs{
		PersonID:      cmd.OperatorID, // operator's identity (subject)
		TenantID:      target.ID().String(),
		TenantSlug:    target.Slug().String(),
		MembershipID:  syntheticMembershipID,
		SecurityStamp: cmd.OperatorSecurityStamp, // operator's stamp — rotation kills session
		IsPlatform:    false,                     // DOWNGRADED — no platform bypass during session
		IsSuperUser:   false,                     // DOWNGRADED
		// Per ADR 0045: operator gets the tenant-admin meta permission.
		// This is the same permission CompanyOwner has by default; lets
		// operator do anything a normal tenant admin could do, scoped
		// to the target tenant via standard RLS plumbing.
		Permissions: []string{permission.IdentityPermissions.Meta.TenantAdmin},
		Audience:    jwt.ImpersonationAudienceClaim,
		TTL:         tokenTTL,
		Act: &jwt.ActClaim{
			Sub:       cmd.OperatorID,
			SessionID: sess.ID(),
			Reason:    cmd.Reason,
		},
	})
	if err != nil {
		// Session record already persisted; the impersonation flow is
		// idempotent on retry (same session_id → ON CONFLICT no-op).
		// Surface the issuer error without rolling back — operator
		// can re-attempt; the session record is informational.
		return CreateImpersonationSessionResult{}, fmt.Errorf("create_impersonation_session: mint token: %w", err)
	}

	return CreateImpersonationSessionResult{
		SessionID:               sess.ID(),
		ExpiresAtUTC:            sess.ExpiresAt(),
		AccessToken:             accessToken,
		AccessTokenExpiresAtUTC: sess.ExpiresAt(),
	}, nil
}

// syntheticMembershipFromSession derives a deterministic 36-char UUID-
// shaped identifier from the session ID. The result is NOT a real
// tenant_memberships row — it's a placeholder claim value so the JWT
// shape stays uniform with regular tenant-admin tokens.
//
// Handlers that look up Membership by this ID get ErrNotFound; they
// MUST fall back to JWT claims for non-membership-bound operations.
// The deterministic shape lets audit traces correlate "every action
// from this session has the same synthetic membership_id".
//
// Format: SHA-256 truncated + UUID-formatted. Not a real UUIDv7
// (since we want determinism, not time-ordering).
func syntheticMembershipFromSession(sessionID string) string {
	h := sha256.Sum256([]byte("leadkart-impersonation-membership-v1:" + sessionID))
	// Format as a v4-shaped UUID string; bits 6-7 of byte 8 set per RFC 4122.
	hex := hex.EncodeToString(h[:16])
	return hex[0:8] + "-" + hex[8:12] + "-4" + hex[13:16] + "-8" + hex[17:20] + "-" + hex[20:32]
}

// ----- EndImpersonationSession ---------------------------------------------

// EndImpersonationSessionCommand revokes the operator's session.
// Idempotent — already-deleted sessions return nil.
//
// Per ADR 0045: the scoped access token continues to work until its
// natural exp (capped at session.ExpiresAt). Deleting the session
// record prevents NEW operations against this session ID + makes the
// session unreachable from the operator's frontend "active sessions"
// list. The token itself isn't on a denylist (would require redis
// blacklist infra); its exp is short enough that a stolen token is
// usable for ≤ 4h before natural expiry.
//
// If immediate token-level revocation becomes a requirement (Phase 6+
// hardening), the path is to track session_id in a redis denylist
// checked by the verifier middleware.
type EndImpersonationSessionCommand struct {
	OperatorID string
	SessionID  string
}

// EndImpersonationSessionHandler runs the revoke flow.
type EndImpersonationSessionHandler struct {
	store impersonation.Store
}

// NewEndImpersonationSessionHandler wires the handler.
func NewEndImpersonationSessionHandler(store impersonation.Store) EndImpersonationSessionHandler {
	if store == nil {
		panic("command: NewEndImpersonationSessionHandler store required")
	}
	return EndImpersonationSessionHandler{store: store}
}

// Handle deletes the session if (a) it exists AND (b) it belongs to
// the caller. Cross-operator deletion is a 404 (not 403) per
// security.md enumeration-safety.
func (h EndImpersonationSessionHandler) Handle(ctx context.Context, cmd EndImpersonationSessionCommand) error {
	if cmd.OperatorID == "" {
		return errors.New("end_impersonation_session: operator id required")
	}
	if cmd.SessionID == "" {
		return errors.New("end_impersonation_session: session id required")
	}
	sess, err := h.store.Get(ctx, cmd.SessionID)
	if errors.Is(err, impersonation.ErrSessionNotFound) {
		return nil // idempotent
	}
	if err != nil {
		return fmt.Errorf("end_impersonation_session: lookup: %w", err)
	}
	if sess.OperatorID() != cmd.OperatorID {
		return nil // collapse cross-operator delete into idempotent no-op
	}
	if err := h.store.Delete(ctx, cmd.SessionID); err != nil {
		return fmt.Errorf("end_impersonation_session: delete: %w", err)
	}
	return nil
}
