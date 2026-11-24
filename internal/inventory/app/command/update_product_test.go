package command_test

import (
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/leadkart/leadkart-go/internal/common/commercepolicy"
	"github.com/leadkart/leadkart-go/internal/inventory/app/command"
	"github.com/leadkart/leadkart-go/internal/inventory/domain/product"
)

func TestUpdateProductHandler_HappyPath_AppliesPartial(t *testing.T) {
	t.Parallel()
	repo := newFakeProductRepo()
	tid := newTenantID(t)
	actor := newMembershipID(t)
	p := seedProduct(t, repo, tid, actor, "UPD-1")
	h := newUpdate(repo, commercepolicy.DrugScheduleOptional)

	newName := "Renamed"
	err := h.Handle(t.Context(), command.UpdateProductCommand{
		TenantID: tid, ProductID: p.ID(), ActorMembershipID: actor,
		Name: &newName,
	})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	got, _ := repo.GetByID(t.Context(), tid, p.ID())
	if got.Name() != newName {
		t.Fatalf("name: got %q want %q", got.Name(), newName)
	}
}

// Missing product → ErrNotFound (404 at HTTP).
func TestUpdateProductHandler_MissingProduct_ReturnsErrNotFound(t *testing.T) {
	t.Parallel()
	repo := newFakeProductRepo()
	h := newUpdate(repo, commercepolicy.DrugScheduleOptional)
	tid := newTenantID(t)
	actor := newMembershipID(t)

	newName := "x"
	err := h.Handle(t.Context(), command.UpdateProductCommand{
		TenantID: tid, ProductID: product.ID("nonexistent"), ActorMembershipID: actor,
		Name: &newName,
	})
	if !errors.Is(err, product.ErrNotFound) {
		t.Fatalf("err: got %v want ErrNotFound", err)
	}
}

// An inactive product can still be edited (it is never deleted, ADR 0073).
func TestUpdateProductHandler_InactiveProductCanStillBeEdited(t *testing.T) {
	t.Parallel()
	repo := newFakeProductRepo()
	tid := newTenantID(t)
	actor := newMembershipID(t)
	p := seedProduct(t, repo, tid, actor, "INACT-1")
	if err := p.Deactivate(actor, fixedNow); err != nil {
		t.Fatalf("Deactivate: %v", err)
	}
	_ = p.PullEvents()
	h := newUpdate(repo, commercepolicy.DrugScheduleOptional)

	newName := "Renamed while inactive"
	if err := h.Handle(t.Context(), command.UpdateProductCommand{
		TenantID: tid, ProductID: p.ID(), ActorMembershipID: actor, Name: &newName,
	}); err != nil {
		t.Fatalf("update of an inactive product: %v", err)
	}
	if p.Name() != newName || p.IsActive() {
		t.Fatalf("name %q active %v, want renamed and still inactive", p.Name(), p.IsActive())
	}
}

func newUpdate(repo product.Repository, mode string) command.UpdateProductHandler {
	pol, _ := policyLists(mode)
	return command.NewUpdateProductHandler(repo, pol, func() time.Time { return fixedNow })
}

// A product moves to another entry of the company rate list; an explicit 0% entry
// is applied as 0. A deactivated or unknown entry is refused and nothing changes.
func TestUpdateProductHandler_GSTMovesToAnotherListEntry(t *testing.T) {
	t.Parallel()
	repo := newFakeProductRepo()
	tid, actor := newTenantID(t), newMembershipID(t)
	p := seedProduct(t, repo, tid, actor, "GST-0")
	pol, l := policyLists(commercepolicy.DrugScheduleOptional)
	h := command.NewUpdateProductHandler(repo, pol, func() time.Time { return fixedNow })

	for _, tc := range []struct {
		id   string
		want []string
	}{{l.retired, []string{command.CodeInactive}}, {uuid.NewString(), []string{command.CodeNotInList}}, {"", []string{command.CodeRequired}}} {
		err := h.Handle(t.Context(), command.UpdateProductCommand{TenantID: tid, ProductID: p.ID(), ActorMembershipID: actor, GSTRateID: &tc.id})
		if got := fieldCodes(t, err, "gst_rate_id"); !slices.Equal(got, tc.want) {
			t.Fatalf("rate %q: codes %v, want %v", tc.id, got, tc.want)
		}
	}
	nilID := l.nilRated
	if err := h.Handle(t.Context(), command.UpdateProductCommand{TenantID: tid, ProductID: p.ID(), ActorMembershipID: actor, GSTRateID: &nilID}); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	got, _ := repo.GetByID(t.Context(), tid, p.ID())
	if got.GSTRateBps() != 0 || got.GSTRateID() != l.nilRated {
		t.Fatalf("rate %d id %s after choosing the nil entry", got.GSTRateBps(), got.GSTRateID())
	}
}

// The schedule is set, changed and cleared through the company list, and the company
// mode decides what is allowed. Clearing is always allowed except under required.
func TestUpdateProductHandler_DrugScheduleFollowsTheCompanyModeAndList(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{commercepolicy.DrugScheduleOptional, commercepolicy.DrugScheduleOff, commercepolicy.DrugScheduleRequired} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			repo := newFakeProductRepo()
			tid, actor := newTenantID(t), newMembershipID(t)
			p := seedProduct(t, repo, tid, actor, "SCH-1")
			pol, l := policyLists(mode)
			h := command.NewUpdateProductHandler(repo, pol, func() time.Time { return fixedNow })
			set := func(id string) error {
				return h.Handle(t.Context(), command.UpdateProductCommand{TenantID: tid, ProductID: p.ID(), ActorMembershipID: actor, DrugSchedule: &id})
			}

			err := set(l.classA)
			if mode == commercepolicy.DrugScheduleOff {
				if got := fieldCodes(t, err, "drug_schedule"); !slices.Equal(got, []string{command.CodeScheduleIsOff}) {
					t.Fatalf("a class under mode off: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("set a class: %v", err)
			}
			if got, _ := repo.GetByID(t.Context(), tid, p.ID()); got.DrugScheduleClassID() != l.classA {
				t.Fatalf("stored %q", got.DrugScheduleClassID())
			}
			if got := fieldCodes(t, set(uuid.NewString()), "drug_schedule"); !slices.Equal(got, []string{command.CodeNotInList}) {
				t.Fatalf("a class outside the list: %v", got)
			}
			err = set("")
			if mode == commercepolicy.DrugScheduleRequired {
				if got := fieldCodes(t, err, "drug_schedule"); !slices.Equal(got, []string{command.CodeRequired}) {
					t.Fatalf("clearing under required: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("clearing: %v", err)
			}
			if got, _ := repo.GetByID(t.Context(), tid, p.ID()); got.DrugScheduleClassID() != "" {
				t.Fatalf("the class was not cleared: %q", got.DrugScheduleClassID())
			}
		})
	}
}

// Under mode off a product that already carries a class may still be edited and the
// class cleared: the refusal is on SETTING one.
func TestUpdateProductHandler_ClearingAClassIsAllowedUnderModeOff(t *testing.T) {
	t.Parallel()
	repo := newFakeProductRepo()
	tid, actor := newTenantID(t), newMembershipID(t)
	p := seedProduct(t, repo, tid, actor, "SCH-OFF")
	pol, l := policyLists(commercepolicy.DrugScheduleOff)
	h := command.NewUpdateProductHandler(repo, pol, func() time.Time { return fixedNow })
	if err := p.Update(actor, product.UpdateSpec{DrugScheduleClassID: &l.classA}, fixedNow); err != nil {
		t.Fatal(err)
	}
	empty := ""
	if err := h.Handle(t.Context(), command.UpdateProductCommand{TenantID: tid, ProductID: p.ID(), ActorMembershipID: actor, DrugSchedule: &empty}); err != nil {
		t.Fatalf("clearing under off: %v", err)
	}
}

// Category, reorder level and expiry-alert threshold are part of the update.
func TestUpdateProductHandler_AppliesFormFields(t *testing.T) {
	t.Parallel()
	repo := newFakeProductRepo()
	tid := newTenantID(t)
	actor := newMembershipID(t)
	p := seedProduct(t, repo, tid, actor, "UPD-FORM")
	h := newUpdate(repo, commercepolicy.DrugScheduleOptional)

	category := "Diabetic"
	err := h.Handle(t.Context(), command.UpdateProductCommand{
		TenantID: tid, ProductID: p.ID(), ActorMembershipID: actor,
		ProductCategory: &category, ReorderLevel: new(7), ExpiryAlertThresholdDays: new(45),
	})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	got, _ := repo.GetByID(t.Context(), tid, p.ID())
	if got.ProductCategory() != "Diabetic" || got.ReorderLevel() != 7 || got.ExpiryAlertThresholdDays() != 45 {
		t.Fatalf("stored category %q reorder %d threshold %d; want Diabetic, 7, 45",
			got.ProductCategory(), got.ReorderLevel(), got.ExpiryAlertThresholdDays())
	}

	// A value the aggregate refuses changes nothing.
	err = h.Handle(t.Context(), command.UpdateProductCommand{
		TenantID: tid, ProductID: p.ID(), ActorMembershipID: actor, ReorderLevel: new(-1),
	})
	if !errors.Is(err, product.ErrInvalid) {
		t.Fatalf("negative reorder level: err %v, want ErrInvalid", err)
	}
	got, _ = repo.GetByID(t.Context(), tid, p.ID())
	if got.ReorderLevel() != 7 {
		t.Fatalf("reorder level after a refused update = %d, want 7", got.ReorderLevel())
	}
}
