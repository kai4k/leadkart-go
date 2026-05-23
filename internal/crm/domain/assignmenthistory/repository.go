package assignmenthistory

import (
	"context"

	"github.com/leadkart/leadkart-go/internal/common/errs"
	"github.com/leadkart/leadkart-go/internal/crm/domain/crmlead"
)

// ErrNotFound is returned by Repository.GetByID when no entry exists
// for the supplied identifier.
var ErrNotFound = errs.New(errs.KindNotFound, "assignment_history", "assignment history entry not found")

// Repository persists Entry rows. Append-only — no UpdateByID.
//
// Tenant-scoped per ADR 0006. The adapter binds tenant context on the
// connection before queries touch the table.
type Repository interface {
	// Add persists a brand-new Entry. Slice 1 emits no integration
	// event from this aggregate; the parent CrmLead's [crmlead.Assign]
	// path emits the wire-side `crm.lead-assigned.v1` event.
	Add(ctx context.Context, e *Entry) error

	// GetByID returns the entry or [ErrNotFound].
	GetByID(ctx context.Context, id ID) (*Entry, error)

	// ListByLead returns every assignment-history entry for a lead,
	// newest first. No pagination at slice 1.
	ListByLead(ctx context.Context, leadID crmlead.ID) ([]*Entry, error)
}
