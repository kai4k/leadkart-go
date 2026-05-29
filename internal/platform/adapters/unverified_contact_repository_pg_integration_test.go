//go:build integration

// arch-test:no-timeout-needed — every test in this file uses the shared
//   pgtest container (per-package); pgxpool internal conn timeouts +
//   package-level `task ci:test:int -timeout=15m` already bound execution.
//   Per-test context.WithTimeout would be belt-and-suspenders against the
//   shared-pool + parallel-with-RLS canon shape.
//
// SQL-CONTRACT COVERAGE (per ADR 0062 — TDL Test Pyramid):
//   - Outbox row insertion in the SAME tx as the aggregate write
//     (ADR 0008); confirms tenant_id IS NULL for Platform-scoped events
//     per ADR 0059 + C3.
//
// Round-trip Add/GetByID + ErrNotFound coverage moved to
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

// TestUnverifiedContactRepository_Add_DrainsCreatedEventToOutbox —
// SQL-contract (C3 + general outbox shape). After Add, the
// platform.outbox row MUST exist with the canonical topic + tenant_id
// IS NULL (Platform-scoped event per ADR 0059 + migration
// 20260601000002).
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

	// Production forwarder drains + the in-process subscriber receives the
	// event. Strict TDL canon per ADR 0062 Amendment 1.
	msgs := fix.forwardAndWait(t, 1)
	got := platformEventTypes(msgs)
	if len(got) != 1 || got[0] != "platform.unverified_contact_created.v1" {
		t.Fatalf("event_types: got %v want [platform.unverified_contact_created.v1]", got)
	}
	// C3 — platform-scoped events persist as tenant_id NULL, so the
	// forwarder OMITS the tenant_id metadata header (only set when the
	// row carries a real tenant FK). Empty header == NULL on the wire.
	if tid := msgs[0].Metadata.Get(messaging.HeaderTenantID); tid != "" {
		t.Errorf("tenant_id header: got %q; want empty for Platform-scoped event (C3 — tenant_id NULL)", tid)
	}
}
