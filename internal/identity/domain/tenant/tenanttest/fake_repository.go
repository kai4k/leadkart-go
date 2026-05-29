// Package tenanttest provides the in-memory FakeRepository implementing
// [tenant.Repository]. Used by app-layer handler tests + downstream
// integration scenarios that need a working tenant store without a
// Postgres dependency.
//
// TDL CANON (ThreeDotsLabs Wild Workouts + "Go with the Domain"):
//
//   - The fake lives in a sibling package <aggregate>test/, co-located
//     with the domain aggregate it implements an interface from. Same
//     visibility surface as the aggregate itself; no special test-side
//     plumbing.
//   - The fake is a FAITHFUL implementation of [tenant.Repository] — not
//     a mock-with-canned-responses. It honors every contract guarantee:
//     ErrNotFound on missing IDs/slugs, ErrSlugTaken on duplicate slug
//     at Add time (mirrors the SQL unique-constraint translation),
//     created_at ordering on ListAll, idempotent HardDeleteRow.
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
package tenanttest

import (
	"context"
	"slices"

	"github.com/leadkart/leadkart-go/internal/common/slug"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)

// FakeRepository is the in-memory implementation of [tenant.Repository].
// Zero-value-NOT-usable — construct via [NewFakeRepository] so the
// internal maps are initialised. Single-test-owner: do NOT share one
// instance across tests; each test creates its own.
type FakeRepository struct {
	// rows is the tenant index by ID. Tenants are not soft-deleted at
	// the row level (status='Deleted' is the aggregate-level terminal
	// state; HardDeleteRow physically removes the row), so we just
	// delete on HardDeleteRow rather than carrying a tombstone flag.
	rows map[tenant.ID]*tenant.Tenant

	// slugs is the slug → tenant.ID index for ErrSlugTaken enforcement.
	// Mirrors the SQL unique constraint on tenants.slug — there is no
	// partial-index here because tenants.slug is a global unique key
	// regardless of status. A HardDeleteRow frees the slug.
	slugs map[string]tenant.ID
}

// NewFakeRepository returns an empty in-memory tenant repository.
// Single-test-owner — each test should construct its own instance;
// do NOT share one fake across parallel tests (no internal sync).
func NewFakeRepository() *FakeRepository {
	return &FakeRepository{
		rows:  make(map[tenant.ID]*tenant.Tenant),
		slugs: make(map[string]tenant.ID),
	}
}

// Compile-time interface conformance gate. Drift in [tenant.Repository]
// (a method renamed, signature changed) breaks at build time before
// any test runs.
var _ tenant.Repository = (*FakeRepository)(nil)

// Add persists a brand-new tenant. Returns [tenant.ErrSlugTaken] if a
// tenant with the same slug already exists. Mirrors the SQL adapter's
// unique-constraint translation.
func (f *FakeRepository) Add(_ context.Context, t *tenant.Tenant) error {

	if _, taken := f.slugs[t.Slug().String()]; taken {
		return tenant.ErrSlugTaken
	}
	f.rows[t.ID()] = t
	f.slugs[t.Slug().String()] = t.ID()
	return nil
}

// UpdateByID loads, mutates via updateFn, then either persists
// (commit=true) or rolls back (commit=false / err). Returns
// [tenant.ErrNotFound] if the tenant doesn't exist.
//
// The fake doesn't deep-copy the tenant before passing to updateFn; the
// caller observes mutations even if it returns (false, nil). This
// mirrors the pg adapter's behavior — both rely on the aggregate's
// invariants being re-checked at persist time, not snapshot-rollback.
func (f *FakeRepository) UpdateByID(_ context.Context, id tenant.ID, updateFn func(*tenant.Tenant) (bool, error)) error {

	t, ok := f.rows[id]
	if !ok {
		return tenant.ErrNotFound
	}
	commit, err := updateFn(t)
	if err != nil {
		return err
	}
	if !commit {
		return nil
	}
	// Aggregate may have rotated slug as part of the mutation; re-key
	// the slugs index from scratch for this tenant.
	for k, kID := range f.slugs {
		if kID == id {
			delete(f.slugs, k)
			break
		}
	}
	f.slugs[t.Slug().String()] = id
	return nil
}

// GetByID returns the tenant or [tenant.ErrNotFound].
func (f *FakeRepository) GetByID(_ context.Context, id tenant.ID) (*tenant.Tenant, error) {

	t, ok := f.rows[id]
	if !ok {
		return nil, tenant.ErrNotFound
	}
	return t, nil
}

// GetBySlug returns the tenant by URL slug or [tenant.ErrNotFound].
func (f *FakeRepository) GetBySlug(_ context.Context, s slug.Slug) (*tenant.Tenant, error) {

	id, ok := f.slugs[s.String()]
	if !ok {
		return nil, tenant.ErrNotFound
	}
	t, ok := f.rows[id]
	if !ok {
		return nil, tenant.ErrNotFound
	}
	return t, nil
}

// ListAll returns every tenant ordered by created_at — same order as
// the SQL adapter (`ORDER BY created_at`) so callers can rely on
// stable ordering.
func (f *FakeRepository) ListAll(_ context.Context) ([]*tenant.Tenant, error) {

	out := make([]*tenant.Tenant, 0, len(f.rows))
	for _, t := range f.rows {
		out = append(out, t)
	}
	slices.SortFunc(out, func(a, b *tenant.Tenant) int {
		return a.CreatedAt().Compare(b.CreatedAt())
	})
	return out, nil
}

// HardDeleteRow physically removes the tenant row. Idempotent —
// deleting an already-deleted row no-ops without error (mirrors the
// SQL adapter's DELETE … WHERE id = $1 semantics).
func (f *FakeRepository) HardDeleteRow(_ context.Context, id tenant.ID) error {

	t, ok := f.rows[id]
	if !ok {
		return nil
	}
	delete(f.slugs, t.Slug().String())
	delete(f.rows, id)
	return nil
}
