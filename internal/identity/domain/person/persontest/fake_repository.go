// Package persontest provides the in-memory FakeRepository implementing
// [person.Repository]. Used by app-layer handler tests + downstream
// integration scenarios that need a working person store without a
// Postgres dependency.
//
// TDL CANON (ThreeDotsLabs Wild Workouts + "Go with the Domain"):
//
//   - The fake lives in a sibling package <aggregate>test/, co-located
//     with the domain aggregate it implements an interface from. Same
//     visibility surface as the aggregate itself; no special test-side
//     plumbing.
//   - The fake is a FAITHFUL implementation of [person.Repository] —
//     not a mock-with-canned-responses. It honors every contract
//     guarantee: ErrNotFound on missing IDs / hashes / emails, the
//     globally-unique email constraint as [person.ErrEmailTaken] on
//     Add, GetByIDs returns a map keyed by input ID (NOT in input
//     order) with missing rows silently absent.
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
package persontest

import (
	"context"

	"github.com/leadkart/leadkart-go/internal/common/email"
	"github.com/leadkart/leadkart-go/internal/identity/domain/person"
)

// FakeRepository is the in-memory implementation of [person.Repository].
// Zero-value-NOT-usable — construct via [NewFakeRepository] so the
// internal maps are initialised. Single-test-owner: do NOT share one
// instance across tests; each test creates its own.
type FakeRepository struct {
	// rows is the Person index by ID. Persons are not row-level soft-
	// deleted; the aggregate carries an IsAnonymised flag for the
	// retention path. Reads do NOT filter anonymised rows — the
	// aggregate exposes that state and callers decide.
	rows map[person.ID]*person.Person

	// emails is the email → person.ID index for ErrEmailTaken
	// enforcement. Mirrors the SQL unique constraint on persons.email
	// (globally unique per database.md canon).
	emails map[string]person.ID
}

// NewFakeRepository returns an empty in-memory person repository.
// Single-test-owner — each test should construct its own instance;
// do NOT share one fake across parallel tests (no internal sync).
func NewFakeRepository() *FakeRepository {
	return &FakeRepository{
		rows:   make(map[person.ID]*person.Person),
		emails: make(map[string]person.ID),
	}
}

// Compile-time interface conformance gate. Drift in
// [person.Repository] (a method renamed, signature changed) breaks at
// build time before any test runs.
var _ person.Repository = (*FakeRepository)(nil)

// Add persists a brand-new Person. Returns [person.ErrEmailTaken] if a
// Person with the same email already exists (mirrors the SQL adapter's
// unique-constraint translation).
func (f *FakeRepository) Add(_ context.Context, p *person.Person) error {

	if _, taken := f.emails[p.Email().String()]; taken {
		return person.ErrEmailTaken
	}
	f.rows[p.ID()] = p
	f.emails[p.Email().String()] = p.ID()
	return nil
}

// UpdateByID loads, mutates via updateFn, persists. Returns
// [person.ErrNotFound] if the Person doesn't exist.
//
// The fake doesn't deep-copy the Person before passing to updateFn; the
// caller observes mutations even if it returns (false, nil). This
// mirrors the pg adapter's behavior — both rely on the aggregate's
// invariants being re-checked at persist time, not snapshot-rollback.
//
// Re-keys the emails index if the mutation changed the email
// (email-change confirm flow) so subsequent GetByEmail lookups
// resolve to the new address.
func (f *FakeRepository) UpdateByID(_ context.Context, id person.ID, updateFn func(*person.Person) (bool, error)) error {

	p, ok := f.rows[id]
	if !ok {
		return person.ErrNotFound
	}
	commit, err := updateFn(p)
	if err != nil {
		return err
	}
	if !commit {
		return nil
	}
	// Refresh the emails index in case the email rotated.
	for k, kID := range f.emails {
		if kID == id {
			delete(f.emails, k)
			break
		}
	}
	f.emails[p.Email().String()] = id
	return nil
}

// UpdateLockoutState is the hot-path direct-update for the Login
// flow's wrong-password + lockout-clear branches. In the fake this is
// a no-op: the aggregate already carries the mutated lockout state
// (caller invokes [person.RegisterFailedLogin] etc. BEFORE this
// method), and we don't separately project columns. The pg adapter
// drains events same-tx; in the fake no outbox exists, so events
// recorded on the aggregate stay on the aggregate until a caller
// explicitly PullEvents() — same semantics as Add/UpdateByID.
func (f *FakeRepository) UpdateLockoutState(_ context.Context, _ *person.Person) error {

	return nil
}

// GetByID returns the Person or [person.ErrNotFound].
func (f *FakeRepository) GetByID(_ context.Context, id person.ID) (*person.Person, error) {

	p, ok := f.rows[id]
	if !ok {
		return nil, person.ErrNotFound
	}
	return p, nil
}

// GetByIDs hydrates the supplied IDs into a map keyed by ID. Missing
// IDs are silently absent — NOT an error, per the interface contract
// (race-with-anonymise / race-with-delete is possible). An empty input
// slice returns an empty (non-nil) map.
func (f *FakeRepository) GetByIDs(_ context.Context, ids []person.ID) (map[person.ID]*person.Person, error) {

	out := make(map[person.ID]*person.Person, len(ids))
	for _, id := range ids {
		if p, ok := f.rows[id]; ok {
			out[id] = p
		}
	}
	return out, nil
}

// GetByEmail returns the Person by globally-unique email or
// [person.ErrNotFound].
func (f *FakeRepository) GetByEmail(_ context.Context, e email.Address) (*person.Person, error) {

	id, ok := f.emails[e.String()]
	if !ok {
		return nil, person.ErrNotFound
	}
	p, ok := f.rows[id]
	if !ok {
		return nil, person.ErrNotFound
	}
	return p, nil
}

// GetByPasswordResetTokenHash returns the Person whose pending
// password-reset matches the supplied hash, or [person.ErrNotFound].
// Linear scan; the SQL adapter uses a partial unique index. A zero
// hash returns ErrNotFound (mirrors the adapter's IsZero short-circuit).
func (f *FakeRepository) GetByPasswordResetTokenHash(_ context.Context, hash person.PasswordResetTokenHash) (*person.Person, error) {

	if hash.IsZero() {
		return nil, person.ErrNotFound
	}
	for _, p := range f.rows {
		pending := p.PendingPasswordReset()
		if pending.IsZero() {
			continue
		}
		if pending.Hash() == hash {
			return p, nil
		}
	}
	return nil, person.ErrNotFound
}

// GetByEmailChangeTokenHash returns the Person whose pending
// email-change matches the supplied hash, or [person.ErrNotFound].
// Same hash-only lookup shape as [GetByPasswordResetTokenHash].
func (f *FakeRepository) GetByEmailChangeTokenHash(_ context.Context, hash person.EmailChangeTokenHash) (*person.Person, error) {

	if hash.IsZero() {
		return nil, person.ErrNotFound
	}
	for _, p := range f.rows {
		pending := p.PendingEmailChange()
		if pending.IsZero() {
			continue
		}
		if pending.Hash() == hash {
			return p, nil
		}
	}
	return nil, person.ErrNotFound
}
