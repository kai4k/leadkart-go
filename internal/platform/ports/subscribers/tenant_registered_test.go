package subscribers_test

import (
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"

	identityevents "github.com/leadkart/leadkart-go/internal/identity/integrationevents"
	"github.com/leadkart/leadkart-go/internal/platform/app/command"
	"github.com/leadkart/leadkart-go/internal/platform/domain/leadcredit"
	"github.com/leadkart/leadkart-go/internal/platform/platformtest"
	"github.com/leadkart/leadkart-go/internal/platform/ports/subscribers"
)

// silentLog returns a no-output *slog.Logger for tests — required by
// subscriber constructors per Mat Ryer canon (no nil-fallback).
func silentLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func buildHandler(t *testing.T) (*subscribers.TenantRegisteredIngestor, *platformtest.FakeLeadCreditRepository) {
	t.Helper()
	credits := platformtest.NewFakeLeadCreditRepository()
	uow := platformtest.NewFakeUnitOfWork(credits)
	cmd := command.NewInitialiseLeadCreditsHandler(uow, credits, func() time.Time {
		return time.Date(2026, 5, 26, 12, 0, 0, 0, time.UTC)
	})
	return subscribers.NewTenantRegisteredIngestor(cmd, silentLog()), credits
}

// Post-cqrs (ADR 0067): the handler receives the already-decoded typed event;
// topic routing + payload decode are the EventProcessor's job, so the old
// wrong-topic + malformed-payload unit cases are gone.

func validEvent(tenantID uuid.UUID) *identityevents.TenantRegisteredV1 {
	return &identityevents.TenantRegisteredV1{
		TenantID:      tenantID,
		Slug:          "acme",
		LegalName:     "Acme Pharma Pvt Ltd",
		DisplayName:   "Acme",
		AdminEmail:    "owner@acme.test",
		OccurredAtUTC: time.Now().UTC(), // arch-test:wall-clock -- wire-envelope fixture; subscriber doesn't pin timestamps.
	}
}

func TestTenantRegisteredIngestor_HappyPath(t *testing.T) {
	t.Parallel()
	h, credits := buildHandler(t)
	tenantID := uuid.New()

	if err := h.Handle(t.Context(), validEvent(tenantID)); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	stored, err := credits.GetByTenant(t.Context(), leadcredit.TenantID(tenantID.String()))
	if err != nil {
		t.Fatalf("GetByTenant: %v", err)
	}
	if stored.Balance() != 0 {
		t.Errorf("Balance=%d want 0", stored.Balance())
	}
}

func TestTenantRegisteredIngestor_IdempotentOnReplay(t *testing.T) {
	t.Parallel()
	h, credits := buildHandler(t)
	evt := validEvent(uuid.New())

	if err := h.Handle(t.Context(), evt); err != nil {
		t.Fatalf("first: %v", err)
	}
	if err := h.Handle(t.Context(), evt); err != nil {
		t.Fatalf("replay: %v", err)
	}
	// Still ONE row.
	if got := len(credits.Store); got != 1 {
		t.Errorf("Store entries: %d want 1", got)
	}
}
