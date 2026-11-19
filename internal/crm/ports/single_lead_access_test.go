package ports

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/leadkart/leadkart-go/internal/common/pagination"
	"github.com/leadkart/leadkart-go/internal/common/pg"
	"github.com/leadkart/leadkart-go/internal/crm/app"
	"github.com/leadkart/leadkart-go/internal/crm/app/command"
	cmdquery "github.com/leadkart/leadkart-go/internal/crm/app/query"
	"github.com/leadkart/leadkart-go/internal/crm/domain/assignmenthistory"
	"github.com/leadkart/leadkart-go/internal/crm/domain/assignmenthistory/assignmenthistorytest"
	"github.com/leadkart/leadkart-go/internal/crm/domain/calllog"
	"github.com/leadkart/leadkart-go/internal/crm/domain/calllog/calllogtest"
	"github.com/leadkart/leadkart-go/internal/crm/domain/crmlead"
	"github.com/leadkart/leadkart-go/internal/crm/domain/crmlead/crmleadtest"
	"github.com/leadkart/leadkart-go/internal/crm/domain/reminder"
	"github.com/leadkart/leadkart-go/internal/crm/domain/reminder/remindertest"
	"github.com/leadkart/leadkart-go/internal/identity/app/jwt"
	"github.com/leadkart/leadkart-go/internal/identity/domain/permission"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
	"github.com/leadkart/leadkart-go/internal/identity/ports/authn"
)

// The single-lead access rule: a caller holding crm.leads.read_all (or operator
// reach) reaches any lead of the tenant; everyone else only a lead assigned to
// their own membership, and any other lead answers 404 exactly like a lead
// that does not exist.

type inlineUoW struct{}

func (inlineUoW) WithinTx(ctx context.Context, _ pg.TxScope, fn func(ctx context.Context) error) error {
	return fn(ctx)
}

type accessWorld struct {
	tenantID  string
	leads     *crmleadtest.FakeRepository
	reminders *remindertest.FakeRepository
	app       app.Application
	owner     string // membership the lead and the reminder are assigned to
	other     string // another member of the tenant
	leadID    string
	remID     string
}

func newAccessWorld(t *testing.T) *accessWorld {
	t.Helper()
	w := &accessWorld{
		tenantID:  uuid.NewString(),
		leads:     crmleadtest.NewFakeRepository(),
		reminders: remindertest.NewFakeRepository(),
		owner:     uuid.NewString(),
		other:     uuid.NewString(),
	}
	now := func() time.Time { return time.Date(2026, 11, 20, 9, 0, 0, 0, time.UTC) }
	l, err := crmlead.New(crmlead.ID(uuid.NewString()), tenant.ID(w.tenantID), crmlead.Profile{ContactName: "Chemist", PhoneE164: "+919812345678"}, w.owner, now())
	if err != nil {
		t.Fatal(err)
	}
	if err := l.Assign(w.owner, w.owner, "seed", now()); err != nil {
		t.Fatal(err)
	}
	if err := w.leads.Add(t.Context(), l); err != nil {
		t.Fatal(err)
	}
	w.leadID = l.ID().String()
	rem, err := reminder.NewManualReminder(reminder.ID(uuid.NewString()), tenant.ID(w.tenantID), l.ID(), w.owner, w.owner, now().Add(time.Hour), "call back", now())
	if err != nil {
		t.Fatal(err)
	}
	if err := w.reminders.Add(t.Context(), rem); err != nil {
		t.Fatal(err)
	}
	w.remID = rem.ID().String()
	calls := calllogtest.NewFakeRepository()
	history := assignmenthistorytest.NewFakeRepository()
	w.app = app.Application{
		Commands: app.Commands{
			AssignLead:            command.NewAssignLeadHandler(w.leads, history, inlineUoW{}, now, func() assignmenthistory.ID { return assignmenthistory.ID(uuid.NewString()) }),
			ChangeLeadStage:       command.NewChangeLeadStageHandler(w.leads, now),
			ChangeLeadTemperature: command.NewChangeLeadTemperatureHandler(w.leads, now),
			LogCall:               command.NewLogCallHandler(w.leads, calls, now, func() calllog.ID { return calllog.ID(uuid.NewString()) }),
			ConvertLead:           command.NewConvertLeadHandler(w.leads, now),
			LoseLead:              command.NewLoseLeadHandler(w.leads, now),
			CreateReminder:        command.NewCreateReminderHandler(w.leads, w.reminders, now, func() reminder.ID { return reminder.ID(uuid.NewString()) }),
			MarkReminderSent:      command.NewMarkReminderSentHandler(w.reminders, now),
			CancelReminder:        command.NewCancelReminderHandler(w.reminders, now),
		},
		Queries: app.Queries{GetLead: cmdquery.NewGetLeadHandler(w.leads)},
	}
	return w
}

func (w *accessWorld) claims(membership string, perms ...string) *jwt.Claims {
	return &jwt.Claims{MembershipID: membership, TenantID: w.tenantID, TenantSlug: "acme", Permissions: perms}
}

func (w *accessWorld) lead(t *testing.T) *crmlead.CrmLead {
	t.Helper()
	l, err := w.leads.GetByID(t.Context(), tenant.ID(w.tenantID), crmlead.ID(w.leadID))
	if err != nil {
		t.Fatal(err)
	}
	return l
}

func (w *accessWorld) reminder(t *testing.T) *reminder.Reminder {
	t.Helper()
	r, err := w.reminders.GetByID(t.Context(), tenant.ID(w.tenantID), reminder.ID(w.remID))
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// accessRoute is one single-lead (or by-reminder) route plus a probe of whether
// it changed anything.
type accessRoute struct {
	name    string
	handler func(w *accessWorld) http.Handler
	body    string
	pathKey string // "leadId" or "reminderId"
	okCode  int
	changed func(t *testing.T, w *accessWorld) bool
}

func accessRoutes() []accessRoute {
	log := silentLog()
	return []accessRoute{
		{"get lead", func(w *accessWorld) http.Handler { return handleGetLead(log, w.app) }, "", "leadId", http.StatusOK,
			func(*testing.T, *accessWorld) bool { return false }},
		{"change stage", func(w *accessWorld) http.Handler { return handleChangeStage(log, w.app) }, `{"new_stage":"contacted","reason":"first call"}`, "leadId", http.StatusNoContent,
			func(t *testing.T, w *accessWorld) bool { return w.lead(t).Stage() != crmlead.StageNew }},
		{"change temperature", func(w *accessWorld) http.Handler { return handleChangeTemperature(log, w.app) }, `{"new_temperature":"hot"}`, "leadId", http.StatusNoContent,
			func(t *testing.T, w *accessWorld) bool { return w.lead(t).Temperature() == crmlead.TemperatureHot }},
		{"log call", func(w *accessWorld) http.Handler { return handleLogCall(log, w.app) }, `{"outcome":"connected","notes":"spoke"}`, "leadId", http.StatusCreated,
			func(*testing.T, *accessWorld) bool { return false }},
		{"convert", func(w *accessWorld) http.Handler { return handleConvertLead(log, w.app) }, ``, "leadId", http.StatusNoContent,
			func(t *testing.T, w *accessWorld) bool { return w.lead(t).Stage() == crmlead.StageConverted }},
		{"lose", func(w *accessWorld) http.Handler { return handleLoseLead(log, w.app) }, `{"reason":"not interested"}`, "leadId", http.StatusNoContent,
			func(t *testing.T, w *accessWorld) bool { return w.lead(t).Stage() == crmlead.StageLost }},
		{"assign", func(w *accessWorld) http.Handler { return handleAssignLead(log, w.app) }, `{"assignee_membership_id":"__OTHER__","reason":"rebalance"}`, "leadId", http.StatusOK,
			func(t *testing.T, w *accessWorld) bool { return w.lead(t).AssigneeMembershipID() != w.owner }},
		{"create reminder", func(w *accessWorld) http.Handler { return handleCreateReminder(log, w.app) }, `{"assigned_to_membership_id":"__OTHER__","due_at":"2030-01-01T00:00:00Z"}`, "leadId", http.StatusCreated,
			func(*testing.T, *accessWorld) bool { return false }},
		{"mark reminder sent", func(w *accessWorld) http.Handler { return handleMarkReminderSent(log, w.app) }, ``, "reminderId", http.StatusNoContent,
			func(t *testing.T, w *accessWorld) bool { return w.reminder(t).State() == reminder.StateSent }},
		{"cancel reminder", func(w *accessWorld) http.Handler { return handleCancelReminder(log, w.app) }, `{"reason":"no longer needed"}`, "reminderId", http.StatusNoContent,
			func(t *testing.T, w *accessWorld) bool { return w.reminder(t).State() == reminder.StateCancelled }},
	}
}

func (rt accessRoute) call(t *testing.T, w *accessWorld, c *jwt.Claims) *httptest.ResponseRecorder {
	t.Helper()
	ctx := h8Ctx(t.Context(), c)
	body := strings.ReplaceAll(rt.body, "__OTHER__", w.other)
	r := httptest.NewRequestWithContext(ctx, http.MethodPost, "/", strings.NewReader(body))
	id := w.leadID
	if rt.pathKey == "reminderId" {
		id = w.remID
	}
	r.SetPathValue(rt.pathKey, id)
	rec := httptest.NewRecorder()
	rt.handler(w).ServeHTTP(rec, r)
	return rec
}

func TestSingleLeadRoutes_AssigneeOnlyUnlessReadAllOrOperator(t *testing.T) {
	t.Parallel()
	readAll := permission.IdentityPermissions.Crm.Leads.ReadAll
	for _, rt := range accessRoutes() {
		t.Run(rt.name, func(t *testing.T) {
			t.Parallel()

			// A member who is not the assignee and lacks read_all: 404, nothing changed.
			w := newAccessWorld(t)
			rec := rt.call(t, w, w.claims(w.other))
			wantCode := errCodeLeadNotFound
			if rt.pathKey == "reminderId" {
				wantCode = errCodeReminderNotFound
			}
			if rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), `"`+wantCode+`"`) {
				t.Errorf("non-assignee: %d %s, want 404 %s", rec.Code, rec.Body.String(), wantCode)
			}
			if rt.changed(t, w) {
				t.Error("non-assignee changed state")
			}

			// A caller whose membership claim is not an id is restricted to nothing.
			w = newAccessWorld(t)
			if rec := rt.call(t, w, w.claims("not-a-uuid")); rec.Code != http.StatusNotFound {
				t.Errorf("unusable membership claim: %d %s, want 404", rec.Code, rec.Body.String())
			}
			if rt.changed(t, w) {
				t.Error("caller without a membership changed state")
			}

			// The assignee (spelled in upper case in the claim) passes.
			w = newAccessWorld(t)
			if rec := rt.call(t, w, w.claims(strings.ToUpper(w.owner))); rec.Code != rt.okCode {
				t.Errorf("assignee: %d %s, want %d", rec.Code, rec.Body.String(), rt.okCode)
			}

			// read_all passes.
			w = newAccessWorld(t)
			if rec := rt.call(t, w, w.claims(w.other, readAll)); rec.Code != rt.okCode {
				t.Errorf("read_all: %d %s, want %d", rec.Code, rec.Body.String(), rt.okCode)
			}

			// Operator reach (platform.tenants.view on a platform-slug principal) passes.
			w = newAccessWorld(t)
			op := w.claims(w.other, permission.IdentityPermissions.Platform.TenantsView)
			op.IsPlatform, op.TenantSlug = true, authn.PlatformTenantSlug
			if !authn.CanActAcrossTenants(op) {
				t.Fatal("test operator claims do not give operator reach")
			}
			if rec := rt.call(t, w, op); rec.Code != rt.okCode {
				t.Errorf("operator: %d %s, want %d", rec.Code, rec.Body.String(), rt.okCode)
			}
		})
	}
}

// A lead that does not exist and a lead the caller may not see answer the same
// way, so a lead id leaks nothing.
func TestSingleLeadRoutes_UnseenLeadLooksLikeMissingLead(t *testing.T) {
	t.Parallel()
	w := newAccessWorld(t)
	get := accessRoutes()[0]
	seen := get.call(t, w, w.claims(w.other))
	w.leadID = uuid.NewString()
	missing := get.call(t, w, w.claims(w.other))
	if seen.Code != missing.Code || seen.Body.String() != missing.Body.String() {
		t.Errorf("unseen lead %d %s differs from missing lead %d %s", seen.Code, seen.Body.String(), missing.Code, missing.Body.String())
	}
}

// The assign response echoes the canonical assignee id, not the spelling sent.
func TestAssignLead_ResponseEchoesCanonicalAssignee(t *testing.T) {
	t.Parallel()
	w := newAccessWorld(t)
	upper := strings.ToUpper(w.other)
	body := `{"assignee_membership_id":"` + upper + `","reason":"rebalance"}`
	r := httptest.NewRequestWithContext(h8Ctx(t.Context(), w.claims(w.owner)), http.MethodPost, "/", strings.NewReader(body))
	r.SetPathValue("leadId", w.leadID)
	rec := httptest.NewRecorder()
	handleAssignLead(silentLog(), w.app).ServeHTTP(rec, r)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"`+w.other+`"`) || strings.Contains(rec.Body.String(), upper) {
		t.Fatalf("assign: %d %s, want 200 echoing %s", rec.Code, rec.Body.String(), w.other)
	}
	if got := w.lead(t).AssigneeMembershipID(); got != w.other {
		t.Errorf("assignee reaching the command = %q, want canonical %q", got, w.other)
	}
}

// Upper-case body ids reach the commands in canonical form.
func TestReminderCreate_BodyIDReachesTheCommandCanonical(t *testing.T) {
	t.Parallel()
	w := newAccessWorld(t)
	upper := strings.ToUpper(w.other)
	body := `{"assigned_to_membership_id":"` + upper + `","due_at":"2030-01-01T00:00:00Z"}`
	r := httptest.NewRequestWithContext(h8Ctx(t.Context(), w.claims(w.owner)), http.MethodPost, "/", strings.NewReader(body))
	r.SetPathValue("leadId", w.leadID)
	rec := httptest.NewRecorder()
	handleCreateReminder(silentLog(), w.app).ServeHTTP(rec, r)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create reminder: %d %s", rec.Code, rec.Body.String())
	}
	page, err := w.reminders.ListPagePending(t.Context(), tenant.ID(w.tenantID), reminder.PendingFilter{AssigneeMembershipID: w.other}, pagination.Cursor{}, 10)
	if err != nil || len(page.Items) != 1 {
		t.Fatalf("reminders assigned to the canonical id: %d (err %v), want 1", len(page.Items), err)
	}
}
