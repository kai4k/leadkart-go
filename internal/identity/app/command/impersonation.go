package command

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
	"github.com/leadkart/leadkart-go/internal/platform/impersonation"
)

// ----- CreateImpersonationSession -------------------------------------------

// CreateImpersonationSessionCommand opens a new operator session
// targeting a specific tenant. Reason MUST be ≥ 10 chars per the
// session VO + DPDP §12 / SOC2 CC4.1 audit requirement.
//
// OperatorID arrives from the verified JWT Subject; never from body.
type CreateImpersonationSessionCommand struct {
	OperatorID     string
	TargetTenantID tenant.ID
	Reason         string
	Duration       time.Duration // 0 = default 30min; capped at 4h
}

// CreateImpersonationSessionResult is the wire-friendly outcome.
type CreateImpersonationSessionResult struct {
	SessionID    string
	ExpiresAtUTC time.Time
}

// ErrImpersonationInvalid surfaces from session-creation rejection
// (missing inputs, reason too short, duration too long).
var ErrImpersonationInvalid = errors.New("impersonation: invalid input")

// CreateImpersonationSessionHandler runs the create flow.
type CreateImpersonationSessionHandler struct {
	store impersonation.Store
	now   func() time.Time
}

// NewCreateImpersonationSessionHandler wires the handler.
func NewCreateImpersonationSessionHandler(store impersonation.Store, now func() time.Time) CreateImpersonationSessionHandler {
	if store == nil {
		panic("command: NewCreateImpersonationSessionHandler store required")
	}
	if now == nil {
		now = time.Now
	}
	return CreateImpersonationSessionHandler{store: store, now: now}
}

// Handle constructs + persists the session.
func (h CreateImpersonationSessionHandler) Handle(ctx context.Context, cmd CreateImpersonationSessionCommand) (CreateImpersonationSessionResult, error) {
	if cmd.OperatorID == "" {
		return CreateImpersonationSessionResult{}, errors.New("create_impersonation_session: operator id required")
	}
	if cmd.TargetTenantID.IsZero() {
		return CreateImpersonationSessionResult{}, errors.New("create_impersonation_session: target tenant id required")
	}
	sess, err := impersonation.NewSession(cmd.OperatorID, cmd.TargetTenantID.String(), cmd.Reason, cmd.Duration, h.now())
	if err != nil {
		return CreateImpersonationSessionResult{}, fmt.Errorf("%w: %v", ErrImpersonationInvalid, err)
	}
	if err := h.store.Put(ctx, sess); err != nil {
		return CreateImpersonationSessionResult{}, fmt.Errorf("create_impersonation_session: persist: %w", err)
	}
	return CreateImpersonationSessionResult{
		SessionID:    sess.ID(),
		ExpiresAtUTC: sess.ExpiresAt(),
	}, nil
}

// ----- EndImpersonationSession ---------------------------------------------

// EndImpersonationSessionCommand revokes the operator's session.
// Idempotent — already-deleted sessions return nil.
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
