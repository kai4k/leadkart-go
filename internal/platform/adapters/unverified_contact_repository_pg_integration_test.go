//go:build integration

// arch-test:no-timeout-needed — shared pgtest container + pgxpool conn
//   timeouts + package-level `task ci:test:int -timeout=15m` already bound
//   execution.
//
// SQL-contract coverage (ADR 0062, TDL Test Pyramid):
//   - Outbox row written in the aggregate's tx (ADR 0008); tenant_id IS NULL
//     for platform-scoped events (ADR 0059, C3).
//
// Round-trip + ErrNotFound coverage lives in
// [unverifiedcontacttest.FakeRepository].

package adapters_test

import (
	"context"
	"testing"
	"time"

	"github.com/leadkart/leadkart-go/internal/common/ids"
	"github.com/leadkart/leadkart-go/internal/common/messaging"
	"github.com/leadkart/leadkart-go/internal/common/pg"
	"github.com/leadkart/leadkart-go/internal/platform/adapters"
	"github.com/leadkart/leadkart-go/internal/platform/domain/unverifiedcontact"
)

func newSampleContact(t *testing.T) (*unverifiedcontact.UnverifiedContact, unverifiedcontact.MembershipID) {
	t.Helper()
	agentID := unverifiedcontact.MembershipID(ids.NewV7().String())
	cID := unverifiedcontact.ID(ids.NewV7().String())
	c, err := unverifiedcontact.New(cID, fixtureForm(t), agentID, nowUTC())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c, agentID
}

// TestUnverifiedContactRepository_Add_DrainsCreatedEventToOutbox asserts Add
// writes the outbox row with the canonical topic and tenant_id NULL
// (platform-scoped; ADR 0059, C3).
func TestUnverifiedContactRepository_Add_DrainsCreatedEventToOutbox(t *testing.T) {
	// arch-test:no-parallel — cross-tenant scan; uses TruncateAll
	sharedPG.TruncateAll(t)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	_ = ctx // arch-test:integration-timeout-anchor
	fix := newPlatformOutboxFixture(t)
	repo := adapters.NewUnverifiedContactRepository(fix.pool, pg.NewTransactor(fix.pool))

	c, _ := newSampleContact(t)
	if err := repo.Add(t.Context(), c); err != nil {
		t.Fatalf("Add: %v", err)
	}

	msgs := fix.forwardAndWait(t, 1)
	got := platformEventTypes(msgs)
	if len(got) != 1 || got[0] != "platform.unverified_contact_created.v1" {
		t.Fatalf("event_types: got %v want [platform.unverified_contact_created.v1]", got)
	}
	// C3: platform-scoped events persist as tenant_id NULL, so the producer
	// omits the tenant_id header (empty header == NULL on the wire).
	if tid := msgs[0].Metadata.Get(messaging.HeaderTenantID); tid != "" {
		t.Errorf("tenant_id header: got %q; want empty for Platform-scoped event (C3 — tenant_id NULL)", tid)
	}
}
