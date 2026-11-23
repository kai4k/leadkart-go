//go:build integration

// arch-test:no-timeout-needed — shared pgtest container + pgxpool conn
//   timeouts + package-level `task ci:test:int -timeout=15m` bound execution.
//
// arch-test:parallel-safe — every test seeds a FRESH tenant and every table is
// RLS-scoped, so tests are isolated by tenant.
//
// SQL-contract coverage (ADR 0070): defaults for a tenant with no row, seed +
// history + outbox in one transaction, the stale-save refusal under a row lock,
// the GST list round trip, and tenant isolation by RLS.

package adapters_test

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/leadkart/leadkart-go/internal/common/commercepolicy"
	"github.com/leadkart/leadkart-go/internal/common/ids"
	"github.com/leadkart/leadkart-go/internal/common/messaging/messagingtest"
	"github.com/leadkart/leadkart-go/internal/common/pg"
	"github.com/leadkart/leadkart-go/internal/common/pgconv"
	"github.com/leadkart/leadkart-go/internal/companysettings/adapters"
	"github.com/leadkart/leadkart-go/internal/companysettings/adapters/db"
	"github.com/leadkart/leadkart-go/internal/companysettings/domain/commercesettings"
	"github.com/leadkart/leadkart-go/internal/companysettings/integrationevents"
	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)

func newRepo(t *testing.T) *adapters.SettingsRepository {
	t.Helper()
	p := pool(t)
	return adapters.NewSettingsRepository(p, pg.NewTransactor(p))
}

func jsonPatch(t *testing.T, kv map[string]any) commercepolicy.Changes {
	t.Helper()
	out := commercepolicy.Changes{}
	for k, v := range kv {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		out[k] = b
	}
	return out
}

func seed(t *testing.T, repo *adapters.SettingsRepository, tid tenant.ID) {
	t.Helper()
	err := repo.UpdateByID(t.Context(), tid, func(s *commercesettings.Settings) (bool, error) {
		return true, s.Seed(fixedNow)
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
}

func TestSettingsRepository_NoRowReadsTheDefaultsAtVersionZero(t *testing.T) {
	t.Parallel()
	repo := newRepo(t)
	tid := seedTenant(t, pool(t))
	s, err := repo.GetByID(t.Context(), tid)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if s.Version() != 0 {
		t.Fatalf("version = %d, want 0", s.Version())
	}
	if diff := cmp.Diff(commercepolicy.Initial().Policy, s.Policy()); diff != "" {
		t.Fatalf("a tenant with no row must read commercepolicy.Initial():\n%s", diff)
	}
}

func TestSettingsRepository_SeedStoresVersionOneAndOneOutboxEvent(t *testing.T) {
	t.Parallel()
	p := pool(t)
	repo := newRepo(t)
	tid := seedTenant(t, p)
	seed(t, repo, tid)

	s, err := repo.GetByID(t.Context(), tid)
	if err != nil || s.Version() != 1 {
		t.Fatalf("after seed: %v v%d", err, s.Version())
	}

	var mine int
	for _, row := range messagingtest.RowsForTopic(messagingtest.DrainOutbox(t.Context(), t, p), integrationevents.Topic) {
		if row.Message.Metadata["tenant_id"] == tid.String() {
			mine++
		}
	}
	if mine != 1 {
		t.Fatalf("outbox policy events for the tenant = %d, want 1 (written in the same tx)", mine)
	}
}

func TestSettingsRepository_ChangePersistsAndStaleSaveConflicts(t *testing.T) {
	t.Parallel()
	repo := newRepo(t)
	tid := seedTenant(t, pool(t))
	actor := commercesettings.Actor{MembershipID: membership.ID(ids.NewV7().String())}
	seed(t, repo, tid)

	change := func(expected int64, v int) error {
		return repo.UpdateByID(t.Context(), tid, func(s *commercesettings.Settings) (bool, error) {
			res, err := s.Change(commercesettings.ChangeInput{
				ExpectedVersion: expected, Actor: actor, Now: fixedNow,
				Changes: jsonPatch(t, map[string]any{"expiry.stop_months_before": v}),
			})
			return res.Changed, err
		})
	}
	if err := change(1, 3); err != nil {
		t.Fatalf("change: %v", err)
	}
	s, _ := repo.GetByID(t.Context(), tid)
	if s.Version() != 2 || s.Policy().Expiry.StopMonthsBefore != 3 || s.UpdatedBy() != actor.MembershipID {
		t.Fatalf("v%d policy %+v by %q", s.Version(), s.Policy().Expiry, s.UpdatedBy())
	}
	err := change(1, 5)
	if !errors.Is(err, commercesettings.ErrVersionConflict) {
		t.Fatalf("stale save = %v, want ErrVersionConflict", err)
	}
	s, _ = repo.GetByID(t.Context(), tid)
	if s.Policy().Expiry.StopMonthsBefore != 3 || s.Version() != 2 {
		t.Fatal("a stale save must change nothing")
	}
}

func TestSettingsRepository_ConcurrentSavesOnTheSameVersionOneWins(t *testing.T) {
	t.Parallel()
	repo := newRepo(t)
	tid := seedTenant(t, pool(t))
	actor := commercesettings.Actor{MembershipID: membership.ID(ids.NewV7().String())}

	const n = 8
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = repo.UpdateByID(t.Context(), tid, func(s *commercesettings.Settings) (bool, error) {
				res, err := s.Change(commercesettings.ChangeInput{
					ExpectedVersion: 0, Actor: actor, Now: fixedNow,
					Changes: jsonPatch(t, map[string]any{"expiry.stop_months_before": 2 + i}),
				})
				return res.Changed, err
			})
		}()
	}
	wg.Wait()
	won, conflicted := 0, 0
	for _, err := range errs {
		switch {
		case err == nil:
			won++
		case errors.Is(err, commercesettings.ErrVersionConflict):
			conflicted++
		default:
			t.Fatalf("unexpected error: %v", err)
		}
	}
	if won != 1 || conflicted != n-1 {
		t.Fatalf("won=%d conflicted=%d, want 1 and %d", won, conflicted, n-1)
	}
}

func TestSettingsRepository_GSTRatesRoundTrip(t *testing.T) {
	t.Parallel()
	repo := newRepo(t)
	tid := seedTenant(t, pool(t))
	actor := commercesettings.Actor{MembershipID: membership.ID(ids.NewV7().String())}
	id := ids.NewV7().String()
	err := repo.UpdateByID(t.Context(), tid, func(s *commercesettings.Settings) (bool, error) {
		return true, s.AddGSTRate(commercesettings.AddGSTRateInput{
			ID: id, RateBps: 500, Label: "GST 5%", ConfirmedBy: "A. Mehta (CA)", ConfirmedOn: "2026-10-30",
			Reason: "per CA", Actor: actor, Now: fixedNow,
		})
	})
	if err != nil {
		t.Fatalf("add rate: %v", err)
	}
	off := false
	err = repo.UpdateByID(t.Context(), tid, func(s *commercesettings.Settings) (bool, error) {
		return true, s.UpdateGSTRate(commercesettings.UpdateGSTRateInput{ID: id, Active: &off, Reason: "retired", Actor: actor, Now: fixedNow})
	})
	if err != nil {
		t.Fatalf("deactivate: %v", err)
	}
	s, err := repo.GetByID(t.Context(), tid)
	if err != nil {
		t.Fatal(err)
	}
	want := []commercepolicy.GSTRate{{ID: id, RateBps: 500, Label: "GST 5%", ConfirmedBy: "A. Mehta (CA)", ConfirmedOn: "2026-10-30", Active: false}}
	if diff := cmp.Diff(want, s.Policy().Tax.GSTRates); diff != "" {
		t.Fatalf("rates (-want +got):\n%s", diff)
	}
	if s.Version() != 2 {
		t.Fatalf("two rate changes from a no-row tenant: version = %d, want 2", s.Version())
	}
}

func TestSettingsRepository_RowLevelSecurityIsolatesTenants(t *testing.T) {
	t.Parallel()
	p := pool(t)
	repo := newRepo(t)
	a, b := seedTenant(t, p), seedTenant(t, p)
	seed(t, repo, a)

	// Tenant B's session cannot see tenant A's rows, whatever the WHERE says.
	err := pg.NewTransactor(p).WithinTxPgxTenant(t.Context(), b.String(), func(ctx context.Context, tx pgx.Tx) error {
		q := db.New(p).WithTx(tx)
		aid := pgconv.PgUUID(uuid.MustParse(a.String()))
		if _, err := q.GetCommercePolicy(ctx, aid); !errors.Is(err, pgx.ErrNoRows) {
			t.Errorf("tenant B read tenant A's policy: %v", err)
		}
		rates, err := q.ListGSTRates(ctx, aid)
		if err != nil || len(rates) != 0 {
			t.Errorf("tenant B read tenant A's rates: %d rows, %v", len(rates), err)
		}
		// A write aimed at tenant A's row matches nothing.
		werr := q.UpdateCommercePolicy(ctx, db.UpdateCommercePolicyParams{
			TenantID: aid, Version: 9, Policy: []byte(`{}`), UpdatedAt: pgconv.PgRequiredTimestamp(fixedNow),
		})
		if werr != nil {
			t.Errorf("update across tenants errored instead of matching nothing: %v", werr)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	s, _ := repo.GetByID(t.Context(), a)
	if s.Version() != 1 {
		t.Fatalf("tenant A's policy was touched by tenant B: version %d", s.Version())
	}
}
