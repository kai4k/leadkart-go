package query_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/uuid"

	"github.com/leadkart/leadkart-go/internal/common/commercepolicy"
	"github.com/leadkart/leadkart-go/internal/common/pagination"
	"github.com/leadkart/leadkart-go/internal/companysettings/app/query"
	"github.com/leadkart/leadkart-go/internal/companysettings/domain/commercesettings"
	"github.com/leadkart/leadkart-go/internal/companysettings/domain/commercesettings/commercesettingstest"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)

var tenantID = tenant.ID(uuid.NewString())

func TestGetSettingsHandler_TenantWithNoRowReadsTheDefaultsAtVersionZero(t *testing.T) {
	t.Parallel()
	h := query.NewGetSettingsHandler(commercesettingstest.NewFakeRepository())
	v, err := h.Handle(t.Context(), query.GetSettingsQuery{TenantID: tenantID})
	if err != nil {
		t.Fatal(err)
	}
	if v.Version != 0 || v.UpdatedAt != nil {
		t.Fatalf("view = %+v", v)
	}
	if diff := cmp.Diff(commercepolicy.Initial().Policy, v.Policy); diff != "" {
		t.Fatal(diff)
	}
	if len(v.Options) != len(commercepolicy.Settings()) {
		t.Fatalf("option metadata = %d entries, want %d", len(v.Options), len(commercepolicy.Settings()))
	}
}

func TestListGSTRatesHandler_StartsEmptyAndFiltersInactive(t *testing.T) {
	t.Parallel()
	repo := commercesettingstest.NewFakeRepository()
	h := query.NewListGSTRatesHandler(repo)
	empty, err := h.Handle(t.Context(), query.ListGSTRatesQuery{TenantID: tenantID})
	if err != nil || len(empty.Items) != 0 || empty.PolicyVersion != 0 {
		t.Fatalf("a company starts with no rate: %+v %v", empty, err)
	}
	at := time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC)
	five, twelve := uuid.NewString(), uuid.NewString()
	err = repo.UpdateByID(t.Context(), tenantID, func(s *commercesettings.Settings) (bool, error) {
		if err := s.AddGSTRate(commercesettings.AddGSTRateInput{ID: twelve, RateBps: 1200, Label: "GST 12%", ConfirmedBy: "A", ConfirmedOn: "2026-10-30", Reason: "r", Now: at}); err != nil {
			return false, err
		}
		if err := s.AddGSTRate(commercesettings.AddGSTRateInput{ID: five, RateBps: 500, Label: "GST 5%", ConfirmedBy: "A", ConfirmedOn: "2026-10-30", Reason: "r", Now: at}); err != nil {
			return false, err
		}
		off := false
		return true, s.UpdateGSTRate(commercesettings.UpdateGSTRateInput{ID: twelve, Active: &off, Reason: "retired", Now: at})
	})
	if err != nil {
		t.Fatal(err)
	}
	all, err := h.Handle(t.Context(), query.ListGSTRatesQuery{TenantID: tenantID})
	if err != nil || len(all.Items) != 2 || all.Items[0].RateBps != 500 || all.PolicyVersion != 3 {
		t.Fatalf("all rates in rate order at the current version: %+v %v", all, err)
	}
	active, err := h.Handle(t.Context(), query.ListGSTRatesQuery{TenantID: tenantID, ActiveOnly: true})
	if err != nil || len(active.Items) != 1 || active.Items[0].ID != five {
		t.Fatalf("the picker lists active rates only: %+v %v", active, err)
	}
}

func TestListHistoryHandler_ClampsThePageSize(t *testing.T) {
	t.Parallel()
	reader := &recordingReader{}
	h := query.NewListHistoryHandler(reader)
	if _, err := h.Handle(t.Context(), query.ListHistoryQuery{TenantID: tenantID, PageSize: 100000}); err != nil {
		t.Fatal(err)
	}
	if reader.size != pagination.MaxPageSize {
		t.Fatalf("page size = %d, want clamped to %d", reader.size, pagination.MaxPageSize)
	}
	if _, err := h.Handle(t.Context(), query.ListHistoryQuery{}); err == nil {
		t.Fatal("a missing tenant must be refused")
	}
}

type recordingReader struct{ size int }

func (r *recordingReader) ListHistory(_ context.Context, _ tenant.ID, _ pagination.Cursor, size int) (pagination.Page[query.HistoryEntry], error) {
	r.size = size
	return pagination.Page[query.HistoryEntry]{}, nil
}
