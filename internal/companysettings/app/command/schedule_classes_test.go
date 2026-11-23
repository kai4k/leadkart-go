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

// fakeUsage answers the schedule usage questions from fixed counts.
type fakeUsage struct {
	scheduled int64
	byClass   map[string]int64
}

func (f fakeUsage) ProductsWithSchedule(_ context.Context, _ tenant.ID) (int64, error) {
	return f.scheduled, nil
}

func (f fakeUsage) ProductsWithClass(_ context.Context, _ tenant.ID, classID string) (int64, error) {
	return f.byClass[classID], nil
}

func TestScheduleClasses_DefaultsThenAddRenameRemove(t *testing.T) {
	t.Parallel()
	repo := commercesettingstest.NewFakeRepository()
	add := command.NewAddScheduleClassHandler(repo, now, uuid.NewString)
	rename := command.NewRenameScheduleClassHandler(repo, now)
	remove := command.NewRemoveScheduleClassHandler(repo, fakeUsage{}, now)

	added, err := add.Handle(t.Context(), command.AddScheduleClassCommand{TenantID: tenantID, Label: "Controlled", Reason: "local rule", ActorMembershipID: member})
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	if added.Class.ID == "" || added.Class.Label != "Controlled" || added.PolicyVersion != 1 {
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

	removed, err := remove.Handle(t.Context(), command.RemoveScheduleClassCommand{TenantID: tenantID, ClassID: added.Class.ID, Reason: "unused"})
	if err != nil || removed.PolicyVersion != 3 || len(removed.Items) != len(defaults) {
		t.Fatalf("remove: %+v %v", removed, err)
	}
	if len(repo.DrainedEvents) != 3 {
		t.Fatalf("events = %d, want one per change", len(repo.DrainedEvents))
	}
}

func TestScheduleClasses_AClassInUseCannotBeRemoved(t *testing.T) {
	t.Parallel()
	repo := commercesettingstest.NewFakeRepository()
	inUse := commercepolicy.Default().DrugSchedule.Classes[0].ID
	remove := command.NewRemoveScheduleClassHandler(repo, fakeUsage{byClass: map[string]int64{inUse: 2}}, now)

	_, err := remove.Handle(t.Context(), command.RemoveScheduleClassCommand{TenantID: tenantID, ClassID: inUse, Reason: "cleanup"})
	if !errors.Is(err, commercesettings.ErrScheduleClassInUse) {
		t.Fatalf("err = %v, want ErrScheduleClassInUse", err)
	}
	if len(repo.DrainedEvents) != 0 {
		t.Fatal("a refused removal must change nothing")
	}
	if _, err := remove.Handle(t.Context(), command.RemoveScheduleClassCommand{TenantID: tenantID, ClassID: uuid.NewString(), Reason: "x"}); !errors.Is(err, commercesettings.ErrScheduleClassNotFound) {
		t.Fatalf("unknown class: %v", err)
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
