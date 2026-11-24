package command_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/leadkart/leadkart-go/internal/common/commercepolicy"
	"github.com/leadkart/leadkart-go/internal/companysettings/app/command"
	"github.com/leadkart/leadkart-go/internal/companysettings/domain/commercesettings"
	"github.com/leadkart/leadkart-go/internal/companysettings/domain/commercesettings/commercesettingstest"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)

// fakeUsage answers the schedule usage question from a fixed count.
type fakeUsage struct {
	scheduled int64
}

func (f fakeUsage) ProductsWithSchedule(_ context.Context, _ tenant.ID) (int64, error) {
	return f.scheduled, nil
}

func TestScheduleClasses_DefaultsThenAddRenameDeactivateReactivate(t *testing.T) {
	t.Parallel()
	repo := commercesettingstest.NewFakeRepository()
	add := command.NewAddScheduleClassHandler(repo, now, uuid.NewString)
	rename := command.NewRenameScheduleClassHandler(repo, now)
	deactivate := command.NewDeactivateScheduleClassHandler(repo, now)
	reactivate := command.NewReactivateScheduleClassHandler(repo, now)

	added, err := add.Handle(t.Context(), command.AddScheduleClassCommand{TenantID: tenantID, Label: "Controlled", Reason: "local rule", ActorMembershipID: member})
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	if added.Class.ID == "" || added.Class.Label != "Controlled" || !added.Class.Active || added.PolicyVersion != 1 {
		t.Fatalf("added = %+v", added)
	}
	defaults := commercepolicy.Default().DrugSchedule.Classes
	if len(added.Items) != len(defaults)+1 {
		t.Fatalf("items = %d, want the defaults plus one", len(added.Items))
	}

	renamed, err := rename.Handle(t.Context(), command.RenameScheduleClassCommand{TenantID: tenantID, ClassID: added.Class.ID, Label: "Controlled drug", Reason: "clearer"})
	if err != nil || renamed.Class.Label != "Controlled drug" || renamed.PolicyVersion != 2 {
		t.Fatalf("rename: %+v %v", renamed, err)
	}
	same, err := rename.Handle(t.Context(), command.RenameScheduleClassCommand{TenantID: tenantID, ClassID: added.Class.ID, Label: "Controlled drug", Reason: "again"})
	if err != nil || same.PolicyVersion != 2 {
		t.Fatalf("a no-op rename must not move the version: %+v %v", same, err)
	}

	// ADR 0073: a class is made inactive and back, never deleted. The list keeps it either way.
	st := command.ChangeScheduleClassStatusCommand{TenantID: tenantID, ClassID: added.Class.ID, Reason: "retired", ActorMembershipID: member}
	off, err := deactivate.Handle(t.Context(), st)
	if err != nil || off.Class.Active || off.PolicyVersion != 3 || len(off.Items) != len(defaults)+1 {
		t.Fatalf("deactivate: %+v %v", off, err)
	}
	again, err := deactivate.Handle(t.Context(), st)
	if err != nil || again.PolicyVersion != 3 {
		t.Fatalf("deactivating an inactive class must not move the version: %+v %v", again, err)
	}
	on, err := reactivate.Handle(t.Context(), st)
	if err != nil || !on.Class.Active || on.PolicyVersion != 4 {
		t.Fatalf("reactivate: %+v %v", on, err)
	}
	if again, err = reactivate.Handle(t.Context(), st); err != nil || again.PolicyVersion != 4 {
		t.Fatalf("reactivating an active class must not move the version: %+v %v", again, err)
	}
	if len(repo.DrainedEvents) != 4 {
		t.Fatalf("events = %d, want one per real change", len(repo.DrainedEvents))
	}
}

func TestScheduleClasses_DeactivateRefusals(t *testing.T) {
	t.Parallel()
	repo := commercesettingstest.NewFakeRepository()
	deactivate := command.NewDeactivateScheduleClassHandler(repo, now)
	first := commercepolicy.Default().DrugSchedule.Classes[0].ID

	if _, err := deactivate.Handle(t.Context(), command.ChangeScheduleClassStatusCommand{TenantID: tenantID, ClassID: uuid.NewString(), Reason: "x"}); !errors.Is(err, commercesettings.ErrScheduleClassNotFound) {
		t.Fatalf("unknown class: %v", err)
	}
	_, err := deactivate.Handle(t.Context(), command.ChangeScheduleClassStatusCommand{TenantID: tenantID, ClassID: first})
	if got := commercepolicy.Fields(err)["reason"]; len(got) != 1 || got[0] != commercepolicy.CodeRequired {
		t.Fatalf("missing reason: %v", err)
	}
	if len(repo.DrainedEvents) != 0 {
		t.Fatal("a refused change must store nothing")
	}
}

func TestScheduleClasses_RefusalsAreFieldCodes(t *testing.T) {
	t.Parallel()
	repo := commercesettingstest.NewFakeRepository()
	add := command.NewAddScheduleClassHandler(repo, now, uuid.NewString)
	dup := commercepolicy.Default().DrugSchedule.Classes[0].Label

	_, err := add.Handle(t.Context(), command.AddScheduleClassCommand{TenantID: tenantID, Label: " " + dup + " ", Reason: "x"})
	if got := commercepolicy.Fields(err)["label"]; len(got) != 1 || got[0] != commercepolicy.CodeDuplicate {
		t.Fatalf("duplicate label: %v", err)
	}
	_, err = add.Handle(t.Context(), command.AddScheduleClassCommand{TenantID: tenantID, Label: "New", Reason: ""})
	if got := commercepolicy.Fields(err)["reason"]; len(got) != 1 || got[0] != commercepolicy.CodeRequired {
		t.Fatalf("missing reason: %v", err)
	}
	_, err = add.Handle(t.Context(), command.AddScheduleClassCommand{TenantID: tenantID, Label: "  ", Reason: "x"})
	if len(commercepolicy.Fields(err)["label"]) == 0 {
		t.Fatalf("blank label: %v", err)
	}
}

func TestChangeSettings_TheScheduleModeCannotGoOffWhileAProductCarriesAClass(t *testing.T) {
	t.Parallel()
	repo := commercesettingstest.NewFakeRepository()
	off := patch(t, map[string]any{"drug_schedule.mode": commercepolicy.DrugScheduleOff})

	h := command.NewChangeSettingsHandler(repo, fakeUsage{scheduled: 3}, now)
	_, err := h.Handle(t.Context(), command.ChangeSettingsCommand{
		TenantID: tenantID, ExpectedVersion: 0, ActorMembershipID: member, MayManageTax: true, Reason: "retire", Changes: off,
	})
	if got := commercepolicy.Fields(err)["drug_schedule.mode"]; len(got) != 1 || got[0] != commercesettings.CodeScheduleInUse {
		t.Fatalf("err = %v, want drug_schedule.mode: [schedule_in_use]", err)
	}
	if len(repo.DrainedEvents) != 0 {
		t.Fatal("a refused change must store nothing")
	}

	free := command.NewChangeSettingsHandler(repo, fakeUsage{}, now)
	res, err := free.Handle(t.Context(), command.ChangeSettingsCommand{
		TenantID: tenantID, ExpectedVersion: 0, ActorMembershipID: member, MayManageTax: true, Reason: "retire", Changes: off,
	})
	if err != nil || res.Policy.DrugSchedule.Mode != commercepolicy.DrugScheduleOff {
		t.Fatalf("with no scheduled product the mode goes off: %+v %v", res, err)
	}
}

func TestChangeSettings_TheClassListAndTheOffsetAreRegistrySettingsOrManagedElsewhere(t *testing.T) {
	t.Parallel()
	repo := commercesettingstest.NewFakeRepository()
	h := command.NewChangeSettingsHandler(repo, fakeUsage{}, now)
	_, err := h.Handle(t.Context(), command.ChangeSettingsCommand{
		TenantID: tenantID, ExpectedVersion: 0, MayManageTax: true, Reason: "x",
		Changes: patch(t, map[string]any{"drug_schedule.classes": []any{}}),
	})
	if got := commercepolicy.Fields(err)["drug_schedule.classes"]; len(got) != 1 || got[0] != commercepolicy.CodeManagedElsewhere {
		t.Fatalf("the class list changes through its own routes: %v", err)
	}
	// The business-day offset is market data: no company can patch it.
	_, err = h.Handle(t.Context(), command.ChangeSettingsCommand{
		TenantID: tenantID, ExpectedVersion: 0, MayManage: true,
		Changes: patch(t, map[string]any{"expiry.business_day_utc_offset_minutes": -720}),
	})
	if got := commercepolicy.Fields(err)["expiry.business_day_utc_offset_minutes"]; len(got) != 1 || got[0] != commercepolicy.CodeUnknownSetting {
		t.Fatalf("the offset patch: %v", err)
	}
}
