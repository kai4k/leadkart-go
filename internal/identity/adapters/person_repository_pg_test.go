//go:build integration

package adapters_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/leadkart/leadkart-go/internal/common/email"
	"github.com/leadkart/leadkart-go/internal/common/ids"
	"github.com/leadkart/leadkart-go/internal/identity/adapters"
	"github.com/leadkart/leadkart-go/internal/identity/domain/person"
	"github.com/leadkart/leadkart-go/internal/common/pg"
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

func TestPersonRepository_Add_PersistsRowAndOutboxEvent(t *testing.T) {
	pool := repoFixture(t)
	repo := adapters.NewPersonRepository(pool, pg.NewTransactor(pool))
	ctx := t.Context()

	p := newPerson(t, "alice@example.test")
	if err := repo.Add(ctx, p); err != nil {
		t.Fatalf("Add: %v", err)
	}

	got, err := repo.GetByID(ctx, p.ID())
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got.Email() != p.Email() {
		t.Fatalf("email round-trip: got %q want %q", got.Email(), p.Email())
	}
	if got.IsActive() != true {
		t.Fatal("expected IsActive=true on new Person")
	}

	rawDB, err := openRawDB(t, pool)
	if err != nil {
		t.Fatalf("openRawDB: %v", err)
	}
	defer rawDB.Close()

	var topic string
	// Person events are written under the platform-tenant sentinel
	// (uuid.Nil); under platform scope the SELECT sees them.
	if _, err := rawDB.ExecContext(ctx, `SELECT set_config('app.is_platform','true',false)`); err != nil {
		t.Fatalf("set platform: %v", err)
	}
	if err := rawDB.QueryRowContext(ctx, `
		SELECT topic FROM identity.outbox WHERE topic = 'identity.person_created.v1'
	`).Scan(&topic); err != nil {
		t.Fatalf("read outbox: %v", err)
	}
	if topic != "identity.person_created.v1" {
		t.Fatalf("topic: got %q want identity.person_created.v1", topic)
	}
}

func TestPersonRepository_Add_DuplicateEmail_ReturnsErrEmailTaken(t *testing.T) {
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

func TestPersonRepository_GetByEmail_ResolvesGlobally(t *testing.T) {
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

func TestPersonRepository_UpdateByID_ChangePasswordRotatesStamp(t *testing.T) {
	pool := repoFixture(t)
	repo := adapters.NewPersonRepository(pool, pg.NewTransactor(pool))
	ctx := t.Context()

	p := newPerson(t, "rot@example.test")
	if err := repo.Add(ctx, p); err != nil {
		t.Fatalf("Add: %v", err)
	}
	stampBefore := p.SecurityStamp().String()

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
	if got.SecurityStamp().String() == stampBefore {
		t.Fatal("SecurityStamp did not rotate after ChangePassword")
	}
	if got.PasswordHash().String() != newHash.String() {
		t.Fatal("PasswordHash did not persist")
	}
}

func TestPersonRepository_UpdateByID_AnonymiseScrubs(t *testing.T) {
	pool := repoFixture(t)
	repo := adapters.NewPersonRepository(pool, pg.NewTransactor(pool))
	ctx := t.Context()

	p := newPerson(t, "scrub@example.test")
	if err := repo.Add(ctx, p); err != nil {
		t.Fatalf("Add: %v", err)
	}

	err := repo.UpdateByID(ctx, p.ID(), func(p2 *person.Person) (bool, error) {
		if err := p2.Anonymise(testNow); err != nil {
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
	if !got.IsAnonymised() {
		t.Fatal("IsAnonymised not set")
	}
	if got.IsActive() {
		t.Fatal("IsActive still true after Anonymise")
	}
	if got.FirstName() != "anonymised" || got.LastName() != "anonymised" {
		t.Fatalf("PII not scrubbed: %q %q", got.FirstName(), got.LastName())
	}
	if got.AnonymisedAt().IsZero() {
		t.Fatal("AnonymisedAt not set")
	}
}

func TestPersonRepository_GetByID_NotFound(t *testing.T) {
	pool := repoFixture(t)
	repo := adapters.NewPersonRepository(pool, pg.NewTransactor(pool))
	ctx := t.Context()

	missing := person.ID(ids.NewV7().String())
	_, err := repo.GetByID(ctx, missing)
	if !errors.Is(err, person.ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}
