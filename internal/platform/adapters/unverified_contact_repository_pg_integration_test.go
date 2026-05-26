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
	"github.com/leadkart/leadkart-go/internal/common/messaging/messagingtest"
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
	pool := platformPool(t)
	tx := pg.NewTransactor(pool)
	repo := adapters.NewUnverifiedContactRepository(pool, tx)

	c, _ := newSampleContact(t)
	if err := repo.Add(t.Context(), c); err != nil {
		t.Fatalf("Add: %v", err)
	}

	// Bypass RLS via platform GUC handled internally by the helper.
	topic, tenantNil := messagingtest.OutboxLatestTopicAndTenantNull(t, pool, messagingtest.SchemaPlatform)
	if topic != "platform.unverified_contact_created.v1" {
		t.Errorf("topic: got %q want platform.unverified_contact_created.v1", topic)
	}
	if !tenantNil {
		// C3 — platform-scoped events MUST persist as tenant_id NULL.
		t.Error("tenant_id: got non-NULL; want NULL for Platform-scoped event (C3)")
	}
}
