package reminder

import (
	"context"
	"errors"

	"github.com/leadkart/leadkart-go/internal/common/errs"
	"github.com/leadkart/leadkart-go/internal/common/pagination"
	"github.com/leadkart/leadkart-go/internal/crm/domain/crmlead"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)

// ----- Sentinel errors ------------------------------------------------------

// ErrNotFound is returned by [Repository.GetByID] / [Repository.UpdateByID]
// when no reminder exists for the supplied identifier (or RLS hides a
// cross-tenant row).
var ErrNotFound = errs.New(errs.KindNotFound, "reminder", "reminder not found")

// ErrAlreadyExists is returned by [Repository.Add] when the unique-
// constraint guard fires. Used by:
//
//   - The CallLogged subscriber: a duplicate broker delivery for the
//     same call_log_id finds an EXISTING pending callback reminder via
//     the partial unique index (tenant_id, source_call_log_id) WHERE
//     type='callback' AND state='pending'. The subscriber treats this
//     as success (ACK).
//   - The mature-lead scheduler: the partial unique index on
//     (tenant_id, lead_id) WHERE type='mature_lead' AND state='pending'
//     ensures at-most-one outstanding mature-lead reminder per lead.
var ErrAlreadyExists = errs.New(errs.KindConflict, "reminder", "reminder already exists")

// ----- Repository contract --------------------------------------------------

// Repository persists Reminder aggregates. Per Cheney "accept
// interfaces, return structs" — the CONSUMER (the app handler) declares
// what it needs; adapters in internal/crm/adapters/ implement.
//
// Tenant scoping (ADR 0062 — TDL canon): every method that takes an ID
// without an aggregate ALSO takes an EXPLICIT tenantID parameter. The
// adapter binds the GUC from that parameter at tx-begin; ctx-tenancy
// is NOT read at the adapter layer.
type Repository interface {
	// Add persists a brand-new reminder from one of the factory
	// functions. The aggregate carries its TenantID — no separate
	// param needed. PullEvents are drained inside the same transaction
	// + appended to crm.outbox per ADR 0027.
	//
	// Returns [ErrAlreadyExists] when the partial unique index on
	// callback / mature-lead source identifiers fires (SQLSTATE 23505
	// translation in the adapter).
	Add(ctx context.Context, r *Reminder) error

	// UpdateByID loads the reminder (scoped to tenantID), runs
	// updateFn (which mutates state via aggregate methods), then
	// persists + emits events — all in one transaction. TDL Sep 2024
	// canon.
	//
	// updateFn returns (true, nil) to commit; (false, nil) to abort
	// without changes; (_, err) to roll back.
	//
	// Returns [ErrNotFound] when the reminder doesn't exist in the
	// tenant (or RLS hides it).
	UpdateByID(ctx context.Context, tenantID tenant.ID, id ID, updateFn func(*Reminder) (bool, error)) error

	// GetByID returns the reminder from the supplied tenant or
	// [ErrNotFound]. Read-only path.
	GetByID(ctx context.Context, tenantID tenant.ID, id ID) (*Reminder, error)

	// ListPagePending returns the cursor-paginated set of PENDING
	// reminders in the tenant per ADR 0038. filter narrows the result
	// set; all filter fields are optional (zero value disables).
	//
	// The sort tuple is (due_at ASC, id ASC) — overdue + soon-due
	// reminders surface first ("today / upcoming / overdue" dashboard
	// per BRD §4.6).
	ListPagePending(ctx context.Context, tenantID tenant.ID, filter PendingFilter, cursor pagination.Cursor, pageSize int) (pagination.Page[*Reminder], error)

	// FindPendingMatureForLead returns the open (pending) mature-lead
	// reminder for the supplied lead, or [ErrNotFound] when none
	// exists. The mature-lead daily scan uses this for the at-most-
	// one-pending-per-lead idempotency check (a partial unique index
	// also enforces this at the DB layer; this helper avoids the round-
	// trip of catching the SQLSTATE 23505 on the hot scan path).
	FindPendingMatureForLead(ctx context.Context, tenantID tenant.ID, leadID crmlead.ID) (*Reminder, error)
}

// PendingFilter narrows the [Repository.ListPagePending] result set.
// All fields are optional (zero value disables the filter).
type PendingFilter struct {
	// AssigneeMembershipID restricts to a single membership when
	// non-empty. The HTTP handler defaults this to the caller's own
	// membership when the caller lacks crm.leads.read_all.
	AssigneeMembershipID string

	// Type restricts to a single reminder type when non-empty.
	Type Type

	// LeadID restricts to one lead when non-empty (lead-detail panel).
	LeadID crmlead.ID
}

// Compile-time guarantee that the sentinel errors are wrapped-comparable.
var _ = errors.Is
