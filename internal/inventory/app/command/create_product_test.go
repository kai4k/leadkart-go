package command_test

import (
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/leadkart/leadkart-go/internal/common/commercepolicy"
	"github.com/leadkart/leadkart-go/internal/inventory/app/command"
	"github.com/leadkart/leadkart-go/internal/inventory/app/sellability/sellabilitytest"
	"github.com/leadkart/leadkart-go/internal/inventory/domain/product"
)

// The company lists the product handlers read: a 12% rate, a nil rate (0%), a
// deactivated 5% rate, and two drug schedule classes under the given mode.
type lists struct {
	twelve, nilRated, retired string
	classA, classB            string
	// classOff is a class of the list that was made inactive (ADR 0073).
	classOff string
}

func policyLists(mode string) (sellabilitytest.Policy, lists) {
	l := lists{twelve: uuid.NewString(), nilRated: uuid.NewString(), retired: uuid.NewString(), classA: uuid.NewString(), classB: uuid.NewString(), classOff: uuid.NewString()}
	p := commercepolicy.Default()
	p.Tax.GSTRates = []commercepolicy.GSTRate{
		{ID: l.twelve, RateBps: 1200, Label: "GST 12%", ConfirmedBy: "CA", ConfirmedOn: "2026-10-30", Active: true},
		{ID: l.nilRated, RateBps: 0, Label: "Nil", ConfirmedBy: "CA", ConfirmedOn: "2026-10-30", Active: true},
		{ID: l.retired, RateBps: 500, Label: "GST 5%", ConfirmedBy: "CA", ConfirmedOn: "2026-10-30", Active: false},
	}
	p.DrugSchedule = commercepolicy.DrugSchedule{Mode: mode, Classes: []commercepolicy.ScheduleClass{
		{ID: l.classA, Label: "Class A", Active: true}, {ID: l.classB, Label: "Class B", Active: true},
		{ID: l.classOff, Label: "Class Off", Active: false},
	}}
	return sellabilitytest.Policy{Value: commercepolicy.Versioned{Version: 3, Policy: p}}, l
}

func newCreate(repo product.Repository, mode string) (command.CreateProductHandler, lists) {
	pol, l := policyLists(mode)
	return command.NewCreateProductHandler(repo, pol, func() time.Time { return fixedNow }, testNewProductID), l
}

func baseCreate(t *testing.T, sku, rateID string) command.CreateProductCommand {
	t.Helper()
	return command.CreateProductCommand{
		TenantID: newTenantID(t), ActorMembershipID: newMembershipID(t),
		SKU: sku, Name: "Amoxicillin 500 mg", DosageForm: "Capsule", PackSize: "10x10", HSNCode: "30049099",
		GSTRateID: rateID, Manufacturer: "Acme",
	}
}

func fieldCodes(t *testing.T, err error, field string) []string {
	t.Helper()
	var r *command.FieldRefusal
	if !errors.As(err, &r) {
		t.Fatalf("err = %v, want a field refusal", err)
	}
	return r.Fields[field]
}

func TestCreateProductHandler_HappyPath_TakesTheRateOfTheChosenListEntry(t *testing.T) {
	t.Parallel()
	repo := newFakeProductRepo()
	h, l := newCreate(repo, commercepolicy.DrugScheduleOptional)
	cmd := baseCreate(t, "AMOX-500", l.twelve)
	cmd.DefaultSaleRatePaise, cmd.GenericName, cmd.Composition, cmd.StorageCondition = 12500, " amoxicillin ", "Amoxicillin 500 mg", "Below 25 C"

	out, err := h.Handle(t.Context(), cmd)
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	p, _ := repo.GetByID(t.Context(), cmd.TenantID, out.ProductID)
	if p.GSTRateBps() != 1200 || p.GSTRateID() != l.twelve || p.DefaultSaleRatePaise() != 12500 || p.GenericName() != "amoxicillin" || p.StorageCondition() != "Below 25 C" {
		t.Fatalf("stored rate %d id %s default %d generic %q storage %q", p.GSTRateBps(), p.GSTRateID(), p.DefaultSaleRatePaise(), p.GenericName(), p.StorageCondition())
	}
	if p.DrugScheduleClassID() != "" {
		t.Fatal("a product carries no schedule unless one is chosen (nullable)")
	}
}

// There is no default rate: a missing, unknown or deactivated entry is refused by
// field, and nothing is written. A nil-rated entry (0%) is a rate like any other.
func TestCreateProductHandler_GSTComesOnlyFromTheCompanyList(t *testing.T) {
	t.Parallel()
	repo := newFakeProductRepo()
	h, l := newCreate(repo, commercepolicy.DrugScheduleOptional)

	cmd := baseCreate(t, "G-1", "")
	cmd.ProductCategory = "Nutraceuticals" // the old category fallback must not apply
	if _, err := h.Handle(t.Context(), cmd); !slices.Equal(fieldCodes(t, err, "gst_rate_id"), []string{command.CodeRequired}) {
		t.Fatalf("no rate: %v", err)
	}
	if _, err := h.Handle(t.Context(), baseCreate(t, "G-2", uuid.NewString())); !slices.Equal(fieldCodes(t, err, "gst_rate_id"), []string{command.CodeNotInList}) {
		t.Fatalf("a rate that is not in the list: %v", err)
	}
	if _, err := h.Handle(t.Context(), baseCreate(t, "G-3", l.retired)); !slices.Equal(fieldCodes(t, err, "gst_rate_id"), []string{command.CodeInactive}) {
		t.Fatalf("a deactivated rate: %v", err)
	}
	if repo.AddCalls != 0 {
		t.Fatalf("a refused product was written: %d", repo.AddCalls)
	}
	nilCmd := baseCreate(t, "G-4", l.nilRated)
	out, err := h.Handle(t.Context(), nilCmd)
	if err != nil {
		t.Fatalf("a nil rate: %v", err)
	}
	if p, _ := repo.GetByID(t.Context(), nilCmd.TenantID, out.ProductID); p.GSTRateBps() != 0 {
		t.Fatalf("the nil-rated entry stores 0%%, got %d", p.GSTRateBps())
	}
}

// The schedule is open and company-defined: any class of the company own list, nullable
// except where the company chose required; nothing is tied to a particular class.
func TestCreateProductHandler_DrugScheduleFollowsTheCompanyModeAndList(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, mode string
		schedule   *string
		want       []string // codes on drug_schedule; nil = accepted
	}{
		{"optional, none", commercepolicy.DrugScheduleOptional, nil, nil},
		{"optional, a class of the list", commercepolicy.DrugScheduleOptional, ptr("A"), nil},
		{"optional, a class not in the list", commercepolicy.DrugScheduleOptional, ptr("x"), []string{command.CodeNotInList}},
		{"off, none", commercepolicy.DrugScheduleOff, nil, nil},
		{"off, a class", commercepolicy.DrugScheduleOff, ptr("A"), []string{command.CodeScheduleIsOff}},
		{"optional, an inactive class", commercepolicy.DrugScheduleOptional, ptr("off"), []string{command.CodeInactive}},
		{"required, none", commercepolicy.DrugScheduleRequired, nil, []string{command.CodeRequired}},
		{"required, blank", commercepolicy.DrugScheduleRequired, ptr(""), []string{command.CodeRequired}},
		{"required, a class", commercepolicy.DrugScheduleRequired, ptr("B"), nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			repo := newFakeProductRepo()
			h, l := newCreate(repo, tc.mode)
			cmd := baseCreate(t, "S-1", l.twelve)
			if tc.schedule != nil {
				cmd.DrugScheduleGiven = true
				switch *tc.schedule {
				case "A":
					cmd.DrugSchedule = l.classA
				case "B":
					cmd.DrugSchedule = l.classB
				case "off":
					cmd.DrugSchedule = l.classOff
				case "x":
					cmd.DrugSchedule = uuid.NewString()
				}
			}
			out, err := h.Handle(t.Context(), cmd)
			if tc.want == nil {
				if err != nil {
					t.Fatalf("Handle: %v", err)
				}
				p, _ := repo.GetByID(t.Context(), cmd.TenantID, out.ProductID)
				if p.DrugScheduleClassID() != cmd.DrugSchedule {
					t.Fatalf("stored class %q, want %q", p.DrugScheduleClassID(), cmd.DrugSchedule)
				}
				return
			}
			if got := fieldCodes(t, err, "drug_schedule"); !slices.Equal(got, tc.want) {
				t.Fatalf("codes %v, want %v", got, tc.want)
			}
			if repo.AddCalls != 0 {
				t.Fatal("a refused product was written")
			}
		})
	}
}

func ptr(s string) *string { return &s }

// Empty SKU → ErrInvalid.
func TestCreateProductHandler_RejectsInvalidSpec(t *testing.T) {
	t.Parallel()
	repo := newFakeProductRepo()
	h, l := newCreate(repo, commercepolicy.DrugScheduleOptional)

	_, err := h.Handle(t.Context(), baseCreate(t, "", l.twelve))
	if !errors.Is(err, product.ErrInvalid) {
		t.Fatalf("err: got %v want ErrInvalid", err)
	}
	if repo.AddCalls != 0 {
		t.Fatalf("AddCalls on invalid spec: got %d want 0 (Add MUST NOT be called)", repo.AddCalls)
	}
}

// Duplicate SKU → ErrSKUTaken.
func TestCreateProductHandler_DuplicateSKU_ReturnsErrSKUTaken(t *testing.T) {
	t.Parallel()
	repo := newFakeProductRepo()
	h, l := newCreate(repo, commercepolicy.DrugScheduleOptional)
	cmd := baseCreate(t, "DUP-1", l.twelve)
	_ = seedProduct(t, repo, cmd.TenantID, cmd.ActorMembershipID, "DUP-1") // arch-test:ignore-err — seed helper, error reported via t.Fatalf inside helper

	if _, err := h.Handle(t.Context(), cmd); !errors.Is(err, product.ErrSKUTaken) {
		t.Fatalf("err: got %v want ErrSKUTaken", err)
	}
}

// A SKU never frees up: an INACTIVE product's SKU is told apart (ADR 0073 addendum).
func TestCreateProductHandler_SKUOfAnInactiveProduct_ReturnsErrSKUTakenByInactive(t *testing.T) {
	t.Parallel()
	repo := newFakeProductRepo()
	h, l := newCreate(repo, commercepolicy.DrugScheduleOptional)
	cmd := baseCreate(t, "OLD-1", l.twelve)
	old := seedProduct(t, repo, cmd.TenantID, cmd.ActorMembershipID, "OLD-1")
	if err := old.Deactivate(cmd.ActorMembershipID, fixedNow); err != nil {
		t.Fatal(err)
	}

	_, err := h.Handle(t.Context(), cmd)
	if !errors.Is(err, product.ErrSKUTakenByInactive) || errors.Is(err, product.ErrSKUTaken) {
		t.Fatalf("err: got %v want ErrSKUTakenByInactive (and not ErrSKUTaken)", err)
	}
}

// Repo Add error propagates wrapped (caller can branch on errors.Is).
func TestCreateProductHandler_RepoAddError_Propagates(t *testing.T) {
	t.Parallel()
	repo := newFakeProductRepo()
	repo.AddErr = errSentinel
	h, l := newCreate(repo, commercepolicy.DrugScheduleOptional)

	if _, err := h.Handle(t.Context(), baseCreate(t, "X-1", l.twelve)); !errors.Is(err, errSentinel) {
		t.Fatalf("err: got %v want errSentinel propagated", err)
	}
}

// The form's fields reach the aggregate. An absent expiry-alert threshold takes the
// BRD default (90 days); an explicit 0 switches the alert off and is kept.
func TestCreateProductHandler_FormFields(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name          string
		threshold     *int
		wantThreshold int
	}{
		{"absent threshold takes the default", nil, product.ExpiryAlertThresholdDaysDefault},
		{"explicit zero stays zero", new(0), 0},
		{"explicit threshold is kept", new(30), 30},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			repo := newFakeProductRepo()
			h, l := newCreate(repo, commercepolicy.DrugScheduleOptional)
			cmd := baseCreate(t, "FORM-1", l.twelve)
			cmd.ProductCategory, cmd.ReorderLevel, cmd.ExpiryAlertThresholdDays = "Cardiac", 40, tc.threshold

			out, err := h.Handle(t.Context(), cmd)
			if err != nil {
				t.Fatalf("Handle: %v", err)
			}
			p, err := repo.GetByID(t.Context(), cmd.TenantID, out.ProductID)
			if err != nil {
				t.Fatalf("GetByID: %v", err)
			}
			if p.ProductCategory() != "Cardiac" || p.ReorderLevel() != 40 || p.ExpiryAlertThresholdDays() != tc.wantThreshold {
				t.Fatalf("stored category %q reorder %d threshold %d; want Cardiac, 40, %d",
					p.ProductCategory(), p.ReorderLevel(), p.ExpiryAlertThresholdDays(), tc.wantThreshold)
			}
		})
	}
}
