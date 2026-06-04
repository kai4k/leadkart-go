package workitem

import (
	"context"
	"errors"
	"time"

	"github.com/leadkart/leadkart-go/internal/common/errs"
	"github.com/leadkart/leadkart-go/internal/common/pagination"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)

// ----- Sentinel errors ------------------------------------------------------

// ErrNotFound is returned by Repository.GetByID when no work item
// exists for the supplied identifier (or RLS silently hides a
// cross-tenant row).
var ErrNotFound = errs.New(errs.KindNotFound, "work_item", "work item not found")

// ErrAlreadyExistsForSource is returned by Repository.Add when an
// auto-created task would violate the (source_entity_type,
// source_entity_id) partial unique index — i.e. a subscriber replay
// re-attempts a task that's still open. Subscriber-side handlers
// translate this into a no-op-ack.
var ErrAlreadyExistsForSource = errs.New(errs.KindAlreadyExists, "work_item",
	"open work item already exists for source entity")

// ----- Repository contract --------------------------------------------------

// Repository persists WorkItem aggregates. The contract is declared
// here in the domain package per Cheney "accept interfaces, return
// structs" — the CONSUMER (the application service) defines what it
// needs; adapters in internal/tasks/adapters/ implement.
//
// All methods MUST be safe for concurrent use by multiple goroutines.
//
// Tenant scoping (ADR 0062 — TDL canon): every method that takes an
// ID without an aggregate ALSO takes an EXPLICIT tenantID parameter.
// The adapter binds the GUC from the parameter at tx-begin (NOT from
// ctx). RLS remains the security backstop.
type Repository interface {
	// Add persists a brand-new work item from [NewManual] or
	// [NewAutoCreated]. The aggregate carries its own TenantID.
	// PullEvents drains inside the same transaction.
	//
	// Returns [ErrAlreadyExistsForSource] when the partial unique index
	// uq_tasks_source_open trips (subscriber-side idempotency).
	Add(ctx context.Context, w *WorkItem) error

	// UpdateByID loads the work item (scoped to tenantID), runs
	// updateFn (which mutates state via aggregate methods), then
	// persists + emits events — all in one transaction. TDL Sep 2024
	// canon.
	//
	// updateFn returns (true, nil) to commit; (false, nil) to abort
	// without changes; (_, err) to roll back.
	//
	// Returns [ErrNotFound] when the work item doesn't exist in the
	// tenant (or RLS hides it).
	UpdateByID(ctx context.Context, tenantID tenant.ID, id ID, updateFn func(*WorkItem) (bool, error)) error

	// GetByID returns the work item from the supplied tenant or
	// [ErrNotFound]. Read-only path; does not drain events.
	GetByID(ctx context.Context, tenantID tenant.ID, id ID) (*WorkItem, error)

	// GetOpenBySource returns the OPEN (pending / in_progress) work
	// item for the supplied source pair, or [ErrNotFound] when none
	// exists in the tenant. Used by the auto-complete-by-source flow
	// to find the matching CallbackReminder when a new CallLog logs
	// the corresponding callback.
	GetOpenBySource(ctx context.Context, tenantID tenant.ID, entityType, entityID string) (*WorkItem, error)

	// ListPage is the cursor-paginated list endpoint per ADR 0038.
	// Sort tuple is (due_at DESC, id DESC). filter narrows the
	// returned set; all filter fields are optional (zero values mean
	// no filter).
	ListPage(ctx context.Context, tenantID tenant.ID, filter ListFilter, cursor pagination.Cursor, pageSize int) (pagination.Page[*WorkItem], error)

	// ListOverdueCandidates returns up to `limit` work items in
	// pending/in_progress state whose due_at is strictly before
	// `asOf`. Used by the periodic overdue-scan job per BRD §6.8.
	// Cross-tenant (scanner runs under platform scope); tenantID is
	// optional and zero means "all tenants".
	ListOverdueCandidates(ctx context.Context, tenantID tenant.ID, asOf time.Time, limit int) ([]*WorkItem, error)

	// ListPurgeCandidates returns up to `limit` work items in
	// completed/cancelled state whose terminal timestamp
	// (completed_at OR cancelled_at) is strictly before `before`.
	// Used by the daily purge job per BRD §6.8. Cross-tenant; tenantID
	// optional.
	ListPurgeCandidates(ctx context.Context, tenantID tenant.ID, before time.Time, limit int) ([]*WorkItem, error)

	// DeleteByID flips is_deleted = true for the supplied work item
	// in the supplied tenant (soft delete). Idempotent — already-
	// deleted returns nil. Returns [ErrNotFound] if no matching live
	// row exists.
	DeleteByID(ctx context.Context, tenantID tenant.ID, id ID) error

	// CountDashboard returns the BRD §6.8 dashboard tallies for a
	// membership scope. When visibleMembershipIDs is non-empty, the
	// counts span those memberships (team view); otherwise the counts
	// are scoped to a single membership (self view). `asOf` is the
	// reference instant for Today (same calendar day) + CompletedToday.
	CountDashboard(ctx context.Context, tenantID tenant.ID, visibleMembershipIDs []string, asOf time.Time) (DashboardCounts, error)
}

// ListFilter narrows the work-item-list result set. All fields are
// optional (zero value disables the filter).
type ListFilter struct {
	State                  State
	Type                   Type
	Priority               Priority
	AssignedToMembershipID string
	BatchID                string
	DueBefore              time.Time
	DueAfter               time.Time

	// SelfFilter constrains the result set to a single assignee. When
	// set + non-zero, overrides AssignedToMembershipID. Used by the
	// app layer to enforce the "only my tasks" rule for callers
	// without [tasks.work_items.read_all].
	SelfFilter string
}

// DashboardCounts is the BRD §6.8 dashboard tally bundle.
type DashboardCounts struct {
	Today          int // open tasks due on the same calendar day as asOf
	Upcoming       int // open tasks due after asOf's calendar day
	Overdue        int // tasks in StateOverdue
	CompletedToday int // tasks completed on the same calendar day as asOf
	TotalPending   int // tasks in StatePending
}

// Compile-time guarantee that the sentinel errors are wrapped-comparable.
var _ = errors.Is
