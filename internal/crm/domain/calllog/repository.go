package calllog

import (
	"context"

	"github.com/leadkart/leadkart-go/internal/common/errs"
	"github.com/leadkart/leadkart-go/internal/crm/domain/crmlead"
)

// ErrNotFound is returned by Repository.GetByID when no call log exists
// for the supplied identifier.
var ErrNotFound = errs.New(errs.KindNotFound, "call_log", "call log not found")

// Repository persists CallLog aggregates. Append-only — no UpdateByID;
// the Slice-1 surface is intentionally minimal.
//
// Tenant-scoped reads/writes per ADR 0006; the adapter binds tenant
// context on the connection before queries touch the table.
type Repository interface {
	// Add persists a brand-new CallLog from [New] and drains its
	// events into the crm.outbox in the same transaction.
	Add(ctx context.Context, c *CallLog) error

	// GetByID returns the call log or [ErrNotFound].
	GetByID(ctx context.Context, id ID) (*CallLog, error)

	// ListByLead returns every call log for a lead, newest first.
	// No pagination at slice 1 — high-volume leads can switch to a
	// paged variant in slice 2.
	ListByLead(ctx context.Context, leadID crmlead.ID) ([]*CallLog, error)
}
