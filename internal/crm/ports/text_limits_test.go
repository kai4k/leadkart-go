// text_limits_test.go — I6: a text over its limit is a field-named 422, never a
// 500, and a too-long lose reason is no longer reported as invalid_stage.

package ports

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/leadkart/leadkart-go/internal/common/pg"
	"github.com/leadkart/leadkart-go/internal/common/textlimit"
	"github.com/leadkart/leadkart-go/internal/crm/app"
	"github.com/leadkart/leadkart-go/internal/crm/app/command"
	"github.com/leadkart/leadkart-go/internal/crm/domain/assignmenthistory"
	"github.com/leadkart/leadkart-go/internal/crm/domain/assignmenthistory/assignmenthistorytest"
	"github.com/leadkart/leadkart-go/internal/crm/domain/calllog"
	"github.com/leadkart/leadkart-go/internal/crm/domain/calllog/calllogtest"
	"github.com/leadkart/leadkart-go/internal/crm/domain/crmlead"
	"github.com/leadkart/leadkart-go/internal/crm/domain/crmlead/crmleadtest"
	"github.com/leadkart/leadkart-go/internal/crm/domain/reminder"
	"github.com/leadkart/leadkart-go/internal/crm/domain/reminder/remindertest"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)

// tlNow pins the clock every handler in this file runs on.
var tlNow = time.Date(2026, 6, 2, 9, 0, 0, 0, time.UTC)

func tlClock() time.Time { return tlNow }

type directUoW struct{}

func (directUoW) WithinTx(ctx context.Context, _ pg.TxScope, fn func(context.Context) error) error {
	return fn(ctx)
}

type textLimitFixture struct {
	app    app.Application
	tenant string
	actor  string
	lead   string
}

func newTextLimitFixture(t *testing.T) textLimitFixture {
	t.Helper()
	f := textLimitFixture{tenant: uuid.NewString(), actor: uuid.NewString(), lead: uuid.NewString()}
	leads := crmleadtest.NewFakeRepository()
	l, err := crmlead.New(crmlead.ID(f.lead), tenant.ID(f.tenant), crmlead.Profile{
		ContactName: "Sharma Medical Store", PhoneE164: "+919876543210", City: "Pune", District: "Pune",
		State: "Maharashtra", Pincode: "411001", BusinessType: "PCD", MedicineSystem: "Allopathic",
		OrderValue: "Upto25000", BuyTimeline: "WithinWeek",
	}, f.actor, tlNow)
	if err != nil {
		t.Fatalf("seed lead: %v", err)
	}
	if err := leads.Add(t.Context(), l); err != nil {
		t.Fatalf("seed lead add: %v", err)
	}
	history := assignmenthistorytest.NewFakeRepository()
	calls := calllogtest.NewFakeRepository()
	reminders := remindertest.NewFakeRepository()
	f.app = app.Application{Commands: app.Commands{
		AssignLead: command.NewAssignLeadHandler(leads, history, directUoW{}, tlClock,
			func() assignmenthistory.ID { return assignmenthistory.ID(uuid.NewString()) }),
		LogCall: command.NewLogCallHandler(leads, calls, directUoW{}, tlClock,
			func() calllog.ID { return calllog.ID(uuid.NewString()) }),
		LoseLead: command.NewLoseLeadHandler(leads, tlClock),
		CreateReminder: command.NewCreateReminderHandler(leads, reminders, directUoW{}, tlClock,
			func() reminder.ID { return reminder.ID(uuid.NewString()) }),
	}}
	return f
}

// post runs h with a read_all caller (so no assignee scoping applies) and
// returns the status and the error code of the answer.
func (f textLimitFixture) post(t *testing.T, h http.Handler, body any) (int, string) {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	claims := h8Claims(f.actor, true)
	claims.TenantID = f.tenant
	req := httptest.NewRequestWithContext(h8Ctx(t.Context(), claims), http.MethodPost, "/x", strings.NewReader(string(raw)))
	req.SetPathValue("leadId", f.lead)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var er errorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &er); err != nil {
		t.Fatalf("decode error body %q: %v", rec.Body.String(), err)
	}
	return rec.Code, er.Error
}

func TestCrmWrites_TextOverItsLimitIs422WithAFieldCode(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		run  func(t *testing.T, f textLimitFixture) (int, string)
		code string
	}{
		{"assign reason over 1000 bytes", func(t *testing.T, f textLimitFixture) (int, string) {
			return f.post(t, handleAssignLead(silentLog(), f.app), AssignLeadRequest{
				AssigneeMembershipID: uuid.NewString(), Reason: strings.Repeat("x", 1001)})
		}, "reason_too_long"},
		{"call notes over 4000 bytes", func(t *testing.T, f textLimitFixture) (int, string) {
			return f.post(t, handleLogCall(silentLog(), f.app), LogCallRequest{
				Outcome: "connected", Notes: strings.Repeat("x", 4001)})
		}, "notes_too_long"},
		{"lose reason over 1000 bytes (was invalid_stage)", func(t *testing.T, f textLimitFixture) (int, string) {
			return f.post(t, handleLoseLead(silentLog(), f.app), LoseLeadRequest{Reason: strings.Repeat("x", 1001)})
		}, "reason_too_long"},
		{"reminder notes over 2000 bytes (was invalid_due_at)", func(t *testing.T, f textLimitFixture) (int, string) {
			return f.post(t, handleCreateReminder(silentLog(), f.app), CreateReminderRequest{
				AssignedToMembershipID: uuid.NewString(), DueAt: tlNow.Add(time.Hour),
				Notes: strings.Repeat("x", 2001)})
		}, "notes_too_long"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			status, code := tc.run(t, newTextLimitFixture(t))
			if status != http.StatusUnprocessableEntity || code != tc.code {
				t.Fatalf("status %d code %q; want 422 %s", status, code, tc.code)
			}
		})
	}
}

// The value just inside each limit still goes through (nothing refused early).
func TestCrmWrites_TextAtItsLimitIsAccepted(t *testing.T) {
	t.Parallel()
	f := newTextLimitFixture(t)
	status, code := f.post(t, handleLogCall(silentLog(), f.app), LogCallRequest{
		Outcome: "connected", Notes: strings.Repeat("x", 4000)})
	if status != http.StatusCreated {
		t.Fatalf("4000-byte note: status %d code %q; want 201", status, code)
	}
}

// Cancel-reminder reasons are mapped by mapReminderMutationErr: a too-long
// reason is reason_too_long, not reason_required.
func TestMapReminderMutationErr_ReasonTooLong(t *testing.T) {
	t.Parallel()
	err := wrapInvalid(reminder.ErrInvalid, textlimit.Bytes("reason", strings.Repeat("x", 1001), 1000))
	rec := httptest.NewRecorder()
	mapReminderMutationErr(rec, silentLog(), httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/x", nil), err, "cancel reminder")
	var er errorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &er); err != nil {
		t.Fatalf("decode error body %q: %v", rec.Body.String(), err)
	}
	if rec.Code != http.StatusUnprocessableEntity || er.Error != "reason_too_long" {
		t.Fatalf("status %d code %q; want 422 reason_too_long", rec.Code, er.Error)
	}
}

// Other crmlead validation failures keep their existing code; only the
// too-long reason moved.
func TestMapMutationErr_OtherInvalidKeepsInvalidStage(t *testing.T) {
	t.Parallel()
	rec := httptest.NewRecorder()
	mapMutationErr(rec, silentLog(), httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/x", nil),
		wrapInvalid(crmlead.ErrInvalid, nil), "stage")
	var er errorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &er); err != nil {
		t.Fatalf("decode error body %q: %v", rec.Body.String(), err)
	}
	if rec.Code != http.StatusUnprocessableEntity || er.Error != errCodeInvalidStage {
		t.Fatalf("status %d code %q; want 422 %s", rec.Code, er.Error, errCodeInvalidStage)
	}
}

func wrapInvalid(sentinel, cause error) error {
	if cause == nil {
		return sentinel
	}
	return fmt.Errorf("%w: %w", sentinel, cause)
}

func (directUoW) WithinTenantTx(ctx context.Context, _ string, fn func(context.Context) error) error {
	return fn(ctx)
}
