package command_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/leadkart/leadkart-go/internal/inventory/app/command"
	"github.com/leadkart/leadkart-go/internal/inventory/domain/product"
)

func TestCreateProductHandler_HappyPath_Persists(t *testing.T) {
	t.Parallel()
	repo := newFakeProductRepo()
	h := command.NewCreateProductHandler(repo, nil, func() time.Time { return fixedNow }, testNewProductID)
	tid := newTenantID(t)
	actor := newMembershipID(t)

	out, err := h.Handle(t.Context(), command.CreateProductCommand{
		TenantID: tid, ActorMembershipID: actor,
		SKU: "AMOX-500", Name: "Amoxicillin 500 mg",
		DosageForm: "Capsule", PackSize: "10x10",
		HSNCode: "30049099", GSTRateBps: new(1200),
		Manufacturer: "Acme",
	})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if out.ProductID.IsZero() {
		t.Fatal("ProductID should be set on success")
	}
	if repo.AddCalls != 1 {
		t.Fatalf("AddCalls: got %d want 1", repo.AddCalls)
	}
}

// Empty SKU → ErrInvalid.
func TestCreateProductHandler_RejectsInvalidSpec(t *testing.T) {
	t.Parallel()
	repo := newFakeProductRepo()
	h := command.NewCreateProductHandler(repo, nil, func() time.Time { return fixedNow }, testNewProductID)
	tid := newTenantID(t)
	actor := newMembershipID(t)

	_, err := h.Handle(t.Context(), command.CreateProductCommand{
		TenantID: tid, ActorMembershipID: actor,
		SKU: "", Name: "x", // empty SKU → ErrInvalid
		DosageForm: "Tablet", PackSize: "10",
		HSNCode: "3004", GSTRateBps: new(1200),
	})
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
	h := command.NewCreateProductHandler(repo, nil, func() time.Time { return fixedNow }, testNewProductID)
	tid := newTenantID(t)
	actor := newMembershipID(t)
	_ = seedProduct(t, repo, tid, actor, "DUP-1") // arch-test:ignore-err — seed helper, error reported via t.Fatalf inside helper

	_, err := h.Handle(t.Context(), command.CreateProductCommand{
		TenantID: tid, ActorMembershipID: actor,
		SKU: "DUP-1", Name: "Second",
		DosageForm: "Tablet", PackSize: "10",
		HSNCode: "3004", GSTRateBps: new(1200),
	})
	if !errors.Is(err, product.ErrSKUTaken) {
		t.Fatalf("err: got %v want ErrSKUTaken", err)
	}
}

// Repo Add error propagates wrapped (caller can branch on errors.Is).
func TestCreateProductHandler_RepoAddError_Propagates(t *testing.T) {
	t.Parallel()
	repo := newFakeProductRepo()
	repo.AddErr = errSentinel
	h := command.NewCreateProductHandler(repo, nil, func() time.Time { return fixedNow }, testNewProductID)
	tid := newTenantID(t)
	actor := newMembershipID(t)

	_, err := h.Handle(t.Context(), command.CreateProductCommand{
		TenantID: tid, ActorMembershipID: actor,
		SKU: "X-1", Name: "X",
		DosageForm: "Tablet", PackSize: "10",
		HSNCode: "3004", GSTRateBps: new(1200),
	})
	if !errors.Is(err, errSentinel) {
		t.Fatalf("err: got %v want errSentinel propagated", err)
	}
}

// fixedGstDefaults answers the same category default for every category.
type fixedGstDefaults struct{ bps int }

func (f fixedGstDefaults) Default(context.Context, string) (int, bool, error) {
	return f.bps, true, nil
}

// C14: an explicit 0% must be stored as 0%, not replaced by the category
// default; an absent rate still takes the default.
func TestCreateProductHandler_GSTRate_AbsentVersusExplicitZero(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		gst  *int
		want int
	}{
		{"absent takes the category default", nil, 1200},
		{"explicit zero stays zero", new(0), 0},
		{"explicit rate wins over the default", new(500), 500},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			repo := newFakeProductRepo()
			h := command.NewCreateProductHandler(repo, fixedGstDefaults{bps: 1200},
				func() time.Time { return fixedNow }, testNewProductID)
			tid := newTenantID(t)

			out, err := h.Handle(t.Context(), command.CreateProductCommand{
				TenantID: tid, ActorMembershipID: newMembershipID(t),
				SKU: "GST-" + tc.name[:3], Name: "Nil rated", DosageForm: "Tablet", PackSize: "10",
				HSNCode: "3004", GSTRateBps: tc.gst,
			})
			if err != nil {
				t.Fatalf("Handle: %v", err)
			}
			p, err := repo.GetByID(t.Context(), tid, out.ProductID)
			if err != nil {
				t.Fatalf("GetByID: %v", err)
			}
			if p.GSTRateBps() != tc.want {
				t.Fatalf("stored gst_rate_bps = %d, want %d", p.GSTRateBps(), tc.want)
			}
		})
	}
}

// The form's three fields reach the aggregate. An absent expiry-alert threshold
// takes the BRD default (90 days); an explicit 0 switches the alert off and is
// kept (it used to be the silent result of every HTTP create).
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
			h := command.NewCreateProductHandler(repo, nil, func() time.Time { return fixedNow }, testNewProductID)
			tid := newTenantID(t)

			out, err := h.Handle(t.Context(), command.CreateProductCommand{
				TenantID: tid, ActorMembershipID: newMembershipID(t),
				SKU: "FORM-1", Name: "Form", DosageForm: "Tablet", PackSize: "10", HSNCode: "3004", GSTRateBps: new(500),
				ProductCategory: "Cardiac", ReorderLevel: 40, ExpiryAlertThresholdDays: tc.threshold,
			})
			if err != nil {
				t.Fatalf("Handle: %v", err)
			}
			p, err := repo.GetByID(t.Context(), tid, out.ProductID)
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
