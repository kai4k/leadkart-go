package verificationcall_test

import (
	"errors"
	"testing"
	"time"

	"github.com/leadkart/leadkart-go/internal/platform/domain/unverifiedcontact"
	"github.com/leadkart/leadkart-go/internal/platform/domain/verificationcall"
)

var (
	callID    = verificationcall.ID("01900000-0000-7000-8000-000000000100")
	contactID = unverifiedcontact.ID("01900000-0000-7000-8000-000000000010")
	agentID   = unverifiedcontact.MembershipID("01900000-0000-7000-8000-000000000001")
	now       = time.Date(2026, 6, 1, 10, 0, 0, 0, time.UTC)
)

func TestNew_NonBusyOutcome_HappyPath(t *testing.T) {
	t.Parallel()
	c, err := verificationcall.New(
		callID, contactID, verificationcall.OutcomeVerified, "Confirmed in person",
		time.Time{}, time.Time{}, agentID, now,
	)
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if c.Outcome() != verificationcall.OutcomeVerified {
		t.Errorf("outcome=%q", c.Outcome())
	}
	evs := c.PullEvents()
	if len(evs) != 1 {
		t.Fatalf("expected 1 event, got %d", len(evs))
	}
	if _, ok := evs[0].(verificationcall.LoggedEvent); !ok {
		t.Errorf("expected LoggedEvent, got %T", evs[0])
	}
}

func TestNew_BusyOutcome_RequiresCallbackWindow(t *testing.T) {
	t.Parallel()
	_, err := verificationcall.New(
		callID, contactID, verificationcall.OutcomeBusy, "",
		time.Time{}, time.Time{}, agentID, now,
	)
	if !errors.Is(err, verificationcall.ErrInvalid) {
		t.Errorf("expected ErrInvalid, got %v", err)
	}
}

func TestNew_BusyOutcome_HappyPath(t *testing.T) {
	t.Parallel()
	cbAt := now.Add(time.Hour)
	cbEnd := cbAt.Add(30 * time.Minute)
	c, err := verificationcall.New(
		callID, contactID, verificationcall.OutcomeBusy, "Customer asked to call back",
		cbAt, cbEnd, agentID, now,
	)
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if !c.CallbackWindowStartAt().Equal(cbAt) {
		t.Errorf("CallbackWindowStartAt=%v", c.CallbackWindowStartAt())
	}
}

func TestNew_BusyOutcome_EndBeforeStart_Rejected(t *testing.T) {
	t.Parallel()
	_, err := verificationcall.New(
		callID, contactID, verificationcall.OutcomeBusy, "",
		now.Add(2*time.Hour), now.Add(time.Hour), agentID, now,
	)
	if !errors.Is(err, verificationcall.ErrInvalid) {
		t.Errorf("expected ErrInvalid, got %v", err)
	}
}

func TestNew_NonBusyOutcomeWithWindow_Rejected(t *testing.T) {
	t.Parallel()
	_, err := verificationcall.New(
		callID, contactID, verificationcall.OutcomeVerified, "",
		now.Add(time.Hour), now.Add(2*time.Hour), agentID, now,
	)
	if !errors.Is(err, verificationcall.ErrInvalid) {
		t.Errorf("expected ErrInvalid (non-busy with window), got %v", err)
	}
}

func TestNew_InvalidOutcome_Rejected(t *testing.T) {
	t.Parallel()
	_, err := verificationcall.New(
		callID, contactID, verificationcall.OutcomeCode("escalated"), "",
		time.Time{}, time.Time{}, agentID, now,
	)
	if !errors.Is(err, verificationcall.ErrInvalid) {
		t.Errorf("expected ErrInvalid, got %v", err)
	}
}

func TestNew_RejectsZeroFields(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		mut  func() (verificationcall.ID, unverifiedcontact.ID, unverifiedcontact.MembershipID)
	}{
		{"empty id", func() (verificationcall.ID, unverifiedcontact.ID, unverifiedcontact.MembershipID) {
			return "", contactID, agentID
		}},
		{"empty contact id", func() (verificationcall.ID, unverifiedcontact.ID, unverifiedcontact.MembershipID) {
			return callID, "", agentID
		}},
		{"empty agent id", func() (verificationcall.ID, unverifiedcontact.ID, unverifiedcontact.MembershipID) {
			return callID, contactID, ""
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			id, cid, aid := tc.mut()
			_, err := verificationcall.New(
				id, cid, verificationcall.OutcomeNoAnswer, "",
				time.Time{}, time.Time{}, aid, now,
			)
			if !errors.Is(err, verificationcall.ErrInvalid) {
				t.Errorf("expected ErrInvalid, got %v", err)
			}
		})
	}
}
