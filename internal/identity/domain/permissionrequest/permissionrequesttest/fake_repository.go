// Package permissionrequesttest provides the in-memory FakeRepository
// implementing [permissionrequest.Repository]. Used by app-layer
// handler tests + downstream integration scenarios that need a working
// elevation-request store without a Postgres dependency.
//
// TDL CANON (ThreeDotsLabs Wild Workouts + "Go with the Domain"):
//
//   - The fake lives in a sibling package <aggregate>test/, co-located
//     with the domain aggregate it implements an interface from. Same
//     visibility surface as the aggregate itself; no special test-side
//     plumbing.
//   - The fake is a FAITHFUL implementation of
//     [permissionrequest.Repository] — not a mock-with-canned-responses.
//     It honors every contract guarantee: ErrNotFound on missing IDs,
//     ErrPendingRequestExists on a second Pending row for the same
//     (requester, permission) tuple (mirrors the partial unique index
//     uq_permission_requests_pending), state-transition awareness on
//     UpdateByID (Pending → terminal frees the slot), drain-events on
//     commit.
//   - Single-test-owner pattern: each test creates its OWN
//     FakeRepository via [NewFakeRepository] — no shared mutable state
//     across tests. t.Parallel is naturally safe because no two tests
//     share the same fake instance. This is TDL canon: fakes don't
//     need sync primitives because they're per-test, and putting sync
//     in domain-co-located test packages would trip
//     TestArch_NoGoroutinesInDomain (domain layer is concurrency-free
//     by design — Bryan Mills "Rethinking Concurrency Patterns").
//
// Why fakes, not mocks: per TDL "Go with the Domain" ch. 8, mocks
// couple the test to the call-pattern of the SUT (Subject Under Test);
// fakes couple to the CONTRACT. Refactoring the SUT to use the
// interface differently breaks mock-tests but leaves fake-tests
// green. The contract is the load-bearing thing.
package permissionrequesttest

import (
	"context"

	"github.com/leadkart/leadkart-go/internal/common/pagination"
	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/identity/domain/permissionrequest"
)

// FakeRepository is the in-memory implementation of
// [permissionrequest.Repository]. Zero-value-NOT-usable — construct
// via [NewFakeRepository] so the internal maps are initialised.
// Single-test-owner: do NOT share one instance across tests; each test
// creates its own.
type FakeRepository struct {
	// requests is the full Request index by ID, covering every State
	// (Pending / Approved / Denied / Cancelled). Reads do NOT filter
	// by state at the GetByID level — callers inspect r.State().
	requests map[permissionrequest.ID]*permissionrequest.Request

	// pending is the (requester|permission) → permissionrequest.ID
	// index for ErrPendingRequestExists enforcement. Mirrors the
	// partial unique index uq_permission_requests_pending which
	// covers the at-most-one-pending invariant per (requester,
	// permission) tuple. UpdateByID maintains this index as state
	// transitions out of Pending.
	pending map[string]permissionrequest.ID
}

// NewFakeRepository returns an empty in-memory request repository.
// Single-test-owner — each test should construct its own instance;
// do NOT share one fake across parallel tests (no internal sync).
func NewFakeRepository() *FakeRepository {
	return &FakeRepository{
		requests: make(map[permissionrequest.ID]*permissionrequest.Request),
		pending:  make(map[string]permissionrequest.ID),
	}
}

// Compile-time interface conformance gate. Drift in
// [permissionrequest.Repository] (a method renamed, signature changed)
// breaks at build time before any test runs.
var _ permissionrequest.Repository = (*FakeRepository)(nil)

// Add persists a brand-new Pending Request. Returns
// [permissionrequest.ErrPendingRequestExists] if another Pending row
// already exists for the same (requester, permission) tuple — mirrors
// the SQL adapter's translation of SQLSTATE 23505 from the partial
// unique index uq_permission_requests_pending.
//
// Drains domain events from the aggregate so callers can chain
// PullEvents() at the test layer without double-counting.
func (f *FakeRepository) Add(_ context.Context, r *permissionrequest.Request) error {

	if r.State() == permissionrequest.StatePending {
		k := pendingKey(r.RequesterMembershipID(), r.Permission().Name())
		if _, exists := f.pending[k]; exists {
			return permissionrequest.ErrPendingRequestExists
		}
		f.pending[k] = r.ID()
	}
	f.requests[r.ID()] = r
	r.PullEvents()
	return nil
}

// UpdateByID loads, mutates via updateFn, persists. Returns
// [permissionrequest.ErrNotFound] if the row doesn't exist.
//
// Maintains the pending-tuple index: if the mutation moves State away
// from Pending (→ Approved / Denied / Cancelled), the
// (requester, permission) slot is freed so a future Add can succeed
// for the same tuple. Drains events on commit, matching the SQL
// adapter's drainPermissionRequestEvents → outbox flow.
func (f *FakeRepository) UpdateByID(_ context.Context, id permissionrequest.ID, updateFn func(*permissionrequest.Request) (bool, error)) error {

	x, ok := f.requests[id]
	if !ok {
		return permissionrequest.ErrNotFound
	}
	prevState := x.State()
	prevPerm := x.Permission().Name()
	prevRequester := x.RequesterMembershipID()
	commit, err := updateFn(x)
	if err != nil {
		return err
	}
	if !commit {
		return nil
	}
	// State transitioned out of Pending — free the partial-index slot.
	if prevState == permissionrequest.StatePending && x.State() != permissionrequest.StatePending {
		delete(f.pending, pendingKey(prevRequester, prevPerm))
	}
	x.PullEvents()
	return nil
}

// GetByID returns the Request or [permissionrequest.ErrNotFound].
func (f *FakeRepository) GetByID(_ context.Context, id permissionrequest.ID) (*permissionrequest.Request, error) {

	x, ok := f.requests[id]
	if !ok {
		return nil, permissionrequest.ErrNotFound
	}
	return x, nil
}

// GetPendingForMembership returns every Pending request the supplied
// Membership has open. Used by the application service to pre-validate
// the at-most-one-pending invariant before mint AND by the requester's
// UI ("show my pending elevations").
func (f *FakeRepository) GetPendingForMembership(_ context.Context, m membership.ID) ([]*permissionrequest.Request, error) {

	var out []*permissionrequest.Request
	for _, x := range f.requests {
		if x.RequesterMembershipID() == m && x.State() == permissionrequest.StatePending {
			out = append(out, x)
		}
	}
	return out, nil
}

// ListPendingApprovableBy returns the keyset-paginated queue of
// Pending requests where approver_membership_id matches the supplied
// Membership ID. Per ADR 0038 cursor semantics: first page passes the
// zero pagination.Cursor + the adapter applies its own sentinel
// internally. The fake delegates page assembly to
// [pagination.BuildPage] using (CreatedAt, ID) as the sort tuple.
func (f *FakeRepository) ListPendingApprovableBy(_ context.Context, approver membership.ID, pageSize int, _ pagination.Cursor) (pagination.Page[*permissionrequest.Request], error) {

	var items []*permissionrequest.Request
	for _, x := range f.requests {
		if x.ApproverMembershipID() == approver && x.State() == permissionrequest.StatePending {
			items = append(items, x)
		}
	}
	return pagination.BuildPage(items, pageSize, func(req *permissionrequest.Request) pagination.Cursor {
		return pagination.Cursor{SortValue: req.CreatedAt(), ID: req.ID().String()}
	}), nil
}

// ListByRequester returns the keyset-paginated history of every state
// for the supplied Requester Membership. Used by the requester's
// "my requests" UI.
func (f *FakeRepository) ListByRequester(_ context.Context, requester membership.ID, pageSize int, _ pagination.Cursor) (pagination.Page[*permissionrequest.Request], error) {

	var items []*permissionrequest.Request
	for _, x := range f.requests {
		if x.RequesterMembershipID() == requester {
			items = append(items, x)
		}
	}
	return pagination.BuildPage(items, pageSize, func(req *permissionrequest.Request) pagination.Cursor {
		return pagination.Cursor{SortValue: req.CreatedAt(), ID: req.ID().String()}
	}), nil
}

// pendingKey is the (membership, permission name) composite used for
// ErrPendingRequestExists enforcement. Mirrors the partial unique
// index keyspace.
func pendingKey(m membership.ID, perm string) string {
	return m.String() + "|" + perm
}
