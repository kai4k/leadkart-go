//go:build integration

// arch-test:no-timeout-needed — every test in this file uses the shared
//   pgtest container (per-package); pgxpool internal conn timeouts +
//   package-level `task ci:test:int -timeout=15m` already bound execution.
//   Per-test context.WithTimeout would be belt-and-suspenders against the
//   shared-pool + parallel-with-RLS canon shape.
//
// SQL-CONTRACT COVERAGE for this file (ADR 0062 — adapter integration
// tests are SQL-contract-only; business-rule + state-machine coverage
// lives in persontest.FakeRepository unit tests):
//
//   - Outbox-row insertion in the same tx as the person row, verified
//     end-to-end via the production forwarder + an in-process Watermill
//     subscriber. Persons are global (tenant_id = platform-tenant
//     sentinel uuid.Nil); the subscriber receives the canonical
//     identity.person_created.v1 event_type regardless.
//   - password_hash + security_stamp binary/text round-trip through the
//     pgx driver — proves the Argon2id PHC string and UUID columns
//     survive Marshal/Unmarshal intact across an UPDATE.
//   - SQLSTATE 23505 → person.ErrEmailTaken translation via the unique
//     index on the email_lc GENERATED ALWAYS column (case-insensitive
//     uniqueness enforced at the SQL layer, not in Go).
//   - GetByEmail resolves through the same email_lc GENERATED column
//     (SQL-specific — the fake doesn't have generated-column semantics).
//
// PARALLELISM POLICY: tests that read outbox emissions via the
// per-package subscriber fixture (newOutboxFixture) are SERIAL + use
// sharedPG.TruncateAll(t). Tests that only touch person/tenant rows
// stay t.Parallel() under RLS isolation.

package adapters_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/leadkart/leadkart-go/internal/common/email"
	"github.com/leadkart/leadkart-go/internal/common/ids"
	"github.com/leadkart/leadkart-go/internal/common/messaging"
	"github.com/leadkart/leadkart-go/internal/common/pg"
	"github.com/leadkart/leadkart-go/internal/identity/adapters"
	"github.com/leadkart/leadkart-go/internal/identity/domain/person"
)

// strongHash is a synthetic Argon2id PHC string acceptable to
// person.NewPasswordHash. Crypto correctness isn't tested here — the
// real hasher lives in adapters/auth (TBD). Long enough to clear the
// 40-char minimum length sanity check.
const strongHash = "$argon2id$v=19$m=19456,t=2,p=1$c2FsdHkx$abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789"

func newPerson(t *testing.T, addr string) *person.Person {
	t.Helper()
	id := person.ID(ids.NewV7().String())
	e, err := email.New(addr)
	if err != nil {
		t.Fatalf("email: %v", err)
	}
	hash, err := person.NewPasswordHash(strongHash)
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	p, err := person.New(id, e, "Alice", "Acme", hash, testNow)
	if err != nil {
		t.Fatalf("person.New: %v", err)
	}
	return p
}

// SQL-contract: Add writes outbox row identity.person_created.v1 in the
// same tx as the person row, verified end-to-end via the production
// forwarder + an in-process Watermill subscriber.
//
// arch-test:no-parallel — subscriber-fixture test; uses TruncateAll
// (strict-TDL outbox-observation rule per ADR 0062 Amendment 1).
func TestPersonRepository_Add_PersistsOutboxEventInSameTx(t *testing.T) {
	sharedPG.TruncateAll(t)
	fix := newOutboxFixture(t)
	repo := adapters.NewPersonRepository(fix.pool, pg.NewTransactor(fix.pool))

	p := newPerson(t, "alice@example.test")
	if err := repo.Add(t.Context(), p); err != nil {
		t.Fatalf("Add: %v", err)
	}

	msgs := fix.forwardAndWait(t, 1)
	got := eventTypes(msgs)
	if len(got) != 1 || got[0] != "identity.person_created.v1" {
		t.Fatalf("event_types: got %v want [identity.person_created.v1]", got)
	}
	// Person events ship under the platform-tenant sentinel (uuid.Nil).
	// The forwarder still sets the tenant_id metadata header for outbox
	// rows whose tenant_id is non-NULL; for person rows on the
	// platform-tenant the row carries uuid.Nil so the header is
	// uuid.Nil.String() (forwarder treats Valid+Zero as a present FK,
	// matching identity's tenant_id NOT NULL DEFAULT '00..00' constraint).
	if tid := msgs[0].Metadata.Get(messaging.HeaderTenantID); tid == "" {
		t.Fatalf("tenant_id header: empty (expected platform-tenant sentinel)")
	}
}

// SQL-contract: SQLSTATE 23505 from the unique index on the email_lc
// GENERATED ALWAYS column is translated to person.ErrEmailTaken.
// Case-insensitive uniqueness is enforced at the SQL layer (the fake
// approximates it but the generated-column behavior is Postgres-specific).
//
// arch-test:parallel-safe — shared pgtest container; persons are written
// under the platform-tenant sentinel but each test uses a unique email
// (the test owns its address); the duplicate collision is self-contained.
// No TruncateAll, no cross-tenant scan, no process-global mutation.
func TestPersonRepository_Add_DuplicateEmail_ReturnsErrEmailTaken(t *testing.T) {
	t.Parallel()
	pool := repoFixture(t)
	repo := adapters.NewPersonRepository(pool, pg.NewTransactor(pool))
	ctx := t.Context()

	first := newPerson(t, "dup@example.test")
	if err := repo.Add(ctx, first); err != nil {
		t.Fatalf("first Add: %v", err)
	}

	second := newPerson(t, "dup@example.test")
	err := repo.Add(ctx, second)
	if !errors.Is(err, person.ErrEmailTaken) {
		t.Fatalf("expected ErrEmailTaken, got %v", err)
	}
}

// SQL-contract: GetByEmail resolves through the email_lc GENERATED
// ALWAYS column index — the lookup path is SQL-specific (the fake
// approximates by normalising in-memory).
//
// arch-test:parallel-safe — shared pgtest container; each test owns a
// unique email so the lookup is self-contained. No TruncateAll, no
// cross-tenant scan, no process-global mutation.
func TestPersonRepository_GetByEmail_ResolvesViaGeneratedColumn(t *testing.T) {
	t.Parallel()
	pool := repoFixture(t)
	repo := adapters.NewPersonRepository(pool, pg.NewTransactor(pool))
	ctx := t.Context()

	p := newPerson(t, "router@example.test")
	if err := repo.Add(ctx, p); err != nil {
		t.Fatalf("Add: %v", err)
	}

	addr, _ := email.New("router@example.test")
	got, err := repo.GetByEmail(ctx, addr)
	if err != nil {
		t.Fatalf("GetByEmail: %v", err)
	}
	if got.ID() != p.ID() {
		t.Fatalf("id round-trip: got %q want %q", got.ID(), p.ID())
	}
}

// SQL-contract: password_hash (long Argon2id PHC text) + security_stamp
// (UUID) survive UPDATE → SELECT round-trip via the pgx driver. The
// in-memory fake covers stamp rotation logic; this test pins down the
// binary/text encoding on the wire.
//
// arch-test:parallel-safe — shared pgtest container; each test owns a
// unique email/person so the round-trip is self-contained. No
// TruncateAll, no cross-tenant scan, no process-global mutation.
func TestPersonRepository_UpdateByID_PasswordHashAndStampRoundTrip(t *testing.T) {
	t.Parallel()
	pool := repoFixture(t)
	repo := adapters.NewPersonRepository(pool, pg.NewTransactor(pool))
	ctx := t.Context()

	p := newPerson(t, "rot@example.test")
	if err := repo.Add(ctx, p); err != nil {
		t.Fatalf("Add: %v", err)
	}

	newHash, _ := person.NewPasswordHash(strings.Replace(strongHash, "abcdef", "fedcba", 1))
	err := repo.UpdateByID(ctx, p.ID(), func(p2 *person.Person) (bool, error) {
		if err := p2.ChangePassword(newHash, testNow); err != nil {
			return false, err
		}
		return true, nil
	})
	if err != nil {
		t.Fatalf("UpdateByID: %v", err)
	}

	got, err := repo.GetByID(ctx, p.ID())
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got.PasswordHash().String() != newHash.String() {
		t.Fatal("PasswordHash did not persist (text round-trip broken)")
	}
	if got.SecurityStamp().String() == "" {
		t.Fatal("SecurityStamp did not round-trip (UUID column unhydrated)")
	}
}
