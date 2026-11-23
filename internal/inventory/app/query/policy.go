package query

import (
	"context"
	"errors"
	"fmt"

	"github.com/leadkart/leadkart-go/internal/common/commercepolicy"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)

// PolicyReader reads Inventory' replica of the company commerce policy (ADR 0070).
// Defined here, with its consumer; the pg adapter implements it.
//
// It reads THROUGH the transaction already open on ctx (pg.TxFromContext), so a
// business handler holding a row lock reads the rule without a second pooled
// connection (review finding C1). With no transaction on ctx it opens its own
// tenant-scoped one. A tenant with no stored policy reads
// commercepolicy.Initial().
type PolicyReader interface {
	ReadPolicy(ctx context.Context, tenantID tenant.ID) (commercepolicy.Versioned, error)
}

// GetPolicyQuery selects the tenant's policy as Inventory last learned it.
type GetPolicyQuery struct {
	TenantID tenant.ID
}

// GetPolicyHandler reads the replica. Later rounds read the expiry cut-off and the GST list from it.
type GetPolicyHandler struct {
	policy PolicyReader
}

// NewGetPolicyHandler wires the handler.
func NewGetPolicyHandler(policy PolicyReader) GetPolicyHandler {
	if policy == nil {
		panic("query: NewGetPolicyHandler policy required")
	}
	return GetPolicyHandler{policy: policy}
}

// Handle returns the policy and its version (0 for the defaults).
func (h GetPolicyHandler) Handle(ctx context.Context, q GetPolicyQuery) (commercepolicy.Versioned, error) {
	if q.TenantID == "" {
		return commercepolicy.Versioned{}, errors.New("get policy: tenant id required")
	}
	v, err := h.policy.ReadPolicy(ctx, q.TenantID)
	if err != nil {
		return commercepolicy.Versioned{}, fmt.Errorf("get policy: %w", err)
	}
	return v, nil
}
