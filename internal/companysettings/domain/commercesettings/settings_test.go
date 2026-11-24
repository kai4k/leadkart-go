package commercesettings_test

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/uuid"

	"github.com/leadkart/leadkart-go/internal/common/commercepolicy"
	"github.com/leadkart/leadkart-go/internal/companysettings/domain/commercesettings"
	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)

var (
	t0       = time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC)
	tenantID = tenant.ID(uuid.NewString())
	actor    = commercesettings.Actor{MembershipID: membership.ID(uuid.NewString())}
)

func fresh(t *testing.T) *commercesettings.Settings {
	t.Helper()
	s, err := commercesettings.New(tenantID)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s
}

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

func TestNew_ReadsTheDefaultsAtVersionZeroThroughInitial(t *testing.T) {
	t.Parallel()
	s := fresh(t)
	if s.Version() != 0 {
		t.Fatalf("version = %d, want 0 (nothing stored yet)", s.Version())
	}
	if diff := cmp.Diff(commercepolicy.Initial().Policy, s.Policy()); diff != "" {
		t.Fatalf("a tenant with no row must read commercepolicy.Initial():\n%s", diff)
	}
	if len(s.PullEvents()) != 0 {
		t.Fatal("reading defaults is not a change")
	}
}

func TestSeed_FirstVersionOnce(t *testing.T) {
	t.Parallel()
	s := fresh(t)
	if err := s.Seed(t0); err != nil {
		t.Fatalf("Seed: %v", err)
	}
	if s.Version() != 1 {
		t.Fatalf("version = %d, want 1", s.Version())
	}
	evs := s.PullEvents()
	if len(evs) != 1 {
		t.Fatalf("events = %d, want 1", len(evs))
	}
	pc, ok := evs[0].(commercesettings.PolicyChangedEvent)
	if !ok || pc.Version != 1 || pc.Reason != commercesettings.ReasonInitial {
		t.Fatalf("seed event = %+v", evs[0])
	}
	if err := s.Seed(t0); !errors.Is(err, commercesettings.ErrAlreadySeeded) {
		t.Fatalf("second Seed = %v, want ErrAlreadySeeded (replay is harmless)", err)
	}
}

func TestChange_BumpsVersionRecordsKeysAndEmitsOneEvent(t *testing.T) {
	t.Parallel()
	s := fresh(t)
	res, err := s.Change(commercesettings.ChangeInput{
		ExpectedVersion: 0, Changes: patch(t, map[string]any{"expiry.stop_months_before": 2}),
		Actor: actor, MayManage: true, Now: t0,
	})
	if err != nil {
		t.Fatalf("Change: %v", err)
	}
	if !res.Changed || s.Version() != 1 || s.Policy().Expiry.StopMonthsBefore != 2 {
		t.Fatalf("result %+v version %d policy %+v", res, s.Version(), s.Policy().Expiry)
	}
	evs := s.PullEvents()
	if len(evs) != 1 {
		t.Fatalf("events = %d", len(evs))
	}
	ev := evs[0].(commercesettings.PolicyChangedEvent)
	if diff := cmp.Diff([]string{"expiry.stop_months_before"}, ev.ChangedKeys); diff != "" {
		t.Fatal(diff)
	}
	if ev.Actor != actor || ev.Version != 1 || ev.TenantID != tenantID {
		t.Fatalf("event = %+v", ev)
	}
}

func TestChange_StaleExpectedVersionIsAConflictAndChangesNothing(t *testing.T) {
	t.Parallel()
	s := fresh(t)
	if _, err := s.Change(commercesettings.ChangeInput{ExpectedVersion: 0, Changes: patch(t, map[string]any{"credit.enabled": true}), Actor: actor, MayManage: true, Now: t0}); err != nil {
		t.Fatal(err)
	}
	s.PullEvents()
	_, err := s.Change(commercesettings.ChangeInput{ExpectedVersion: 0, Changes: patch(t, map[string]any{"credit.enabled": false}), Actor: actor, MayManage: true, Now: t0})
	var vc *commercesettings.VersionConflictError
	if !errors.As(err, &vc) || vc.Current != 1 || vc.Expected != 0 {
		t.Fatalf("err = %v, want a VersionConflictError{Current:1, Expected:0}", err)
	}
	if !errors.Is(err, commercesettings.ErrVersionConflict) {
		t.Fatal("must match ErrVersionConflict")
	}
	if !s.Policy().Credit.Enabled || s.Version() != 1 || len(s.PullEvents()) != 0 {
		t.Fatal("a stale save must change nothing")
	}
}

func TestChange_MatchingPatchIsANoOp(t *testing.T) {
	t.Parallel()
	s := fresh(t)
	res, err := s.Change(commercesettings.ChangeInput{ExpectedVersion: 0, Changes: patch(t, map[string]any{"expiry.stop_months_before": 1}), Actor: actor, MayManage: true, Now: t0})
	if err != nil {
		t.Fatal(err)
	}
	if res.Changed || s.Version() != 0 || len(s.PullEvents()) != 0 {
		t.Fatalf("a patch equal to the current policy must not bump the version: %+v v%d", res, s.Version())
	}
}

func TestChange_TaxKeysNeedThePermissionAndAReason(t *testing.T) {
	t.Parallel()
	tax := patch(t, map[string]any{"invoice.round_off": "nearest_rupee"})

	s := fresh(t)
	_, err := s.Change(commercesettings.ChangeInput{ExpectedVersion: 0, Changes: tax, Actor: actor, MayManage: true, Reason: "per CA", Now: t0})
	var denied *commercesettings.TaxManageRequiredError
	if !errors.As(err, &denied) || len(denied.Keys) != 1 || denied.Keys[0] != "invoice.round_off" {
		t.Fatalf("without the tax permission: %v", err)
	}

	_, err = s.Change(commercesettings.ChangeInput{ExpectedVersion: 0, Changes: tax, Actor: actor, MayManage: true, MayManageTax: true, Now: t0})
	fe := commercepolicy.Fields(err)
	if fe == nil || fe["reason"][0] != commercepolicy.CodeRequired {
		t.Fatalf("a tax change without a reason: %v", err)
	}

	if _, err := s.Change(commercesettings.ChangeInput{ExpectedVersion: 0, Changes: tax, Actor: actor, MayManage: true, MayManageTax: true, Reason: "per CA", Now: t0}); err != nil {
		t.Fatalf("tax change with permission and reason: %v", err)
	}
	if s.Policy().Invoice.RoundOff != "nearest_rupee" {
		t.Fatal("not applied")
	}
}

func TestChange_FieldErrorsAreStableCodesAndChangeNothing(t *testing.T) {
	t.Parallel()
	s := fresh(t)
	_, err := s.Change(commercesettings.ChangeInput{ExpectedVersion: 0, Changes: patch(t, map[string]any{"expiry.stop_months_before": -1}), Actor: actor, MayManage: true, Now: t0})
	fe := commercepolicy.Fields(err)
	if fe["expiry.stop_months_before"][0] != commercepolicy.CodeOutOfRange {
		t.Fatalf("err = %v", err)
	}
	if s.Version() != 0 {
		t.Fatal("refused patch must not bump")
	}
}

func TestChange_ReasonIsBounded(t *testing.T) {
	t.Parallel()
	s := fresh(t)
	long := make([]byte, commercesettings.MaxReasonChars+1)
	for i := range long {
		long[i] = 'a'
	}
	_, err := s.Change(commercesettings.ChangeInput{ExpectedVersion: 0, Changes: patch(t, map[string]any{"credit.enabled": true}), Actor: actor, MayManage: true, Reason: string(long), Now: t0})
	if fe := commercepolicy.Fields(err); fe["reason"][0] != commercepolicy.CodeTooLong {
		t.Fatalf("err = %v", err)
	}
	_, err = s.Change(commercesettings.ChangeInput{ExpectedVersion: 0, Changes: patch(t, map[string]any{"credit.enabled": true}), Actor: actor, MayManage: true, Reason: "bad\x00reason", Now: t0})
	if fe := commercepolicy.Fields(err); fe["reason"][0] != commercepolicy.CodeInvalidCharacters {
		t.Fatalf("err = %v", err)
	}
}

func TestAddGSTRate_ListStartsEmptyAndGrows(t *testing.T) {
	t.Parallel()
	s := fresh(t)
	if len(s.Policy().Tax.GSTRates) != 0 {
		t.Fatal("the company's list starts empty")
	}
	id := uuid.NewString()
	if err := s.AddGSTRate(commercesettings.AddGSTRateInput{
		ID: id, RateBps: 1200, Label: " GST 12% ", ConfirmedBy: "A. Mehta (CA)", ConfirmedOn: "2026-10-30",
		Reason: "per CA", Actor: actor, Now: t0,
	}); err != nil {
		t.Fatalf("AddGSTRate: %v", err)
	}
	rates := s.Policy().Tax.GSTRates
	if len(rates) != 1 || rates[0].ID != id || rates[0].Label != "GST 12%" || !rates[0].Active {
		t.Fatalf("rates = %+v", rates)
	}
	if s.Version() != 1 {
		t.Fatalf("a rate change is a policy change: version = %d", s.Version())
	}
	ev := s.PullEvents()[0].(commercesettings.PolicyChangedEvent)
	if diff := cmp.Diff([]string{"tax.gst_rates"}, ev.ChangedKeys); diff != "" || len(ev.Policy.Tax.GSTRates) != 1 {
		t.Fatalf("event must carry the whole policy with the rate: %+v %s", ev, diff)
	}
}

func TestAddGSTRate_RefusalsAreFieldCodes(t *testing.T) {
	t.Parallel()
	s := fresh(t)
	in := commercesettings.AddGSTRateInput{ID: uuid.NewString(), RateBps: 500, Label: "GST 5%", ConfirmedBy: "A", ConfirmedOn: "2026-10-30", Reason: "r", Actor: actor, Now: t0}
	if err := s.AddGSTRate(in); err != nil {
		t.Fatal(err)
	}
	dup := in
	dup.ID = uuid.NewString()
	dup.Label = "gst 5%"
	if fe := commercepolicy.Fields(s.AddGSTRate(dup)); fe["label"][0] != commercepolicy.CodeDuplicate {
		t.Fatalf("the same rate and label twice: %v", fe)
	}
	bad := in
	bad.ID = uuid.NewString()
	bad.RateBps = 20000
	bad.ConfirmedOn = "yesterday"
	fe := commercepolicy.Fields(s.AddGSTRate(bad))
	if fe["rate_bps"][0] != commercepolicy.CodeOutOfRange || fe["confirmed_on"][0] != commercepolicy.CodeInvalidFormat {
		t.Fatalf("fe = %v", fe)
	}
	noReason := in
	noReason.ID = uuid.NewString()
	noReason.RateBps = 1800
	noReason.Reason = ""
	if fe := commercepolicy.Fields(s.AddGSTRate(noReason)); fe["reason"][0] != commercepolicy.CodeRequired {
		t.Fatalf("a rate change is a tax change and needs a reason: %v", fe)
	}
}

func TestUpdateGSTRate_DeactivateNeverDelete(t *testing.T) {
	t.Parallel()
	s := fresh(t)
	id := uuid.NewString()
	if err := s.AddGSTRate(commercesettings.AddGSTRateInput{ID: id, RateBps: 500, Label: "GST 5%", ConfirmedBy: "A", ConfirmedOn: "2026-10-30", Reason: "r", Actor: actor, Now: t0}); err != nil {
		t.Fatal(err)
	}
	s.PullEvents()
	off := false
	if err := s.UpdateGSTRate(commercesettings.UpdateGSTRateInput{ID: id, Active: &off, Reason: "rate retired", Actor: actor, Now: t0}); err != nil {
		t.Fatalf("UpdateGSTRate: %v", err)
	}
	rates := s.Policy().Tax.GSTRates
	if len(rates) != 1 || rates[0].Active || s.Version() != 2 {
		t.Fatalf("rates %+v version %d", rates, s.Version())
	}
	if err := s.UpdateGSTRate(commercesettings.UpdateGSTRateInput{ID: uuid.NewString(), Active: &off, Reason: "r", Actor: actor, Now: t0}); !errors.Is(err, commercesettings.ErrGSTRateNotFound) {
		t.Fatalf("unknown rate: %v", err)
	}
	again := s.Version()
	if err := s.UpdateGSTRate(commercesettings.UpdateGSTRateInput{ID: id, Active: &off, Reason: "r", Actor: actor, Now: t0}); err != nil || s.Version() != again {
		t.Fatalf("deactivating an inactive rate is a no-op: %v v%d", err, s.Version())
	}
}

func TestRepublish_EmitsTheCurrentVersionWithoutChangingIt(t *testing.T) {
	t.Parallel()
	s := fresh(t)
	if err := s.Republish(t0); !errors.Is(err, commercesettings.ErrNothingToRepublish) {
		t.Fatalf("version 0 has nothing stored to republish: %v", err)
	}
	if err := s.Seed(t0); err != nil {
		t.Fatal(err)
	}
	s.PullEvents()
	if err := s.Republish(t0); err != nil {
		t.Fatal(err)
	}
	evs := s.PullEvents()
	if len(evs) != 1 || s.Version() != 1 {
		t.Fatalf("events %d version %d", len(evs), s.Version())
	}
	if _, ok := evs[0].(commercesettings.PolicyRepublishedEvent); !ok {
		t.Fatalf("event = %T", evs[0])
	}
}

func TestChange_NonTaxKeysNeedTheManagePermission(t *testing.T) {
	t.Parallel()
	s := fresh(t)
	// Holds only the tax permission: a non-tax key is refused, and it names the key.
	_, err := s.Change(commercesettings.ChangeInput{
		ExpectedVersion: 0, Changes: patch(t, map[string]any{"credit.enabled": true}),
		Actor: actor, MayManageTax: true, MayManage: false, Now: t0,
	})
	var denied *commercesettings.ManageRequiredError
	if !errors.As(err, &denied) || len(denied.Keys) != 1 || denied.Keys[0] != "credit.enabled" {
		t.Fatalf("a non-tax change without the manage permission: %v", err)
	}
	if s.Version() != 0 {
		t.Fatal("refused change must not bump the version")
	}
	// Holds manage only: a tax key is refused, a non-tax key passes.
	_, err = s.Change(commercesettings.ChangeInput{
		ExpectedVersion: 0, Changes: patch(t, map[string]any{"credit.enabled": true}),
		Actor: actor, MayManage: true, Now: t0,
	})
	if err != nil {
		t.Fatalf("manage-only holder changing a non-tax key: %v", err)
	}
}

func TestAddGSTRate_ConfirmationDateCannotBeInTheFuture(t *testing.T) {
	t.Parallel()
	s := fresh(t)
	in := commercesettings.AddGSTRateInput{ID: uuid.NewString(), RateBps: 500, Label: "GST 5%", ConfirmedBy: "A", Reason: "r", Actor: actor, Now: t0}
	in.ConfirmedOn = "2026-11-03" // two days after t0 (2026-11-01 10:00 UTC)
	if fe := commercepolicy.Fields(s.AddGSTRate(in)); fe["confirmed_on"][0] != commercesettings.CodeInFuture {
		t.Fatalf("fe = %v", fe)
	}
	in.ConfirmedOn = "2026-11-02" // tomorrow UTC: today in IST, allowed
	if err := s.AddGSTRate(in); err != nil {
		t.Fatalf("a date that is today somewhere ahead of UTC: %v", err)
	}
}

func TestAddGSTRate_ListIsCapped(t *testing.T) {
	t.Parallel()
	s := fresh(t)
	for i := range commercesettings.MaxGSTRates {
		err := s.AddGSTRate(commercesettings.AddGSTRateInput{
			ID: uuid.NewString(), RateBps: i, Label: "GST", ConfirmedBy: "A", ConfirmedOn: "2026-10-30", Reason: "r", Actor: actor, Now: t0,
		})
		if err != nil {
			t.Fatalf("rate %d: %v", i, err)
		}
	}
	err := s.AddGSTRate(commercesettings.AddGSTRateInput{
		ID: uuid.NewString(), RateBps: 9999, Label: "GST", ConfirmedBy: "A", ConfirmedOn: "2026-10-30", Reason: "r", Actor: actor, Now: t0,
	})
	if fe := commercepolicy.Fields(err); fe["rate_bps"][0] != commercepolicy.CodeTooMany {
		t.Fatalf("the %dst rate: %v", commercesettings.MaxGSTRates+1, err)
	}
}

// Owner ruling 2026-11-25 (ADR 0073 addendum): a deleted (inactive) schedule class frees its label.
func TestScheduleClassLabels_AreUniqueAmongActiveClassesOnly(t *testing.T) {
	t.Parallel()
	s := fresh(t)
	old := uuid.NewString()
	if err := s.AddScheduleClass(commercesettings.AddScheduleClassInput{ID: old, Label: "Controlled", Reason: "r", Actor: actor, Now: t0}); err != nil {
		t.Fatal(err)
	}
	if err := s.AddScheduleClass(commercesettings.AddScheduleClassInput{ID: uuid.NewString(), Label: "controlled", Reason: "r", Actor: actor, Now: t0}); err == nil {
		t.Fatal("an active duplicate label must be refused")
	}
	if err := s.DeactivateScheduleClass(commercesettings.SetScheduleClassActiveInput{ID: old, Reason: "r", Actor: actor, Now: t0}); err != nil {
		t.Fatal(err)
	}
	fresh2 := uuid.NewString()
	if err := s.AddScheduleClass(commercesettings.AddScheduleClassInput{ID: fresh2, Label: "Controlled", Reason: "r", Actor: actor, Now: t0}); err != nil {
		t.Fatalf("a deleted class frees its label: %v", err)
	}
	if err := s.ReactivateScheduleClass(commercesettings.SetScheduleClassActiveInput{ID: old, Reason: "r", Actor: actor, Now: t0}); !errors.Is(err, commercesettings.ErrScheduleClassNameTaken) {
		t.Fatalf("reactivate over a taken label: %v, want ErrScheduleClassNameTaken", err)
	}
	// A rename onto a deleted class's label is fine; onto an active one is not.
	third := uuid.NewString()
	if err := s.AddScheduleClass(commercesettings.AddScheduleClassInput{ID: third, Label: "Other", Reason: "r", Actor: actor, Now: t0}); err != nil {
		t.Fatal(err)
	}
	if err := s.RenameScheduleClass(commercesettings.RenameScheduleClassInput{ID: third, Label: "Controlled", Reason: "r", Actor: actor, Now: t0}); err == nil {
		t.Fatal("renaming onto an active label must be refused")
	}
	if err := s.DeactivateScheduleClass(commercesettings.SetScheduleClassActiveInput{ID: fresh2, Reason: "r", Actor: actor, Now: t0}); err != nil {
		t.Fatal(err)
	}
	if err := s.RenameScheduleClass(commercesettings.RenameScheduleClassInput{ID: third, Label: "Controlled", Reason: "r", Actor: actor, Now: t0}); err != nil {
		t.Fatalf("renaming onto a deleted class's label: %v", err)
	}
	if err := s.ReactivateScheduleClass(commercesettings.SetScheduleClassActiveInput{ID: old, Reason: "r", Actor: actor, Now: t0}); !errors.Is(err, commercesettings.ErrScheduleClassNameTaken) {
		t.Fatalf("still taken: %v", err)
	}
}
