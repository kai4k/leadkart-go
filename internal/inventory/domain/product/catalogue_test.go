package product_test

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/leadkart/leadkart-go/internal/inventory/domain/product"
)

func TestNew_CatalogueFieldsAreOptionalAndBounded(t *testing.T) {
	t.Parallel()
	pid, tid, actor := freshIDs(t)
	spec := validSpec()
	p, err := product.New(pid, tid, actor, spec, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	if p.DrugScheduleClassID() != "" || p.GSTRateID() != "" || p.DefaultSaleRatePaise() != 0 || p.GenericName() != "" {
		t.Fatal("the catalogue fields start empty")
	}
	for name, mutate := range map[string]func(*product.Spec){
		"gst rate id not a uuid":     func(s *product.Spec) { s.GSTRateID = "twelve" },
		"drug schedule not a uuid":   func(s *product.Spec) { s.DrugScheduleClassID = "H" },
		"negative default sale rate": func(s *product.Spec) { s.DefaultSaleRatePaise = -1 },
		"default sale rate past cap": func(s *product.Spec) { s.DefaultSaleRatePaise = product.DefaultSaleRateMaxPaise + 1 },
		"generic name over 200":      func(s *product.Spec) { s.GenericName = strings.Repeat("g", 201) },
		"composition over 1000":      func(s *product.Spec) { s.Composition = strings.Repeat("c", 1001) },
		"storage condition with NUL": func(s *product.Spec) { s.StorageCondition = "a\x00b" },
	} {
		s := validSpec()
		mutate(&s)
		if _, err := product.New(pid, tid, actor, s, fixedNow); !errors.Is(err, product.ErrInvalid) {
			t.Errorf("%s: err = %v, want ErrInvalid", name, err)
		}
	}
}

func TestUpdate_CatalogueFieldsChangeAndReportTheirNames(t *testing.T) {
	t.Parallel()
	pid, tid, actor := freshIDs(t)
	p, err0 := product.New(pid, tid, actor, validSpec(), fixedNow)
	if err0 != nil {
		t.Fatal(err0)
	}
	_ = p.PullEvents()
	class := uuid.NewString()
	rate := uuid.NewString()
	err := p.Update(actor, product.UpdateSpec{
		GSTRateID: &rate, DrugScheduleClassID: &class, GenericName: strPtr(" amoxicillin "), DefaultSaleRatePaise: int64Ptr(9900),
	}, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	if p.GSTRateID() != rate || p.DrugScheduleClassID() != class || p.GenericName() != "amoxicillin" || p.DefaultSaleRatePaise() != 9900 {
		t.Fatalf("stored %+v", p)
	}
	ev, ok := p.PullEvents()[0].(product.UpdatedEvent)
	if !ok || !slices.Equal(ev.ChangedFields, []string{"default_sale_rate_paise", "drug_schedule", "generic_name", "gst_rate_id"}) {
		t.Fatalf("changed fields %v", ev.ChangedFields)
	}
	// Clearing the class is an update to the empty string, and a repeat changes nothing.
	empty := ""
	if err := p.Update(actor, product.UpdateSpec{DrugScheduleClassID: &empty}, fixedNow); err != nil || p.DrugScheduleClassID() != "" {
		t.Fatalf("clear: %v %q", err, p.DrugScheduleClassID())
	}
	_ = p.PullEvents()
	if err := p.Update(actor, product.UpdateSpec{DrugScheduleClassID: &empty}, fixedNow); err != nil || len(p.PullEvents()) != 0 {
		t.Fatal("a no-op update must emit nothing")
	}
	// A refused catalogue update changes nothing.
	bad := "x"
	if err := p.Update(actor, product.UpdateSpec{GenericName: strPtr("kept?"), DrugScheduleClassID: &bad}, fixedNow); !errors.Is(err, product.ErrInvalid) {
		t.Fatalf("err = %v", err)
	}
	if p.GenericName() != "amoxicillin" {
		t.Fatalf("a refused update changed the generic name to %q", p.GenericName())
	}
}

func int64Ptr(n int64) *int64 { return &n }
