//go:build integration

// arch-test:no-timeout-needed — shares the package's pgtest container; execution
//   is bounded by the pgxpool conn timeouts and the package-level test timeout.
//
// arch-test:parallel-safe — every test seeds its own person / tenant / family.
//
// arch-test:raw-sql-justified — the revoke test reads the xmin system column of
//   the token rows (a row that was rewritten gets a new xmin), and the eviction
//   test reads the stored stamp through a second connection.

package adapters_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/leadkart/leadkart-go/internal/common/ids"
	"github.com/leadkart/leadkart-go/internal/common/pg"
	"github.com/leadkart/leadkart-go/internal/identity/adapters"
	"github.com/leadkart/leadkart-go/internal/identity/domain/person"
	"github.com/leadkart/leadkart-go/internal/identity/domain/refreshtoken"
)

// Item 9: revoking a family persists the family state and writes NO token row. A
// family with many token rows (a long-lived session) is revoked once per password
// change, under the person lock, for every family of the person: rewriting every
// row every time was the cost.
func TestRefreshTokenFamilyRepository_Revoke_WritesNoTokenRow(t *testing.T) {
	t.Parallel()
	pool := repoFixture(t)
	tx := pg.NewTransactor(pool)
	families := adapters.NewRefreshTokenFamilyRepository(pool, tx)
	persons := adapters.NewPersonRepository(pool, tx)
	tenants := adapters.NewTenantRepository(pool, tx)

	f := seedFamily(t, persons, tenants, families, "revoke-token-0")
	prev := "revoke-token-0"
	for i := 1; i <= 6; i++ {
		next := "revoke-token-" + string(rune('0'+i))
		if err := families.UpdateByID(t.Context(), f.ID(), func(loaded *refreshtoken.Family) (bool, error) {
			return true, loaded.Rotate(hashOf(t, prev), hashOf(t, next), tokenTTL, time.Now().UTC())
		}); err != nil {
			t.Fatalf("rotate %d: %v", i, err)
		}
		prev = next
	}

	xmins := func() map[string]string {
		rows, err := pool.Query(t.Context(), `SELECT id::text, xmin::text FROM identity.refresh_tokens WHERE family_id = $1`, f.ID().String())
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		out := map[string]string{}
		for rows.Next() {
			var id, x string
			if err := rows.Scan(&id, &x); err != nil {
				t.Fatal(err)
			}
			out[id] = x
		}
		return out
	}
	before := xmins()
	if len(before) != 7 {
		t.Fatalf("setup: %d token rows, want 7", len(before))
	}

	if err := families.UpdateByID(t.Context(), f.ID(), func(loaded *refreshtoken.Family) (bool, error) {
		return true, loaded.Revoke("password_changed", time.Now().UTC())
	}); err != nil {
		t.Fatalf("revoke: %v", err)
	}

	after := xmins()
	for id, x := range before {
		if after[id] != x {
			t.Fatalf("token row %s was rewritten by the revoke (xmin %s -> %s)", id, x, after[id])
		}
	}
	got, err := families.GetByID(t.Context(), f.ID())
	if err != nil {
		t.Fatal(err)
	}
	if !got.IsRevoked() || got.RevokeReason() != "password_changed" {
		t.Fatalf("family not revoked: revoked=%v reason=%q", got.IsRevoked(), got.RevokeReason())
	}
}

// evictorSpy records, for every eviction, the security stamp a SECOND connection
// reads at that moment: a committed rotation shows the new stamp, a not-yet-committed
// one still shows the old.
type evictorSpy struct {
	mu    sync.Mutex
	read  func(ctx context.Context) (string, error)
	calls []string
	errs  []error
}

func (e *evictorSpy) Invalidate(ctx context.Context, _ person.ID) error {
	stamp, err := e.read(ctx)
	e.mu.Lock()
	defer e.mu.Unlock()
	e.calls = append(e.calls, stamp)
	e.errs = append(e.errs, err)
	return nil
}

func (e *evictorSpy) snapshot() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.calls...)
}

// A committed write that rotates the stamp evicts the cached one AFTER the commit
// (inside a unit of work too), never when the transaction rolls back, and never for a
// write that leaves the stamp alone.
func TestPersonRepository_StampRotation_EvictsAfterTheCommit(t *testing.T) {
	t.Parallel()
	pool := repoFixture(t)
	tx := pg.NewTransactor(pool)
	persons := adapters.NewPersonRepository(pool, tx)
	p := seedPerson(t, persons, "evict-"+ids.NewV7().String()+"@example.test")
	spy := &evictorSpy{read: func(ctx context.Context) (string, error) {
		var s string
		err := pool.QueryRow(ctx, `SELECT security_stamp::text FROM identity.persons WHERE id = $1`, p.ID().String()).Scan(&s)
		return s, err
	}}
	persons.WithStampEvictor(spy)
	storedStamp := func() string {
		s, err := spy.read(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	original := storedStamp()

	// A write that does not touch the stamp evicts nothing.
	if err := persons.UpdateByID(t.Context(), p.ID(), func(loaded *person.Person) (bool, error) { return true, nil }); err != nil {
		t.Fatal(err)
	}
	if n := len(spy.snapshot()); n != 0 {
		t.Fatalf("%d evictions for a write that left the stamp alone", n)
	}

	// A rotation inside a unit of work that rolls back evicts nothing.
	boom := errors.New("rollback")
	err := tx.WithinTx(t.Context(), pg.TxScopePlatform, func(ctx context.Context) error {
		if err := persons.UpdateByID(ctx, p.ID(), func(loaded *person.Person) (bool, error) {
			return true, loaded.RotateSecurityStamp("test", time.Now().UTC())
		}); err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("rollback: %v", err)
	}
	if n := len(spy.snapshot()); n != 0 || storedStamp() != original {
		t.Fatalf("a rolled-back rotation evicted %d times or changed the stamp", n)
	}

	// A rotation inside a unit of work evicts once, after the commit.
	if err := tx.WithinTx(t.Context(), pg.TxScopePlatform, func(ctx context.Context) error {
		if err := persons.UpdateByID(ctx, p.ID(), func(loaded *person.Person) (bool, error) {
			return true, loaded.RotateSecurityStamp("test", time.Now().UTC())
		}); err != nil {
			return err
		}
		if n := len(spy.snapshot()); n != 0 {
			t.Errorf("evicted %d times BEFORE the commit", n)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	calls := spy.snapshot()
	if len(calls) != 1 || calls[0] == original {
		t.Fatalf("evictions = %v, want exactly one that already saw the committed stamp (original %s)", calls, original)
	}

	// And outside a unit of work (the adapter's own transaction).
	if err := persons.UpdateByID(t.Context(), p.ID(), func(loaded *person.Person) (bool, error) {
		return true, loaded.RotateSecurityStamp("test", time.Now().UTC())
	}); err != nil {
		t.Fatal(err)
	}
	calls = spy.snapshot()
	if len(calls) != 2 || calls[1] == calls[0] {
		t.Fatalf("evictions = %v, want a second one that saw the newer stamp", calls)
	}
}
