package command_test

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/leadkart/leadkart-go/internal/common/commercepolicy"
	"github.com/leadkart/leadkart-go/internal/companysettings/app/command"
	"github.com/leadkart/leadkart-go/internal/companysettings/domain/commercesettings"
	"github.com/leadkart/leadkart-go/internal/companysettings/domain/commercesettings/commercesettingstest"
	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)

var (
	now      = func() time.Time { return time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC) }
	tenantID = tenant.ID(uuid.NewString())
	member   = membership.ID(uuid.NewString())
)

func patch(t *testing.T, kv map[string]any) commercepolicy.Changes {
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

func TestChangeSettingsHandler_ReturnsTheUpdatedPolicyAndVersion(t *testing.T) {
	t.Parallel()
	repo := commercesettingstest.NewFakeRepository()
	h := command.NewChangeSettingsHandler(repo, now)
	res, err := h.Handle(t.Context(), command.ChangeSettingsCommand{
		TenantID: tenantID, ExpectedVersion: 0, ActorMembershipID: member, MayManage: true,
		Changes: patch(t, map[string]any{"expiry.stop_months_before": 4}),
	})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if !res.Changed || res.Version != 1 || res.Policy.Expiry.StopMonthsBefore != 4 || res.UpdatedBy != member {
		t.Fatalf("result = %+v", res)
	}
	if len(repo.DrainedEvents) != 1 {
		t.Fatalf("events = %d, want exactly one policy_changed", len(repo.DrainedEvents))
	}
}

func TestChangeSettingsHandler_StaleVersionIsAConflict(t *testing.T) {
	t.Parallel()
	repo := commercesettingstest.NewFakeRepository()
	h := command.NewChangeSettingsHandler(repo, now)
	cmd := command.ChangeSettingsCommand{TenantID: tenantID, ActorMembershipID: member, MayManage: true, Changes: patch(t, map[string]any{"credit.enabled": true})}
	if _, err := h.Handle(t.Context(), cmd); err != nil {
		t.Fatal(err)
	}
	cmd.Changes = patch(t, map[string]any{"credit.enabled": false}) // ExpectedVersion still 0
	_, err := h.Handle(t.Context(), cmd)
	if !errors.Is(err, commercesettings.ErrVersionConflict) {
		t.Fatalf("err = %v, want ErrVersionConflict", err)
	}
	if len(repo.DrainedEvents) != 1 {
		t.Fatal("a stale save must emit nothing")
	}
}

func TestChangeSettingsHandler_NoOpEmitsNothing(t *testing.T) {
	t.Parallel()
	repo := commercesettingstest.NewFakeRepository()
	h := command.NewChangeSettingsHandler(repo, now)
	res, err := h.Handle(t.Context(), command.ChangeSettingsCommand{
		TenantID: tenantID, MayManage: true, Changes: patch(t, map[string]any{"expiry.stop_months_before": 1}),
	})
	if err != nil || res.Changed || res.Version != 0 || len(repo.DrainedEvents) != 0 {
		t.Fatalf("no-op: %+v %v events=%d", res, err, len(repo.DrainedEvents))
	}
}

func TestChangeSettingsHandler_RequiresATenant(t *testing.T) {
	t.Parallel()
	h := command.NewChangeSettingsHandler(commercesettingstest.NewFakeRepository(), now)
	if _, err := h.Handle(t.Context(), command.ChangeSettingsCommand{}); err == nil {
		t.Fatal("a missing tenant must be refused")
	}
}

func TestGSTRateHandlers_AddThenDeactivate(t *testing.T) {
	t.Parallel()
	repo := commercesettingstest.NewFakeRepository()
	add := command.NewAddGSTRateHandler(repo, now, uuid.NewString)
	upd := command.NewUpdateGSTRateHandler(repo, now)

	added, err := add.Handle(t.Context(), command.AddGSTRateCommand{
		TenantID: tenantID, RateBps: 1200, Label: "GST 12%", ConfirmedBy: "A. Mehta (CA)", ConfirmedOn: "2026-10-30",
		Reason: "per CA", ActorMembershipID: member,
	})
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	if added.Rate.ID == "" || !added.Rate.Active || added.PolicyVersion != 1 {
		t.Fatalf("added = %+v", added)
	}
	off := false
	got, err := upd.Handle(t.Context(), command.UpdateGSTRateCommand{TenantID: tenantID, RateID: added.Rate.ID, Active: &off, Reason: "retired", ActorMembershipID: member})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if got.Rate.Active || got.PolicyVersion != 2 {
		t.Fatalf("updated = %+v", got)
	}
	same, err := upd.Handle(t.Context(), command.UpdateGSTRateCommand{TenantID: tenantID, RateID: added.Rate.ID, Active: &off, Reason: "again"})
	if err != nil || same.PolicyVersion != 2 || len(repo.DrainedEvents) != 2 {
		t.Fatalf("a no-op edit must not move the version: %+v %v events=%d", same, err, len(repo.DrainedEvents))
	}
	if _, err := upd.Handle(t.Context(), command.UpdateGSTRateCommand{TenantID: tenantID, RateID: uuid.NewString(), Active: &off, Reason: "x"}); !errors.Is(err, commercesettings.ErrGSTRateNotFound) {
		t.Fatalf("unknown rate: %v", err)
	}
}

func TestSeedSettingsHandler_SeedsOnceAndReplayIsHarmless(t *testing.T) {
	t.Parallel()
	repo := commercesettingstest.NewFakeRepository()
	h := command.NewSeedSettingsHandler(repo, now)
	first, err := h.Handle(t.Context(), command.SeedSettingsCommand{TenantID: tenantID})
	if err != nil || first.AlreadyExisted {
		t.Fatalf("first seed: %+v %v", first, err)
	}
	replay, err := h.Handle(t.Context(), command.SeedSettingsCommand{TenantID: tenantID})
	if err != nil || !replay.AlreadyExisted {
		t.Fatalf("replay: %+v %v", replay, err)
	}
	if len(repo.DrainedEvents) != 1 || repo.Store[tenantID].Version != 1 {
		t.Fatalf("replay must change nothing: events=%d version=%d", len(repo.DrainedEvents), repo.Store[tenantID].Version)
	}
}

func TestRepublishHandler_ReannouncesTheCurrentVersionOnly(t *testing.T) {
	t.Parallel()
	repo := commercesettingstest.NewFakeRepository()
	h := command.NewRepublishHandler(repo, now)
	if err := h.Handle(t.Context(), command.RepublishCommand{TenantID: tenantID}); err != nil {
		t.Fatal(err)
	}
	if len(repo.DrainedEvents) != 0 {
		t.Fatal("a tenant with nothing stored has nothing to republish")
	}
	if _, err := command.NewSeedSettingsHandler(repo, now).Handle(t.Context(), command.SeedSettingsCommand{TenantID: tenantID}); err != nil {
		t.Fatal(err)
	}
	if err := h.Handle(t.Context(), command.RepublishCommand{TenantID: tenantID}); err != nil {
		t.Fatal(err)
	}
	if len(repo.DrainedEvents) != 2 || repo.Store[tenantID].Version != 1 {
		t.Fatalf("events=%d version=%d, want 2 events and version still 1", len(repo.DrainedEvents), repo.Store[tenantID].Version)
	}
	if _, ok := repo.DrainedEvents[1].(commercesettings.PolicyRepublishedEvent); !ok {
		t.Fatalf("second event is %T", repo.DrainedEvents[1])
	}
}
