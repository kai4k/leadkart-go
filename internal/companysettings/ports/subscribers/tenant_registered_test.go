package subscribers_test

import (
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/leadkart/leadkart-go/internal/companysettings/app/command"
	"github.com/leadkart/leadkart-go/internal/companysettings/domain/commercesettings/commercesettingstest"
	"github.com/leadkart/leadkart-go/internal/companysettings/ports/subscribers"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
	identityevents "github.com/leadkart/leadkart-go/internal/identity/integrationevents"
)

func TestTenantRegisteredSubscriber_SeedsVersionOneOnceAndReplayIsHarmless(t *testing.T) {
	t.Parallel()
	repo := commercesettingstest.NewFakeRepository()
	now := func() time.Time { return time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC) }
	sub := subscribers.NewTenantRegisteredSubscriber(command.NewSeedSettingsHandler(repo, now),
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	tid := uuid.New()
	evt := &identityevents.TenantRegisteredV1{TenantID: tid, Slug: "acme", OccurredAtUTC: now()}

	for range 3 {
		if err := sub.Handle(t.Context(), evt); err != nil {
			t.Fatalf("Handle: %v", err)
		}
	}
	stored := repo.Store[tenant.ID(tid.String())]
	if stored.Version != 1 {
		t.Fatalf("a registered tenant gets version 1 through the normal path, got %d", stored.Version)
	}
	if len(repo.DrainedEvents) != 1 {
		t.Fatalf("a replay must emit nothing more: %d events", len(repo.DrainedEvents))
	}
}

func TestHandlers_RegistersTheSeedHandlerUnderItsStableName(t *testing.T) {
	t.Parallel()
	sub := subscribers.NewTenantRegisteredSubscriber(
		command.NewSeedSettingsHandler(commercesettingstest.NewFakeRepository(), time.Now), slog.New(slog.NewTextHandler(io.Discard, nil)))
	hs := subscribers.Handlers(sub)
	if len(hs) != 1 || hs[0].HandlerName() != "companysettings.subscribers.SeedSettings" {
		t.Fatalf("handlers = %v", hs)
	}
	if len(subscribers.Handlers(nil)) != 0 {
		t.Fatal("a nil subscriber is skipped")
	}
}
