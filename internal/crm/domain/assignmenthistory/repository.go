package assignmenthistory

import (
	"context"

	"github.com/leadkart/leadkart-go/internal/common/errs"
	"github.com/leadkart/leadkart-go/internal/crm/domain/crmlead"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)

// ErrNotFound is returned by Repository.GetByID when no entry exists
// for the supplied identifier.
var ErrNotFound = errs.New(errs.KindNotFound, "assignment_history", "assignment history entry not found")

// Repository persists Entry rows. Append-only — no UpdateByID.
//
// Tenant scoping (ADR 0062 — TDL canon): every method that takes an ID
// without an aggregate ALSO takes an EXPLICIT tenantID parameter. The
// adapter binds the GUC from the parameter at tx-begin (NOT from ctx-
// tenancy.WithID — that's a domain value in context, which Khorikov §11
// + Cheney mark as a hidden input).
type Repository interface {
	// Add persists a brand-new Entry. Slice 1 emits no integration
	// event from this aggregate; the parent CrmLead's [crmlead.Assign]
	// path emits the wire-side `crm.lead_assigned.v1` event. The
	// aggregate already carries its TenantID — no separate param needed.
	Add(ctx context.Context, e *Entry) error

	// GetByID returns the entry from the supplied tenant or [ErrNotFound].
	GetByID(ctx context.Context, tenantID tenant.ID, id ID) (*Entry, error)

	// ListByLead returns every assignment-history entry for a lead under
	// the supplied tenant, newest first. No pagination at slice 1.
	ListByLead(ctx context.Context, tenantID tenant.ID, leadID crmlead.ID) ([]*Entry, error)
}
