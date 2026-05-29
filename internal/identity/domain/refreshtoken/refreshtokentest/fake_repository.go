// Package refreshtokentest provides the in-memory FakeRepository
// implementing [refreshtoken.Repository]. Used by app-layer handler
// tests + downstream integration scenarios that need a working refresh-
// token family store without a Postgres dependency.
//
// TDL CANON (ThreeDotsLabs Wild Workouts + "Go with the Domain"):
//
//   - The fake lives in a sibling package <aggregate>test/, co-located
//     with the domain aggregate it implements an interface from. Same
//     visibility surface as the aggregate itself; no special test-side
//     plumbing.
//   - The fake is a FAITHFUL implementation of [refreshtoken.Repository]
//     — not a mock-with-canned-responses. It honors every contract
//     guarantee: ErrNotFound on missing IDs / unmatched token hashes,
//     ListActiveForPerson filters revoked families, GetByTokenHash
//     walks every family's token bag (mirrors the unique-per-system
//     SHA-256 invariant).
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
package refreshtokentest

import (
	"context"

	"github.com/leadkart/leadkart-go/internal/identity/domain/person"
	"github.com/leadkart/leadkart-go/internal/identity/domain/refreshtoken"
)

// FakeRepository is the in-memory implementation of
// [refreshtoken.Repository]. Zero-value-NOT-usable — construct via
// [NewFakeRepository] so the internal map is initialised. Single-test-
// owner: do NOT share one instance across tests; each test creates its
// own.
type FakeRepository struct {
	// families is the active + revoked family index by FamilyID. The
	// aggregate carries its own IsRevoked flag; reads do NOT filter
	// revoked rows at the GetByID / GetByTokenHash level (mirrors the
	// SQL adapter — pgrest exposes the full lifecycle of a family).
	// ListActiveForPerson is the only method that filters revoked.
	families map[refreshtoken.FamilyID]*refreshtoken.Family
}

// NewFakeRepository returns an empty in-memory family repository.
// Single-test-owner — each test should construct its own instance;
// do NOT share one fake across parallel tests (no internal sync).
func NewFakeRepository() *FakeRepository {
	return &FakeRepository{families: make(map[refreshtoken.FamilyID]*refreshtoken.Family)}
}

// Compile-time interface conformance gate. Drift in
// [refreshtoken.Repository] (a method renamed, signature changed)
// breaks at build time before any test runs.
var _ refreshtoken.Repository = (*FakeRepository)(nil)

// Add persists a brand-new family + its first token. No
// uniqueness-on-add check beyond the implicit map-key collision (which
// would be a programmer error — FamilyID generation is UUID-based).
func (f *FakeRepository) Add(_ context.Context, fam *refreshtoken.Family) error {

	f.families[fam.ID()] = fam
	return nil
}

// UpdateByID loads, mutates via updateFn, persists — faithfully
// mirroring the pg adapter's load→updateFn→(persist?) contract.
// Returns [refreshtoken.ErrNotFound] if the family doesn't exist.
//
// The pg adapter loads inside the tx, runs updateFn, and on
// `(false, nil)` returns WITHOUT writing any row or draining events —
// the mutations never reach storage. To honor that LSP contract the
// fake snapshots the stored aggregate BEFORE calling updateFn and, on
// `(false, nil)`, ROLLS the stored row back to that snapshot — so
// mutations made under a no-persist decision are discarded. Pointer
// identity is preserved so a commit==true caller still observes the
// mutation it made through its own reference.
func (f *FakeRepository) UpdateByID(_ context.Context, id refreshtoken.FamilyID, updateFn func(*refreshtoken.Family) (bool, error)) error {

	stored, ok := f.families[id]
	if !ok {
		return refreshtoken.ErrNotFound
	}
	snapshot := cloneFamily(stored)
	commit, err := updateFn(stored)
	if err != nil {
		return err
	}
	if !commit {
		*stored = *snapshot // roll back mutations — mirror the adapter's no-persist branch
		return nil
	}
	return nil
}

// cloneFamily produces an independent copy of a Family (and its token
// chain) via a Snapshot round-trip (the same rehydration path the pg
// adapter uses on load), so mutations on the working copy can't leak
// into the stored original until the updateFn elects to commit. Pending
// events are intentionally not carried — the adapter loads an event-free
// aggregate too.
func cloneFamily(fam *refreshtoken.Family) *refreshtoken.Family {
	toks := fam.AllTokens()
	snaps := make([]refreshtoken.TokenSnapshot, len(toks))
	for i, t := range toks {
		snaps[i] = refreshtoken.TokenSnapshot{
			ID:           t.ID(),
			Hash:         t.Hash(),
			Generation:   t.Generation(),
			IssuedAt:     t.IssuedAt(),
			ExpiresAt:    t.ExpiresAt(),
			ConsumedAt:   t.ConsumedAt(),
			ReplacedByID: t.ReplacedByID(),
		}
	}
	return refreshtoken.UnmarshalFromDB(refreshtoken.FamilySnapshot{
		ID:           fam.ID(),
		PersonID:     fam.PersonID(),
		TenantID:     fam.TenantID(),
		DeviceLabel:  fam.DeviceLabel(),
		CreatedAt:    fam.CreatedAt(),
		LastUsedAt:   fam.LastUsedAt(),
		RevokedAt:    fam.RevokedAt(),
		RevokeReason: fam.RevokeReason(),
		Tokens:       snaps,
	})
}

// GetByID returns the family + tokens or [refreshtoken.ErrNotFound].
// Returns revoked families too — mirrors the SQL adapter which exposes
// the full lifecycle for the rotation flow's "this token was revoked"
// branch detection.
func (f *FakeRepository) GetByID(_ context.Context, id refreshtoken.FamilyID) (*refreshtoken.Family, error) {

	fam, ok := f.families[id]
	if !ok {
		return nil, refreshtoken.ErrNotFound
	}
	return fam, nil
}

// GetByTokenHash resolves a presented refresh token to its family by
// SHA-256 hash. Linear scan over every family's token bag; the SQL
// adapter uses an index on refresh_tokens.token_hash. Returns
// [refreshtoken.ErrNotFound] if no family contains a token with the
// supplied hash.
func (f *FakeRepository) GetByTokenHash(_ context.Context, hash refreshtoken.TokenHash) (*refreshtoken.Family, error) {

	for _, fam := range f.families {
		for _, tok := range fam.AllTokens() {
			if tok.Hash().Equal(hash) {
				return fam, nil
			}
		}
	}
	return nil, refreshtoken.ErrNotFound
}

// ListActiveForPerson returns all non-revoked families for a Person.
// Used by the "manage sessions" UI + family-cap enforcement at mint
// time.
func (f *FakeRepository) ListActiveForPerson(_ context.Context, personID person.ID) ([]*refreshtoken.Family, error) {

	var out []*refreshtoken.Family
	for _, fam := range f.families {
		if fam.PersonID() == personID && !fam.IsRevoked() {
			out = append(out, fam)
		}
	}
	return out, nil
}
