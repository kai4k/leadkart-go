//go:build integration

package adapters_test

import (
	"context"
	"errors"
	"testing"

	"github.com/leadkart/leadkart-go/internal/common/ids"
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

// withPlatformGUC drives the connection under app.is_platform=true so
// the Platform-only RLS policies pass. Mirror of identity's pattern.
func withPlatformGUC(ctx context.Context) context.Context {
	return ctx // tests pass the ctx through pg.Transactor which sets the GUC for TxScopePlatform
}

// TestUnverifiedContactRepository_Add_RoundTripsViaGetByID — write +
// read shape under RLS. Confirms the sqlc INSERT param shape + the
// reader code path return logically equivalent aggregates.
func TestUnverifiedContactRepository_Add_RoundTripsViaGetByID(t *testing.T) {
	pool := platformPool(t)
	tx := pg.NewTransactor(pool)
	repo := adapters.NewUnverifiedContactRepository(pool, tx)

	// Add runs under TxScopePlatform (platform-only table; RLS would
	// reject a tenant scope). The repo's Add method opens its own tx
	// when ctx carries none.
	c, _ := newSampleContact(t)
	if err := repo.Add(withPlatformGUC(t.Context()), c); err != nil {
		t.Fatalf("Add: %v", err)
	}

	// GetByID without an active tx — reader uses pool directly.
	// Under leadkart_app role + RLS, the SELECT only returns rows
	// when app.is_platform()=true (set on the connection's TX-bound
	// GUC). We surface the GUC by wrapping the read in a manual
	// platform-scoped tx.
	err := tx.WithinTx(t.Context(), pg.TxScopePlatform, func(ctx context.Context) error {
		got, err := repo.GetByID(ctx, c.ID())
		if err != nil {
			return err
		}
		if got.ID() != c.ID() {
			t.Errorf("ID round-trip: got %q want %q", got.ID(), c.ID())
		}
		if got.State() != unverifiedcontact.StateNew {
			t.Errorf("State round-trip: got %q want new", got.State())
		}
		if got.Form().MobileE164() != "+919876543210" {
			t.Errorf("MobileE164 round-trip: got %q", got.Form().MobileE164())
		}
		return nil
	})
	if err != nil {
		t.Fatalf("read tx: %v", err)
	}
}

// TestUnverifiedContactRepository_GetByID_RetursErrNotFound — the typed
// sentinel propagation. Catches a regression where pgx.ErrNoRows gets
// surfaced raw instead of mapped to the domain sentinel.
func TestUnverifiedContactRepository_GetByID_ReturnsErrNotFound(t *testing.T) {
	pool := platformPool(t)
	tx := pg.NewTransactor(pool)
	repo := adapters.NewUnverifiedContactRepository(pool, tx)

	err := tx.WithinTx(t.Context(), pg.TxScopePlatform, func(ctx context.Context) error {
		_, err := repo.GetByID(ctx, unverifiedcontact.ID(ids.NewV7().String()))
		if !errors.Is(err, unverifiedcontact.ErrNotFound) {
			t.Errorf("expected ErrNotFound, got %v", err)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("tx: %v", err)
	}
}

// TestUnverifiedContactRepository_Add_DrainsCreatedEventToOutbox — C3
// + general outbox shape. After Add, the platform.outbox row exists
// with the canonical topic + tenant_id IS NULL (Platform-scoped
// event per ADR 0059 + migration 20260601000002).
func TestUnverifiedContactRepository_Add_DrainsCreatedEventToOutbox(t *testing.T) {
	pool := platformPool(t)
	tx := pg.NewTransactor(pool)
	repo := adapters.NewUnverifiedContactRepository(pool, tx)

	c, _ := newSampleContact(t)
	if err := repo.Add(t.Context(), c); err != nil {
		t.Fatalf("Add: %v", err)
	}

	rawDB, err := openRawDB(t, pool)
	if err != nil {
		t.Fatalf("openRawDB: %v", err)
	}
	defer rawDB.Close()
	// Bypass RLS via platform GUC for the verification read.
	if _, err := rawDB.ExecContext(t.Context(), `SELECT set_config('app.is_platform','true',false)`); err != nil {
		t.Fatalf("set platform: %v", err)
	}
	var (
		topic     string
		tenantNil bool
	)
	err = rawDB.QueryRowContext(t.Context(), `
		SELECT topic, (tenant_id IS NULL) FROM platform.outbox
		ORDER BY created_at DESC LIMIT 1
	`).Scan(&topic, &tenantNil)
	if err != nil {
		t.Fatalf("query outbox: %v", err)
	}
	if topic != "platform.unverified_contact_created.v1" {
		t.Errorf("topic: got %q want platform.unverified_contact_created.v1", topic)
	}
	if !tenantNil {
		// C3 — platform-scoped events MUST persist as tenant_id NULL.
		t.Error("tenant_id: got non-NULL; want NULL for Platform-scoped event (C3)")
	}
}
