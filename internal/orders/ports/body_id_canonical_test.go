package ports

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/leadkart/leadkart-go/internal/common/tenancy"
	"github.com/leadkart/leadkart-go/internal/identity/app/jwt"
	"github.com/leadkart/leadkart-go/internal/identity/ports/authn"
	"github.com/leadkart/leadkart-go/internal/orders/app"
	"github.com/leadkart/leadkart-go/internal/orders/app/command"
	"github.com/leadkart/leadkart-go/internal/orders/domain/quotation"
	"github.com/leadkart/leadkart-go/internal/orders/domain/quotation/quotationtest"
)

// Body ids (customer_lead_id, items[].product_id) are validated with the same
// canonical rule as path ids: a malformed one is a 400 before any command runs
// (it used to reach the database as a 500), a valid one reaches the command in
// canonical form, and an absent one is still the domain's 422.

type bodyIDWorld struct {
	tid  string
	repo *quotationtest.FakeRepository
	app  app.Application
}

func newBodyIDWorld() *bodyIDWorld {
	repo := quotationtest.NewFakeRepository()
	now := func() time.Time { return time.Date(2026, 11, 20, 9, 0, 0, 0, time.UTC) }
	return &bodyIDWorld{
		tid:  uuid.NewString(),
		repo: repo,
		app: app.Application{Commands: app.Commands{
			CreateQuotation: command.NewCreateQuotationHandler(repo, now, func() quotation.ID { return quotation.ID(uuid.NewString()) }),
			ReviseQuotation: command.NewReviseQuotationHandler(repo, now),
		}},
	}
}

func (w *bodyIDWorld) post(t *testing.T, h http.Handler, body string, pathID string) *httptest.ResponseRecorder {
	t.Helper()
	c := &jwt.Claims{MembershipID: uuid.NewString(), TenantID: w.tid, TenantSlug: "acme"}
	ctx := tenancy.WithID(authn.WithClaims(t.Context(), c), tenancy.ID(w.tid))
	r := httptest.NewRequestWithContext(ctx, http.MethodPost, "/", strings.NewReader(body))
	if pathID != "" {
		r.SetPathValue("quotationId", pathID)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec
}

func quotationBody(lead, product string) string {
	return `{"customer_lead_id":"` + lead + `","items":[{"product_id":"` + product + `","sku":"SKU-1","quantity":2,"unit_sale_paise":1000,"gst_rate_bps":1200}]}`
}

func TestQuotationBodyIDs_MalformedIsA400AndNoCommandRuns(t *testing.T) {
	t.Parallel()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	good := uuid.NewString()
	for name, bad := range map[string]string{"garbage": "not-a-uuid", "padded": " " + good, "wrapped long": good + "extra", "nil": "00000000-0000-0000-0000-000000000000"} {
		w := newBodyIDWorld()
		for label, body := range map[string]string{
			"customer_lead_id": quotationBody(bad, good),
			"product_id":       quotationBody(good, bad),
		} {
			rec := w.post(t, handleCreateQuotation(log, w.app), body, "")
			if rec.Code != http.StatusBadRequest {
				t.Errorf("create, bad %s (%s): %d %s, want 400", label, name, rec.Code, rec.Body.String())
			}
		}
		if len(w.repo.Store) != 0 {
			t.Errorf("%s: a quotation was stored despite a malformed id", name)
		}
		// revise: the quotation must exist, so seed one through the handler first.
		seed := w.post(t, handleCreateQuotation(log, w.app), quotationBody(good, good), "")
		if seed.Code != http.StatusCreated {
			t.Fatalf("seed: %d %s", seed.Code, seed.Body.String())
		}
		var id quotation.ID
		for k := range w.repo.Store {
			id = k
		}
		rec := w.post(t, handleReviseQuotation(log, w.app), `{"items":[{"product_id":"`+bad+`","sku":"SKU-1","quantity":1,"unit_sale_paise":1,"gst_rate_bps":0}]}`, id.String())
		if rec.Code != http.StatusBadRequest {
			t.Errorf("revise, bad product_id (%s): %d %s, want 400", name, rec.Code, rec.Body.String())
		}
	}
}

func TestQuotationBodyIDs_CanonicalValueReachesTheCommand(t *testing.T) {
	t.Parallel()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	lead, product := uuid.NewString(), uuid.NewString()
	w := newBodyIDWorld()
	rec := w.post(t, handleCreateQuotation(log, w.app), quotationBody(strings.ToUpper(lead), strings.ToUpper(product)), "")
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	var q *quotation.Quotation
	for _, v := range w.repo.Store {
		q = v
	}
	if string(q.CustomerLeadID()) != lead || q.CurrentRevision().Items[0].ProductID != product {
		t.Fatalf("create stored lead=%q product=%q, want canonical %q / %q", q.CustomerLeadID(), q.CurrentRevision().Items[0].ProductID, lead, product)
	}
	other := uuid.NewString()
	rec = w.post(t, handleReviseQuotation(log, w.app), `{"items":[{"product_id":"`+strings.ToUpper(other)+`","sku":"SKU-2","quantity":1,"unit_sale_paise":5,"gst_rate_bps":0}]}`, q.ID().String())
	if rec.Code != http.StatusNoContent {
		t.Fatalf("revise: %d %s", rec.Code, rec.Body.String())
	}
	if got := w.repo.Store[q.ID()].CurrentRevision().Items[0].ProductID; got != other {
		t.Fatalf("revise stored product %q, want canonical %q", got, other)
	}
}

func TestQuotationBodyIDs_AbsentLeadIsStillValidation(t *testing.T) {
	t.Parallel()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	w := newBodyIDWorld()
	rec := w.post(t, handleCreateQuotation(log, w.app), quotationBody("", uuid.NewString()), "")
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("absent customer_lead_id: %d %s, want 422", rec.Code, rec.Body.String())
	}
}
