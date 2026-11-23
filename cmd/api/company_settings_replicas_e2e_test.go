//go:build integration

// arch-test:no-timeout-needed — newE2EFixture → startWiredPostgresForHTTP uses
// context.WithTimeout(90s) internally; per-request HTTP uses t.Context(), and
// every wait below is bounded by waitFor.
//
// arch-test:no-synctest — the proof runs the real outbox forwarder and router
// against a real Postgres; virtual time cannot model that.

// The G1 proof (ADR 0070): a company settings change reaches BOTH replicas
// (Orders and Inventory) through the real outbox, forwarder and router; a stale
// save is a 409 that changes nothing anywhere; a replayed or re-announced event
// is harmless; a replica that missed events converges on the next announcement;
// a tenant gets its first policy through the normal path.

package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/ThreeDotsLabs/watermill"
	"github.com/ThreeDotsLabs/watermill/components/cqrs"
	"github.com/ThreeDotsLabs/watermill/pubsub/gochannel"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/leadkart/leadkart-go/internal/common/audit"
	"github.com/leadkart/leadkart-go/internal/common/commercepolicy"
	"github.com/leadkart/leadkart-go/internal/common/messaging"
	"github.com/leadkart/leadkart-go/internal/common/pg"
	companysettingsadapters "github.com/leadkart/leadkart-go/internal/companysettings/adapters"
	companysettingscommand "github.com/leadkart/leadkart-go/internal/companysettings/app/command"
	csports "github.com/leadkart/leadkart-go/internal/companysettings/ports"
	companysettingssubscribers "github.com/leadkart/leadkart-go/internal/companysettings/ports/subscribers"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
	inventoryadapters "github.com/leadkart/leadkart-go/internal/inventory/adapters"
	inventorycommand "github.com/leadkart/leadkart-go/internal/inventory/app/command"
	inventorysubscribers "github.com/leadkart/leadkart-go/internal/inventory/ports/subscribers"
	ordersadapters "github.com/leadkart/leadkart-go/internal/orders/adapters"
	orderscommand "github.com/leadkart/leadkart-go/internal/orders/app/command"
	orderssubscribers "github.com/leadkart/leadkart-go/internal/orders/ports/subscribers"
)

// policyStack is the production event path for company settings: the outbox
// forwarder and routers with the subscribers cmd/worker registers (the
// tenant-registered seed, the Orders replica, the Inventory replica), plus read
// access to both replicas. The Orders subscriber can start late, which is how
// the test makes Orders miss events.
type policyStack struct {
	orders    *ordersadapters.PolicyReplicaRepository
	inventory *inventoryadapters.PolicyReplicaRepository
	pool      *pgxpool.Pool
	pubsub    *gochannel.GoChannel
	tx        *pg.Transactor
}

// runRouter starts a router hosting handlers on the shared pub/sub and stops it
// with the test.
func (s policyStack) runRouter(t *testing.T, handlers []cqrs.EventHandler) {
	t.Helper()
	log := silentLogger()
	router, err := messaging.NewRouter(messaging.Deps{
		Subscriber:       s.pubsub,
		Publisher:        s.pubsub,
		Logger:           log,
		IdempotencyInbox: messaging.NewIdempotentReceiver(s.pool),
		AuditWriter:      audit.NewWriter(s.pool, log, time.Now),
		DeadLetters:      messaging.NewDeadLetterWriter(s.pool, log, time.Now),
		CloseTimeout:     2 * time.Second,
		Retry:            messaging.RetryConfig{MaxRetries: 1, InitialInterval: 10 * time.Millisecond, MaxInterval: 50 * time.Millisecond, Multiplier: 2.0},
	})
	if err != nil {
		t.Fatalf("messaging.NewRouter: %v", err)
	}
	ep, err := messaging.NewEventProcessor(router.RawRouter(), s.pubsub, watermill.NewSlogLogger(log))
	if err != nil {
		t.Fatalf("messaging.NewEventProcessor: %v", err)
	}
	for _, h := range handlers {
		if err := router.AddCqrsHandler(ep, h); err != nil {
			t.Fatalf("AddCqrsHandler: %v", err)
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := router.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
			t.Errorf("router.Run: %v", err)
		}
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Error("router did not stop within 3s")
		}
	})
	select {
	case <-router.Running():
	case <-time.After(2 * time.Second):
		t.Fatal("router did not start within 2s")
	}
}

// ordersHandlers is the Orders subscriber, built the way cmd/worker builds it.
func (s policyStack) ordersHandlers() []cqrs.EventHandler {
	return orderssubscribers.Handlers(nil, nil, nil, nil,
		orderssubscribers.NewPolicyChangedSubscriber(orderscommand.NewApplyPolicyHandler(s.tx, s.orders), silentLogger()))
}

// startPolicyStack starts the forwarder and a router with the seed and Inventory
// subscribers, and the Orders one too unless ordersLate.
func startPolicyStack(t *testing.T, pool *pgxpool.Pool, ordersLate bool) policyStack {
	t.Helper()
	log := silentLogger()
	tx := pg.NewTransactor(pool)
	s := policyStack{
		orders:    ordersadapters.NewPolicyReplicaRepository(pool, tx),
		inventory: inventoryadapters.NewPolicyReplicaRepository(pool, tx),
		pool:      pool,
		pubsub:    gochannel.NewGoChannel(gochannel.Config{}, watermill.NewSlogLogger(log)),
		tx:        tx,
	}
	t.Cleanup(func() { _ = s.pubsub.Close() })

	settings := companysettingsadapters.NewSettingsRepository(pool, tx)
	var handlers []cqrs.EventHandler
	handlers = append(handlers, companysettingssubscribers.Handlers(
		companysettingssubscribers.NewTenantRegisteredSubscriber(companysettingscommand.NewSeedSettingsHandler(settings, time.Now), log))...)
	handlers = append(handlers, inventorysubscribers.Handlers(nil, nil, nil,
		inventorysubscribers.NewPolicyChangedSubscriber(inventorycommand.NewApplyPolicyHandler(tx, s.inventory), log))...)
	if !ordersLate {
		handlers = append(handlers, s.ordersHandlers()...)
	}
	s.runRouter(t, handlers)

	forwarder, err := messaging.NewOutboxForwarder(pool, s.pubsub, watermill.NewSlogLogger(log))
	if err != nil {
		t.Fatalf("messaging.NewOutboxForwarder: %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	go func() {
		if err := forwarder.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
			t.Logf("forwarder: %v", err)
		}
	}()
	t.Cleanup(func() { _ = forwarder.Close() })
	return s
}

// waitFor polls cond until it holds, failing after 15 seconds with what.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(25 * time.Millisecond) // arch-test:wait-justified — poll interval inside the 15s deadline-bounded loop awaiting the async forwarder -> router -> subscriber path
	}
	t.Fatalf("timed out waiting for %s", what)
}

// replicasAt returns the Orders replica's policy and whether BOTH replicas hold
// exactly version v with identical policies.
func (s policyStack) replicasAt(t *testing.T, tid string, v int64) (commercepolicy.Versioned, bool) {
	t.Helper()
	o, err := s.orders.ReadPolicy(t.Context(), tenant.ID(tid))
	if err != nil {
		t.Fatalf("orders replica: %v", err)
	}
	i, err := s.inventory.ReadPolicy(t.Context(), tenant.ID(tid))
	if err != nil {
		t.Fatalf("inventory replica: %v", err)
	}
	return o, o.Version == v && i.Version == v && equalPolicies(t, o.Policy, i.Policy)
}

func equalPolicies(t *testing.T, a, b commercepolicy.Policy) bool {
	t.Helper()
	ra, errA := commercepolicy.Encode(a)
	rb, errB := commercepolicy.Encode(b)
	if errA != nil || errB != nil {
		t.Fatalf("encode: %v %v", errA, errB)
	}
	return string(ra) == string(rb)
}

func TestE2E_CompanySettings_AChangeReachesBothReplicasAndReplayIsHarmless(t *testing.T) {
	f := newE2EFixture(t)
	// Orders starts LATE: it misses the first versions, as if their events had dead-lettered.
	stack := startPolicyStack(t, f.Pool, true)
	reg := f.registerAndLogin(t, "cs-replicas")
	tok := reg.AccessToken
	tid := tenant.ID(reg.TenantID)
	inventoryAt := func(v int64) bool {
		got, err := stack.inventory.ReadPolicy(t.Context(), tid)
		return err == nil && got.Version == v
	}

	// 1. A new tenant gets its policy through the normal path: version 1 (history
	//    row, policy_changed event), and the replica that is listening learns it.
	waitFor(t, "the seeded version 1 in Inventory's replica", func() bool { return inventoryAt(1) })
	seeded := csDecode[csports.SettingsDto](t, f.authedJSON(t, http.MethodGet, csSettings, tok, nil), http.StatusOK)
	if seeded.Version != 1 {
		t.Fatalf("companysettings version = %d, want 1 (seeded at registration)", seeded.Version)
	}
	csDecode[csports.SettingsDto](t, f.authedJSON(t, http.MethodPatch, csSettings, tok,
		csPatch(1, "", map[string]map[string]any{"expiry": {"stop_months_before": 2}})), http.StatusOK)
	waitFor(t, "version 2 in Inventory's replica", func() bool { return inventoryAt(2) })
	if v, _ := stack.orders.ReadPolicy(t.Context(), tid); v.Version != 0 || !equalPolicies(t, v.Policy, commercepolicy.Default()) {
		t.Fatalf("Orders was not listening yet: its replica must read the defaults at version 0, got v%d", v.Version)
	}

	// 2. Anti-entropy: Orders starts listening and the daily job re-announces the
	//    current version; the replica that missed everything converges.
	stack.runRouter(t, stack.ordersHandlers())
	companySettings := buildCompanySettingsApp(f.Pool, nil)
	republish := func() {
		t.Helper()
		if err := companySettings.Commands.Republish.Handle(t.Context(), companysettingscommand.RepublishCommand{TenantID: tid}); err != nil {
			t.Fatalf("republish: %v", err)
		}
	}
	republish()
	waitFor(t, "both replicas at version 2 after the republish", func() bool { _, ok := stack.replicasAt(t, reg.TenantID, 2); return ok })

	// 3. A change reaches BOTH replicas, with the whole policy.
	patched := csDecode[csports.SettingsDto](t, f.authedJSON(t, http.MethodPatch, csSettings, tok,
		csPatch(2, "", map[string]map[string]any{"expiry": {"stop_months_before": 3}, "credit": {"enabled": true}})), http.StatusOK)
	if patched.Version != 3 {
		t.Fatalf("patched version = %d", patched.Version)
	}
	waitFor(t, "version 3 in both replicas", func() bool { _, ok := stack.replicasAt(t, reg.TenantID, 3); return ok })
	got, _ := stack.replicasAt(t, reg.TenantID, 3)
	if got.Policy.Expiry.StopMonthsBefore != 3 || !got.Policy.Credit.Enabled {
		t.Fatalf("replica policy = %+v", got.Policy.Expiry)
	}

	// 4. A stale save is a 409 and nothing moves: not the policy, not either replica.
	stale := f.authedJSON(t, http.MethodPatch, csSettings, tok,
		csPatch(2, "", map[string]map[string]any{"expiry": {"stop_months_before": 9}}))
	if stale.status != http.StatusConflict || errorCode(t, stale.body) != "settings_version_conflict" {
		t.Fatalf("stale save: %d %s", stale.status, stale.body)
	}
	time.Sleep(300 * time.Millisecond) // arch-test:wait-justified — negative proof: a wrongly emitted event would need the real forwarder and router to arrive; there is nothing to poll for
	if v, ok := stack.replicasAt(t, reg.TenantID, 3); !ok || v.Policy.Expiry.StopMonthsBefore != 3 {
		t.Fatalf("a stale save reached a replica: %+v", v)
	}

	// 5. The GST list is part of the policy: adding a rate reaches both replicas too.
	rate := csDecode[csports.GSTRateChangedDto](t, f.authedJSON(t, http.MethodPost, csRates, tok,
		map[string]any{"rate_bps": 500, "label": "GST 5%", "confirmed_by": "A. Mehta (CA)", "confirmed_on": "2026-10-30", "reason": "per CA"}), http.StatusCreated)
	waitFor(t, "version 4 with the rate in both replicas", func() bool { _, ok := stack.replicasAt(t, reg.TenantID, 4); return ok })
	got, _ = stack.replicasAt(t, reg.TenantID, 4)
	if len(got.Policy.Tax.GSTRates) != 1 || got.Policy.Tax.GSTRates[0].ID != rate.ID {
		t.Fatalf("replica rates = %+v", got.Policy.Tax.GSTRates)
	}

	// 6. Replay is harmless: the job re-announces the current version twice more
	//    (the same alias, flagged); the replicas stay at 4, unchanged, and the
	//    companysettings history gains no row.
	before := csDecode[csports.ListHistoryResponse](t, f.authedJSON(t, http.MethodGet, csHistory, tok, nil), http.StatusOK)
	republish()
	republish()
	time.Sleep(500 * time.Millisecond) // arch-test:wait-justified — negative proof: a re-announced event must have been delivered and ignored; there is nothing to poll for
	if _, ok := stack.replicasAt(t, reg.TenantID, 4); !ok {
		t.Fatal("a re-announced version disturbed a replica")
	}
	after := csDecode[csports.ListHistoryResponse](t, f.authedJSON(t, http.MethodGet, csHistory, tok, nil), http.StatusOK)
	if len(after.Items) != len(before.Items) || after.Items[0].Version != 4 {
		t.Fatalf("a republish wrote history: %d -> %d rows", len(before.Items), len(after.Items))
	}

	// 7. A re-announcement is not a change: it has its own alias and the tenant
	//    audit timeline does not list it (review M2). Real changes are listed.
	timeline := f.authedJSON(t, http.MethodGet, "/api/v1/tenants/"+reg.TenantID+"/audit/events?page_size=200", tok, nil)
	if timeline.status != http.StatusOK {
		t.Fatalf("tenant audit timeline: %d %s", timeline.status, timeline.body)
	}
	var events struct {
		Events []struct {
			Action string `json:"action"`
		} `json:"events"`
	}
	if err := json.Unmarshal(timeline.body, &events); err != nil {
		t.Fatal(err)
	}
	changed := 0
	for _, e := range events.Events {
		if strings.Contains(e.Action, "republished") {
			t.Fatalf("the timeline lists a republish: %s", e.Action)
		}
		if e.Action == "companysettings.policy_changed.v1" {
			changed++
		}
	}
	if changed == 0 {
		t.Fatalf("real changes must still be listed: %s", timeline.body)
	}
}
