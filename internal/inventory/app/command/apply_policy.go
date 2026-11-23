package command

import (
	"context"
	"errors"
	"fmt"

	"github.com/leadkart/leadkart-go/internal/common/commercepolicy"
	"github.com/leadkart/leadkart-go/internal/common/pg"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)

// PolicyReplicaStore is the write side of Inventory' replica of the company
// commerce policy (ADR 0070). Defined here, with its consumer; the pg adapter
// implements it.
type PolicyReplicaStore interface {
	// StoreIfNewer stores the policy only when its version is newer than the
	// stored one (or nothing is stored) and reports whether it did. A duplicate
	// or reordered event stores nothing: that is not an error.
	StoreIfNewer(ctx context.Context, tenantID tenant.ID, policy commercepolicy.Versioned) (bool, error)
}

// ApplyPolicyCommand carries one companysettings.policy_changed.v1 event.
type ApplyPolicyCommand struct {
	TenantID tenant.ID
	Version  int64
	Policy   commercepolicy.Policy
}

// ApplyPolicyResult says whether the event changed the replica.
type ApplyPolicyResult struct {
	Stored bool
}

// ApplyPolicyHandler updates the replica from the event. Idempotent by the
// version guard: the same version again, or an older one, stores nothing.
type ApplyPolicyHandler struct {
	uow   pg.TenantUnitOfWork
	store PolicyReplicaStore
}

// NewApplyPolicyHandler wires the handler.
func NewApplyPolicyHandler(uow pg.TenantUnitOfWork, store PolicyReplicaStore) ApplyPolicyHandler {
	if uow == nil || store == nil {
		panic("command: NewApplyPolicyHandler uow and store required")
	}
	return ApplyPolicyHandler{uow: uow, store: store}
}

// Handle validates the policy (a malformed event is an error, so it is retried
// and then dead-lettered rather than stored) and stores it when newer.
func (h ApplyPolicyHandler) Handle(ctx context.Context, cmd ApplyPolicyCommand) (ApplyPolicyResult, error) {
	if cmd.TenantID == "" {
		return ApplyPolicyResult{}, errors.New("apply policy: tenant id required")
	}
	if cmd.Version < 1 {
		return ApplyPolicyResult{}, fmt.Errorf("apply policy: version %d is not a stored version", cmd.Version)
	}
	if errs := commercepolicy.Validate(cmd.Policy); len(errs) > 0 {
		return ApplyPolicyResult{}, fmt.Errorf("apply policy: invalid policy: %w", errs.Err())
	}
	var out ApplyPolicyResult
	err := h.uow.WithinTenantTx(ctx, cmd.TenantID.String(), func(ctx context.Context) error {
		stored, err := h.store.StoreIfNewer(ctx, cmd.TenantID, commercepolicy.Versioned{Version: cmd.Version, Policy: cmd.Policy})
		out.Stored = stored
		return err
	})
	if err != nil {
		return ApplyPolicyResult{}, fmt.Errorf("apply policy: %w", err)
	}
	return out, nil
}
