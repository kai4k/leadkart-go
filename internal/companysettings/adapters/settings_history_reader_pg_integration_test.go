//go:build integration

// SQL-CONTRACT COVERAGE for this file (ADR 0062): the policy, history and GST rate
// queries against real Postgres under RLS, see the header below.
//
// arch-test:no-timeout-needed — shared pgtest container + pgxpool conn
//   timeouts + package-level `task ci:test:int -timeout=15m` bound execution.
//
// arch-test:parallel-safe — every test seeds a FRESH tenant; the history table
// is RLS-scoped.

package adapters_test

import (
	"errors"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/leadkart/leadkart-go/internal/common/ids"
	"github.com/leadkart/leadkart-go/internal/common/pagination"
	"github.com/leadkart/leadkart-go/internal/common/pg"
	"github.com/leadkart/leadkart-go/internal/companysettings/adapters"
	"github.com/leadkart/leadkart-go/internal/companysettings/domain/commercesettings"
	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
)

func TestSettingsHistoryReader_NewestFirstWithActorReasonAndKeys(t *testing.T) {
	t.Parallel()
	p := pool(t)
	repo := newRepo(t)
	reader := adapters.NewSettingsHistoryReader(p, pg.NewTransactor(p))
	tid := seedTenant(t, p)
	actor := commercesettings.Actor{MembershipID: membership.ID(ids.NewV7().String()), ImpersonatorOperatorID: "op-7"}
	seed(t, repo, tid)
	err := repo.UpdateByID(t.Context(), tid, func(s *commercesettings.Settings) (bool, error) {
		res, err := s.Change(commercesettings.ChangeInput{
			ExpectedVersion: 1, Actor: actor, MayManage: true, MayManageTax: true, Reason: "per CA", Now: fixedNow,
			Changes: jsonPatch(t, map[string]any{"expiry.stop_months_before": 2, "invoice.round_off": "nearest_rupee"}),
		})
		return res.Changed, err
	})
	if err != nil {
		t.Fatal(err)
	}

	page, err := reader.ListHistory(t.Context(), tid, pagination.Cursor{}, 10)
	if err != nil {
		t.Fatalf("ListHistory: %v", err)
	}
	if len(page.Items) != 2 || page.Items[0].Version != 2 || page.Items[1].Version != 1 || page.HasMore {
		t.Fatalf("items = %+v", page.Items)
	}
	top := page.Items[0]
	if diff := cmp.Diff([]string{"expiry.stop_months_before", "invoice.round_off"}, top.ChangedKeys); diff != "" {
		t.Fatalf("changed keys: %s", diff)
	}
	if top.Reason != "per CA" || top.ActorMembershipID != actor.MembershipID.String() || top.ImpersonatorOperatorID != "op-7" ||
		top.Policy.Expiry.StopMonthsBefore != 2 || top.Policy.Invoice.RoundOff != "nearest_rupee" {
		t.Fatalf("top = %+v", top)
	}
	if first := page.Items[1]; first.Reason != commercesettings.ReasonInitial || first.ActorMembershipID != "" || len(first.ChangedKeys) != 0 {
		t.Fatalf("the seeded version is the system's: %+v", first)
	}

	// Keyset: page size 1 walks newest to oldest.
	one, err := reader.ListHistory(t.Context(), tid, pagination.Cursor{}, 1)
	if err != nil || len(one.Items) != 1 || !one.HasMore {
		t.Fatalf("page of one: %+v %v", one, err)
	}
	cur, err := pagination.Decode(one.NextCursor)
	if err != nil {
		t.Fatal(err)
	}
	rest, err := reader.ListHistory(t.Context(), tid, cur, 1)
	if err != nil || len(rest.Items) != 1 || rest.Items[0].Version != 1 || rest.HasMore {
		t.Fatalf("second page: %+v %v", rest, err)
	}
	if _, err := reader.ListHistory(t.Context(), tid, pagination.Cursor{ID: "abc"}, 1); !errors.Is(err, pagination.ErrInvalidCursor) {
		t.Fatalf("a cursor that is not a version: %v", err)
	}

	// Another tenant reads none of it.
	other := seedTenant(t, p)
	none, err := reader.ListHistory(t.Context(), other, pagination.Cursor{}, 10)
	if err != nil || len(none.Items) != 0 {
		t.Fatalf("another tenant's history: %+v %v", none, err)
	}
}
