package leadcredit_test

import (
	"errors"
	"testing"
	"time"

	"github.com/leadkart/leadkart-go/internal/platform/domain/leadcredit"
)

var (
	tenantA  = leadcredit.TenantID("01900000-0000-7000-8000-000000000200")
	operator = leadcredit.MembershipID("01900000-0000-7000-8000-000000000001")
	now      = time.Date(2026, 6, 1, 10, 0, 0, 0, time.UTC)
)

func TestNewForTenant_HappyPath(t *testing.T) {
	t.Parallel()
	l, err := leadcredit.NewForTenant(tenantA, now)
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if l.Balance() != 0 {
		t.Errorf("balance=%d", l.Balance())
	}
	if l.Version() != 0 {
		t.Errorf("version=%d", l.Version())
	}
	if evs := l.PullEvents(); len(evs) != 0 {
		t.Errorf("ctor should not emit events, got %d", len(evs))
	}
}

func TestNewForTenant_RejectsZero(t *testing.T) {
	t.Parallel()
	if _, err := leadcredit.NewForTenant("", now); !errors.Is(err, leadcredit.ErrInvalid) {
		t.Errorf("expected ErrInvalid, got %v", err)
	}
}

func TestTopup_HappyPath(t *testing.T) {
	t.Parallel()
	l, _ := leadcredit.NewForTenant(tenantA, now)
	if err := l.Topup(100, "Q2 marketing budget", operator, now); err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if l.Balance() != 100 {
		t.Errorf("balance=%d", l.Balance())
	}
	evs := l.PullEvents()
	if len(evs) != 1 {
		t.Fatalf("expected 1 event, got %d", len(evs))
	}
	ae, ok := evs[0].(leadcredit.AdjustedEvent)
	if !ok {
		t.Fatalf("expected AdjustedEvent, got %T", evs[0])
	}
	if ae.Delta != 100 || ae.NewBalance != 100 {
		t.Errorf("event=%+v", ae)
	}
}

func TestTopup_RejectsNonPositive(t *testing.T) {
	t.Parallel()
	l, _ := leadcredit.NewForTenant(tenantA, now)
	for _, d := range []int64{0, -1, -100} {
		err := l.Topup(d, "test", operator, now)
		if !errors.Is(err, leadcredit.ErrInvalid) {
			t.Errorf("delta %d: expected ErrInvalid, got %v", d, err)
		}
	}
}

func TestTopup_RequiresReason(t *testing.T) {
	t.Parallel()
	l, _ := leadcredit.NewForTenant(tenantA, now)
	if err := l.Topup(100, "  ", operator, now); !errors.Is(err, leadcredit.ErrInvalid) {
		t.Errorf("expected ErrInvalid, got %v", err)
	}
}

func TestCharge_HappyPath(t *testing.T) {
	t.Parallel()
	l, _ := leadcredit.NewForTenant(tenantA, now)
	_ = l.Topup(100, "init", operator, now) // arch-test:ignore-err — domain test seed
	_ = l.PullEvents()
	if err := l.Charge(30, "Marketplace purchase: lead 0190", operator, now.Add(time.Hour)); err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if l.Balance() != 70 {
		t.Errorf("balance=%d", l.Balance())
	}
	evs := l.PullEvents()
	if len(evs) != 1 {
		t.Fatalf("expected 1 event, got %d", len(evs))
	}
	ae := evs[0].(leadcredit.AdjustedEvent)
	if ae.Delta != -30 || ae.NewBalance != 70 {
		t.Errorf("event=%+v", ae)
	}
}

func TestCharge_RejectsInsufficientBalance(t *testing.T) {
	t.Parallel()
	l, _ := leadcredit.NewForTenant(tenantA, now)
	_ = l.Topup(10, "init", operator, now) // arch-test:ignore-err — domain test seed
	err := l.Charge(100, "Marketplace purchase", operator, now.Add(time.Hour))
	if !errors.Is(err, leadcredit.ErrInsufficientBalance) {
		t.Errorf("expected ErrInsufficientBalance, got %v", err)
	}
}

func TestCharge_RejectsNonPositiveAmount(t *testing.T) {
	t.Parallel()
	l, _ := leadcredit.NewForTenant(tenantA, now)
	for _, a := range []int64{0, -1} {
		err := l.Charge(a, "x", operator, now)
		if !errors.Is(err, leadcredit.ErrInvalid) {
			t.Errorf("amount %d: expected ErrInvalid, got %v", a, err)
		}
	}
}

func TestUnmarshalFromDB_RoundTrip(t *testing.T) {
	t.Parallel()
	snap := leadcredit.Snapshot{
		TenantID:  tenantA,
		Balance:   500,
		Version:   3,
		CreatedAt: now,
		UpdatedAt: now.Add(time.Hour),
	}
	l := leadcredit.UnmarshalFromDB(snap)
	if l.Balance() != 500 || l.Version() != 3 {
		t.Errorf("round-trip lost data: balance=%d version=%d", l.Balance(), l.Version())
	}
}
