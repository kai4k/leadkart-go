package calllog

import (
	"context"

	"github.com/leadkart/leadkart-go/internal/common/errs"
	"github.com/leadkart/leadkart-go/internal/crm/domain/crmlead"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)

// ErrNotFound is returned by Repository.GetByID when no call log exists
// for the supplied identifier.
var ErrNotFound = errs.New(errs.KindNotFound, "call_log", "call log not found")

// Repository persists CallLog aggregates. Append-only — no UpdateByID;
// the Slice-1 surface is intentionally minimal.
//
// Tenant scoping (ADR 0062 — TDL canon): every method that takes an ID
// without an aggregate ALSO takes an EXPLICIT tenantID parameter. The
// adapter binds the GUC from the parameter at tx-begin (NOT from ctx-
// tenancy.WithID — that's a domain value in context, which Khorikov §11
// + Cheney mark as a hidden input).
type Repository interface {
	// Add persists a brand-new CallLog from [New] and drains its
	// events into the crm.outbox in the same transaction. The aggregate
	// already carries its TenantID — no separate param needed.
	Add(ctx context.Context, c *CallLog) error

	// GetByID returns the call log from the supplied tenant or
	// [ErrNotFound].
	GetByID(ctx context.Context, tenantID tenant.ID, id ID) (*CallLog, error)

	// ListByLead returns every call log for a lead under the supplied
	// tenant, newest first. No pagination at slice 1 — high-volume leads
	// can switch to a paged variant in slice 2.
	ListByLead(ctx context.Context, tenantID tenant.ID, leadID crmlead.ID) ([]*CallLog, error)
}
