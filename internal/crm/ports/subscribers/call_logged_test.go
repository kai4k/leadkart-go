package subscribers_test

import (
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/leadkart/leadkart-go/internal/common/ids"
	"github.com/leadkart/leadkart-go/internal/crm/app/command"
	"github.com/leadkart/leadkart-go/internal/crm/domain/crmlead"
	"github.com/leadkart/leadkart-go/internal/crm/domain/crmlead/crmleadtest"
	"github.com/leadkart/leadkart-go/internal/crm/domain/reminder"
	"github.com/leadkart/leadkart-go/internal/crm/domain/reminder/remindertest"
	"github.com/leadkart/leadkart-go/internal/crm/integrationevents"
	"github.com/leadkart/leadkart-go/internal/crm/ports/subscribers"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)

// pinnedSubNow gives deterministic timestamps to the subscriber-test
// command-handler now-func.
func pinnedSubNow() time.Time {
	return time.Date(2026, 6, 2, 9, 0, 0, 0, time.UTC)
}

// seedLeadForCallback inserts a CrmLead into the supplied fake so the
// CreateReminderHandler's parent-lead probe succeeds.
func seedLeadForCallback(t *testing.T, leads *crmleadtest.FakeRepository, tid tenant.ID) crmlead.ID {
	t.Helper()
	l, err := crmlead.New(
		crmlead.ID(ids.NewV7().String()),
		tid,
		crmlead.Profile{ContactName: "Cb", PhoneE164: "+919812345678"},
		"01923400-0000-7000-8000-cccccccc0001",
		pinnedSubNow(),
	)
	if err != nil {
		t.Fatalf("seed lead: %v", err)
	}
	if err := leads.Add(t.Context(), l); err != nil {
		t.Fatalf("seed Add: %v", err)
	}
	return l.ID()
}

// Post-cqrs (ADR 0067): the handler receives the already-decoded typed event;
// topic routing + payload decode are the EventProcessor's job, so the old
// wrong-topic + malformed-payload unit cases are gone.

func TestCallbackReminderCreator_NoWindow_NoOp(t *testing.T) {
	t.Parallel()
	leads := crmleadtest.NewFakeRepository()
	reminders := remindertest.NewFakeRepository()
	tid := uuid.New()
	leadID := seedLeadForCallback(t, leads, tenant.ID(tid.String()))

	create := command.NewCreateReminderHandler(
		leads, reminders, pinnedSubNow,
		func() reminder.ID { return reminder.ID(ids.NewV7().String()) },
	)
	h := subscribers.NewCallbackReminderCreator(create, silentLog())

	evt := &integrationevents.CrmCallLoggedV1{
		CallID:               uuid.New(),
		LeadID:               uuid.MustParse(leadID.String()),
		TenantIDClaim:        tid,
		Outcome:              "no_answer",
		LoggedByMembershipID: uuid.New(),
		// no callback window
		OccurredAtUTC: pinnedSubNow(),
	}
	if err := h.Handle(t.Context(), evt); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if len(reminders.ByID) != 0 {
		t.Fatalf("no reminder should be minted without a window; got %d", len(reminders.ByID))
	}
}

func TestCallbackReminderCreator_WithWindow_CreatesReminder(t *testing.T) {
	t.Parallel()
	leads := crmleadtest.NewFakeRepository()
	reminders := remindertest.NewFakeRepository()
	tid := uuid.New()
	leadID := seedLeadForCallback(t, leads, tenant.ID(tid.String()))
	callID := uuid.New()
	loggedBy := uuid.New()
	due := pinnedSubNow().Add(2 * time.Hour)

	create := command.NewCreateReminderHandler(
		leads, reminders, pinnedSubNow,
		func() reminder.ID { return reminder.ID(ids.NewV7().String()) },
	)
	h := subscribers.NewCallbackReminderCreator(create, silentLog())

	evt := &integrationevents.CrmCallLoggedV1{
		CallID:                callID,
		LeadID:                uuid.MustParse(leadID.String()),
		TenantIDClaim:         tid,
		Outcome:               "callback_requested",
		LoggedByMembershipID:  loggedBy,
		CallbackWindowStartAt: due,
		CallbackWindowEndAt:   due.Add(time.Hour),
		OccurredAtUTC:         pinnedSubNow(),
	}
	if err := h.Handle(t.Context(), evt); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if len(reminders.ByID) != 1 {
		t.Fatalf("want 1 reminder, got %d", len(reminders.ByID))
	}
	for _, r := range reminders.ByID {
		if r.Type() != reminder.TypeCallback {
			t.Fatalf("type: %q", r.Type())
		}
		if r.SourceCallLogID() != callID.String() {
			t.Fatalf("source_call_log_id: %q (want %q)", r.SourceCallLogID(), callID.String())
		}
		if r.AssignedToMembershipID() != loggedBy.String() {
			t.Fatalf("assigned_to: %q (want %q)", r.AssignedToMembershipID(), loggedBy.String())
		}
		if !r.DueAt().Equal(due) {
			t.Fatalf("due_at: %v (want %v)", r.DueAt(), due)
		}
	}
}

func TestCallbackReminderCreator_DuplicateBrokerDelivery_Ack(t *testing.T) {
	t.Parallel()
	leads := crmleadtest.NewFakeRepository()
	reminders := remindertest.NewFakeRepository()
	tid := uuid.New()
	leadID := seedLeadForCallback(t, leads, tenant.ID(tid.String()))

	create := command.NewCreateReminderHandler(
		leads, reminders, pinnedSubNow,
		func() reminder.ID { return reminder.ID(ids.NewV7().String()) },
	)
	h := subscribers.NewCallbackReminderCreator(create, silentLog())

	due := pinnedSubNow().Add(2 * time.Hour)
	evt := &integrationevents.CrmCallLoggedV1{
		CallID:                uuid.New(),
		LeadID:                uuid.MustParse(leadID.String()),
		TenantIDClaim:         tid,
		Outcome:               "callback_requested",
		LoggedByMembershipID:  uuid.New(),
		CallbackWindowStartAt: due,
		CallbackWindowEndAt:   due.Add(time.Hour),
		OccurredAtUTC:         pinnedSubNow(),
	}
	if err := h.Handle(t.Context(), evt); err != nil {
		t.Fatalf("first: %v", err)
	}
	// Replay — same payload, same call_id. The partial unique gate should
	// turn this into AlreadyExisted=true + ACK.
	if err := h.Handle(t.Context(), evt); err != nil {
		t.Fatalf("replay: %v", err)
	}
	if len(reminders.ByID) != 1 {
		t.Fatalf("want 1 reminder after replay, got %d", len(reminders.ByID))
	}
}
