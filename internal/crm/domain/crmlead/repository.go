package crmlead

import (
	"context"
	"errors"

	"github.com/leadkart/leadkart-go/internal/common/errs"
	"github.com/leadkart/leadkart-go/internal/common/pagination"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)

// ----- Sentinel errors ------------------------------------------------------

// ErrNotFound is returned by Repository.GetByID / GetBySourcePurchaseID
// when no lead exists for the supplied identifier (or RLS silently
// hides a cross-tenant row).
var ErrNotFound = errs.New(errs.KindNotFound, "crm_lead", "crm lead not found")

// ----- Repository contract --------------------------------------------------

// Repository persists CrmLead aggregates. The contract is declared here
// in the domain package per Cheney "accept interfaces, return structs" —
// the CONSUMER (the application service) defines what it needs;
// adapters in internal/crm/adapters/ implement.
//
// All methods MUST be safe for concurrent use by multiple goroutines.
//
// Tenant scoping (ADR 0062 — TDL canon): every method that takes an ID
// without an aggregate ALSO takes an EXPLICIT tenantID parameter. The
// adapter binds the GUC from the parameter at tx-begin (NOT from ctx-
// tenancy.WithID — that's a domain value in context, which Khorikov §11
// + Cheney mark as a hidden input). RLS remains the security backstop;
// the explicit param is the API surface contract.
type Repository interface {
	// Add persists a brand-new lead from [New] or [NewFromPurchaseSnapshot].
	// The aggregate already carries its TenantID — no separate param needed.
	// The aggregate's PullEvents are drained inside the same transaction
	// and appended to the crm.outbox table per ADR 0027.
	//
	// Returns nil on success. The subscriber path checks
	// GetBySourcePurchaseID FIRST for idempotency; a same-purchase-id
	// double-Add will surface as a UNIQUE-violation wrapped error.
	Add(ctx context.Context, l *CrmLead) error

	// UpdateByID loads the lead (scoped to tenantID), runs updateFn (which
	// mutates state via aggregate methods), then persists + emits events —
	// all in one transaction. TDL Sep 2024 canon.
	//
	// updateFn returns (true, nil) to commit; (false, nil) to abort
	// without changes; (_, err) to roll back.
	//
	// Returns [ErrNotFound] when the lead doesn't exist in the tenant
	// (or RLS hides it).
	UpdateByID(ctx context.Context, tenantID tenant.ID, id ID, updateFn func(*CrmLead) (bool, error)) error

	// GetByID returns the lead from the supplied tenant or [ErrNotFound].
	// Read-only path; does not drain events or open a write transaction.
	GetByID(ctx context.Context, tenantID tenant.ID, id ID) (*CrmLead, error)

	// GetBySourcePurchaseID returns the lead minted from the given
	// Platform purchase event under the supplied tenant scope, or
	// [ErrNotFound] when no such lead exists. The subscriber's
	// idempotency check uses this.
	GetBySourcePurchaseID(ctx context.Context, tenantID tenant.ID, purchaseID string) (*CrmLead, error)

	// ListPage is the cursor-paginated list endpoint per ADR 0038.
	// Sort tuple is (created_at DESC, id DESC). filter narrows the
	// returned set; all filter fields are optional (empty/zero means
	// no filter). The returned [pagination.Page] carries HasMore +
	// NextCursor for the next-page request.
	ListPage(ctx context.Context, tenantID tenant.ID, filter ListFilter, cursor pagination.Cursor, pageSize int) (pagination.Page[*CrmLead], error)
}

// ListFilter narrows the lead-list result set per BRD §6.3. All fields
// are optional (zero value disables the filter). Multi-select fields
// (ProductRanges, DosageForms) use AND semantics — the lead must contain
// ALL listed values, matching GIN @> behaviour.
type ListFilter struct {
	Stage                Stage
	Temperature          Temperature
	AssigneeMembershipID string // zero string means no filter (use OnlyAssignedToMe via SelfFilter)
	City                 string
	Pincode              string
	BusinessType         string
	MedicineSystem       string
	ProductRanges        []string
	DosageForms          []string
	NameQuery            string // pg_trgm partial-match search on contact_name

	// SelfFilter constrains the result set to a single assignee. When
	// set + non-zero, overrides AssigneeMembershipID. Used by the app
	// layer to enforce the "only my assigned leads" rule for callers
	// without [crm.leads.read_all].
	SelfFilter string
}

// Compile-time guarantee that the sentinel errors are wrapped-comparable.
var _ = errors.Is
