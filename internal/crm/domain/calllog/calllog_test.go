package calllog_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/leadkart/leadkart-go/internal/crm/domain/calllog"
	"github.com/leadkart/leadkart-go/internal/crm/domain/crmlead"
)

func TestNew_HappyPath(t *testing.T) {
	t.Parallel()
	loggedAt := time.Date(2026, 6, 2, 10, 30, 0, 0, time.UTC)
	c, err := calllog.New("call-1", "tenant-1", "lead-1", calllog.OutcomeConnected, "good chat", "mem-actor", loggedAt)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if c.ID() != "call-1" || c.Outcome() != calllog.OutcomeConnected || c.LoggedByMembershipID() != "mem-actor" {
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
	if !ok || got.CallID != "call-1" || got.LeadID != "lead-1" || !got.At.Equal(loggedAt) {
		t.Fatalf("event: %+v", evs[0])
	}
}

func TestNew_Invariants(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		id   calllog.ID
		tid  string
		lid  string
		out  calllog.Outcome
		not  string
		by   string
		at   time.Time
	}{
		{name: "missing id", id: "", tid: "t", lid: "l", out: calllog.OutcomeConnected, by: "m", at: time.Now()},
		{name: "missing tenant", id: "i", tid: "", lid: "l", out: calllog.OutcomeConnected, by: "m", at: time.Now()},
		{name: "missing lead", id: "i", tid: "t", lid: "", out: calllog.OutcomeConnected, by: "m", at: time.Now()},
		{name: "bad outcome", id: "i", tid: "t", lid: "l", out: calllog.Outcome("rude"), by: "m", at: time.Now()},
		{name: "missing by", id: "i", tid: "t", lid: "l", out: calllog.OutcomeBusy, by: "", at: time.Now()},
		{name: "notes too long", id: "i", tid: "t", lid: "l", out: calllog.OutcomeBusy, not: strings.Repeat("x", 5000), by: "m", at: time.Now()},
		{name: "zero logged_at", id: "i", tid: "t", lid: "l", out: calllog.OutcomeBusy, by: "m", at: time.Time{}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			lead := crmlead.ID("lead-x")
			if tc.name == "missing lead" {
				lead = ""
			}
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
		ID: "c-1", TenantID: "t", LeadID: "l", Outcome: calllog.OutcomeInterested,
		Notes: "hot", LoggedByMembershipID: "m", LoggedAt: time.Now().UTC(), CreatedAt: time.Now().UTC(),
	}
	c := calllog.UnmarshalFromDB(snap)
	if c.PullEvents() != nil {
		t.Fatal("UnmarshalFromDB should emit no events")
	}
	if c.Outcome() != calllog.OutcomeInterested {
		t.Fatalf("outcome: %s", c.Outcome())
	}
}
