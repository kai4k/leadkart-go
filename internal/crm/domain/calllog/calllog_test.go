package calllog_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/leadkart/leadkart-go/internal/crm/domain/calllog"
	"github.com/leadkart/leadkart-go/internal/crm/domain/crmlead"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)

// fixedNow pins the wall-clock so emitted-event timestamps are
// deterministic across the table tests (Khorikov §8 — pin time).
var fixedNow = time.Date(2026, 6, 2, 9, 0, 0, 0, time.UTC)

// Test fixture UUIDs — every aggregate ID must parse as RFC 9562 per
// reviewer H6 (validation at aggregate-construction time).
const (
	tidCall1   = "01923400-0000-7000-8000-ffffffff0001"
	tidCallI   = "01923400-0000-7000-8000-ffffffff0002"
	tidTenant1 = "01923400-0000-7000-8000-bbbbbbbb0001"
	tidLead1   = "01923400-0000-7000-8000-aaaaaaaa0001"
	tidLeadX   = "01923400-0000-7000-8000-aaaaaaaa0009"
	tidMember1 = "01923400-0000-7000-8000-cccccccc0001"
)

var tenantID1 = tenant.ID(tidTenant1)

func TestNew_HappyPath(t *testing.T) {
	t.Parallel()
	loggedAt := time.Date(2026, 6, 2, 10, 30, 0, 0, time.UTC)
	c, err := calllog.New(calllog.ID(tidCall1), tenantID1, crmlead.ID(tidLead1), calllog.OutcomeConnected, "good chat", tidMember1, loggedAt)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if c.ID() != calllog.ID(tidCall1) || c.Outcome() != calllog.OutcomeConnected || c.LoggedByMembershipID() != tidMember1 {
		t.Fatalf("fields: %+v", c)
	}
	if !c.LoggedAt().Equal(loggedAt) {
		t.Fatalf("LoggedAt: %v", c.LoggedAt())
	}
	evs := c.PullEvents()
	if len(evs) != 1 {
		t.Fatalf("events: %d", len(evs))
	}
	got, ok := evs[0].(calllog.LoggedEvent)
	if !ok || got.CallID != calllog.ID(tidCall1) || got.LeadID != crmlead.ID(tidLead1) || !got.At.Equal(loggedAt) {
		t.Fatalf("event: %+v", evs[0])
	}
}

func TestNew_Invariants(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		id   calllog.ID
		tid  tenant.ID
		lid  string
		out  calllog.Outcome
		not  string
		by   string
		at   time.Time
	}{
		{name: "missing id", id: "", tid: tenantID1, lid: tidLead1, out: calllog.OutcomeConnected, by: tidMember1, at: fixedNow},
		{name: "non-uuid id", id: calllog.ID("not-a-uuid"), tid: tenantID1, lid: tidLead1, out: calllog.OutcomeConnected, by: tidMember1, at: fixedNow},
		{name: "missing tenant", id: calllog.ID(tidCallI), tid: tenant.ID(""), lid: tidLead1, out: calllog.OutcomeConnected, by: tidMember1, at: fixedNow},
		{name: "non-uuid tenant", id: calllog.ID(tidCallI), tid: tenant.ID("not-a-uuid"), lid: tidLead1, out: calllog.OutcomeConnected, by: tidMember1, at: fixedNow},
		{name: "missing lead", id: calllog.ID(tidCallI), tid: tenantID1, lid: "", out: calllog.OutcomeConnected, by: tidMember1, at: fixedNow},
		{name: "bad outcome", id: calllog.ID(tidCallI), tid: tenantID1, lid: tidLead1, out: calllog.Outcome("rude"), by: tidMember1, at: fixedNow},
		{name: "missing by", id: calllog.ID(tidCallI), tid: tenantID1, lid: tidLead1, out: calllog.OutcomeBusy, by: "", at: fixedNow},
		{name: "notes too long", id: calllog.ID(tidCallI), tid: tenantID1, lid: tidLead1, out: calllog.OutcomeBusy, not: strings.Repeat("x", 5000), by: tidMember1, at: fixedNow},
		{name: "zero logged_at", id: calllog.ID(tidCallI), tid: tenantID1, lid: tidLead1, out: calllog.OutcomeBusy, by: tidMember1, at: time.Time{}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			lead := crmlead.ID(tc.lid)
			_, err := calllog.New(tc.id, tc.tid, lead, tc.out, tc.not, tc.by, tc.at)
			if err == nil {
				t.Fatalf("want error")
			}
			if !errors.Is(err, calllog.ErrInvalid) {
				t.Fatalf("want ErrInvalid, got %v", err)
			}
		})
	}
}

func TestParseOutcome(t *testing.T) {
	t.Parallel()
	if o, err := calllog.ParseOutcome("connected"); err != nil || o != calllog.OutcomeConnected {
		t.Fatalf("ParseOutcome: %v %v", o, err)
	}
	if _, err := calllog.ParseOutcome("snowing"); !errors.Is(err, calllog.ErrInvalid) {
		t.Fatalf("want ErrInvalid, got %v", err)
	}
}

func TestUnmarshalFromDB_RoundTrip(t *testing.T) {
	t.Parallel()
	snap := calllog.Snapshot{
		ID: "c-1", TenantID: tenant.ID("t"), LeadID: "l", Outcome: calllog.OutcomeInterested,
		Notes: "hot", LoggedByMembershipID: "m", LoggedAt: fixedNow, CreatedAt: fixedNow,
	}
	c := calllog.UnmarshalFromDB(snap)
	if c.PullEvents() != nil {
		t.Fatal("UnmarshalFromDB should emit no events")
	}
	if c.Outcome() != calllog.OutcomeInterested {
		t.Fatalf("outcome: %s", c.Outcome())
	}
}
