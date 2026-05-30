//go:build integration

// arch-test:no-timeout-needed — every test in this file uses the shared
//   pgtest container (per-package); pgxpool internal conn timeouts +
//   package-level `task ci:test:int -timeout=15m` already bound execution.
//   Per-test context.WithTimeout would be belt-and-suspenders against the
//   shared-pool + parallel-with-RLS canon shape.
//
// arch-test:parallel-safe — every Test* uses the shared pgtest container
//   + fresh tenant/person/family IDs per test; RLS isolates rows by
//   tenant so parallel runs cannot see each others state. Per-test
//   miniredis (in-memory) prevents cache pollution. Brandur Postgres
//   canon + TDL Wild Workouts canon.
//
// arch-test:no-synctest — subscriber tests exercise the Watermill router
// goroutine against a real Postgres testcontainer; the polled families
// row signals subscriber commit across the SQL driver boundary, which is
// opaque to testing/synctest's virtual clock.

package subscribers_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/ThreeDotsLabs/watermill"
	"github.com/ThreeDotsLabs/watermill/message"
	"github.com/ThreeDotsLabs/watermill/pubsub/gochannel"
	"github.com/alicebob/miniredis/v2"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"github.com/leadkart/leadkart-go/internal/common/audit"
	"github.com/leadkart/leadkart-go/internal/common/audit/audittest"
	"github.com/leadkart/leadkart-go/internal/common/cache"
	"github.com/leadkart/leadkart-go/internal/common/email"
	"github.com/leadkart/leadkart-go/internal/common/ids"
	"github.com/leadkart/leadkart-go/internal/common/messaging"
	"github.com/leadkart/leadkart-go/internal/common/pg"
	"github.com/leadkart/leadkart-go/internal/common/slug"
	"github.com/leadkart/leadkart-go/internal/identity/adapters"
	"github.com/leadkart/leadkart-go/internal/identity/domain/person"
	"github.com/leadkart/leadkart-go/internal/identity/domain/refreshtoken"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
	"github.com/leadkart/leadkart-go/internal/identity/integrationevents"
	"github.com/leadkart/leadkart-go/internal/identity/ports/subscribers"
)

// testNow is the deterministic instant test fixtures pass to domain
// factories + mutators per the clock-injection refactor.
var testNow = time.Date(2026, 5, 24, 12, 0, 0, 0, time.UTC)

// fixture spins ephemeral Postgres + applies migrations + provisions
// non-superuser leadkart_app role. Returns a pgxpool connected as
// leadkart_app so RLS actually fires for the tenant_memberships
// queries we DON'T touch but that other tests in the same fixture run
// would.
type fixture struct {
	pool       *pgxpool.Pool
	tenants    *adapters.TenantRepository
	persons    *adapters.PersonRepository
	families   *adapters.RefreshTokenFamilyRepository
	stampCache *adapters.SecurityStampCache
	miniredis  *miniredis.Miniredis
}

// newFixture returns a per-test fixture backed by the SHARED package-
// scoped Postgres pool (see fixture_integration_test.go TestMain).
// The pool is shared; the miniredis + cache facade + repository
// instances are per-test because they hold per-test in-memory state
// or are cheap to construct.
//
// Per-test isolation: each call creates aggregates under a fresh
// tenant.ID (via seedTenant) so RLS prevents cross-test reads.
func newFixture(t *testing.T) *fixture {
	t.Helper()
	pool := sharedPG.Pool()

	tx := pg.NewTransactor(pool)
	persons := adapters.NewPersonRepository(pool, tx)

	// Real HybridCache + SecurityStampCache against miniredis so the
	// cascade subscriber's invalidation step runs end-to-end against
	// the same facade production wires (audit-checklist.md §12b).
	// miniredis is in-memory + per-test (RunT auto-cleans on test end);
	// cheap (~10ms) — kept per-test to avoid cross-test cache pollution.
	store := miniredis.RunT(t)
	redisCli := redis.NewClient(&redis.Options{Addr: store.Addr()})
	t.Cleanup(func() { _ = redisCli.Close() })
	hc, err := cache.New(cache.Config{
		L1MaxItems: 1000,
		L2:         redisCli,
		Logger:     silentLog(),
	})
	if err != nil {
		t.Fatalf("cache.New: %v", err)
	}
	t.Cleanup(hc.Close)

	return &fixture{
		pool:       pool,
		tenants:    adapters.NewTenantRepository(pool, tx),
		persons:    persons,
		families:   adapters.NewRefreshTokenFamilyRepository(pool, tx),
		stampCache: adapters.NewSecurityStampCache(hc, persons),
		miniredis:  store,
	}
}

const fxStrongHash = "$argon2id$v=19$m=19456,t=2,p=1$c2FsdHkx$abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789"

func (fx *fixture) seedPerson(t *testing.T, addr string) *person.Person {
	t.Helper()
	e, err := email.New(addr)
	if err != nil {
		t.Fatalf("email: %v", err)
	}
	hash, err := person.NewPasswordHash(fxStrongHash)
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	p, err := person.New(person.ID(ids.NewV7().String()), e, "Alice", "Acme", hash, testNow)
	if err != nil {
		t.Fatalf("person.New: %v", err)
	}
	if err := fx.persons.Add(t.Context(), p); err != nil {
		t.Fatalf("Add person: %v", err)
	}
	return p
}

func (fx *fixture) seedTenant(t *testing.T) *tenant.Tenant {
	t.Helper()
	full := ids.NewV7().String()
	s, _ := slug.New("sub-" + full[len(full)-8:])
	addr, _ := email.New("admin@example.test")
	tn, err := tenant.New(tenant.ID(ids.NewV7().String()), s, "Acme Pharma Pvt Ltd", "Acme", addr, testNow)
	if err != nil {
		t.Fatalf("tenant.New: %v", err)
	}
	if err := fx.tenants.Add(t.Context(), tn); err != nil {
		t.Fatalf("Add tenant: %v", err)
	}
	return tn
}

func (fx *fixture) seedFamily(t *testing.T, p *person.Person, tn *tenant.Tenant) *refreshtoken.Family {
	t.Helper()
	hashStr := sha256.Sum256([]byte("seed-token-" + ids.NewV7().String()))
	hash, err := refreshtoken.NewTokenHash(hex.EncodeToString(hashStr[:]))
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	f, err := refreshtoken.NewFamily(
		refreshtoken.FamilyID(ids.NewV7().String()),
		p.ID(), tn.ID(), "iPhone 15 / Safari", hash, 14*24*time.Hour,
		time.Now().UTC(),
	)
	if err != nil {
		t.Fatalf("NewFamily: %v", err)
	}
	if err := fx.families.Add(t.Context(), f); err != nil {
		t.Fatalf("Add family: %v", err)
	}
	return f
}

func silentLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// safeBuffer is a goroutine-safe wrapper around bytes.Buffer used as the
// sink for slog handlers in tests where the assertion goroutine and a
// subscriber goroutine race on the same buffer. slog.Handler implementations
// don't serialise writes to the underlying io.Writer — that's the writer's
// job — so a bare *bytes.Buffer hits the race detector under -race.
type safeBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *safeBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *safeBuffer) Bytes() []byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	return bytes.Clone(b.buf.Bytes())
}

func (b *safeBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// runRouter starts a router goroutine + returns a stop fn.
func runRouter(t *testing.T, r *messaging.Router) func() {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := r.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
			t.Errorf("router run: %v", err)
		}
	}()
	select {
	case <-r.Running():
	case <-time.After(2 * time.Second):
		cancel()
		<-done
		t.Fatal("router did not start within 2s")
	}
	return func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("router did not stop within 5s")
		}
	}
}

// publishEvent serialises evt as the V1 wire payload + sets the
// canonical metadata headers. Returns the message UUID.
func publishEvent(t *testing.T, pubsub *gochannel.GoChannel, evt integrationevents.Event, tenantID uuid.UUID) string {
	t.Helper()
	payload, err := json.Marshal(evt)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	msgID := uuid.NewString()
	msg := message.NewMessage(msgID, bytes.Clone(payload))
	msg.Metadata.Set(messaging.HeaderEventType, evt.Topic())
	msg.Metadata.Set(messaging.HeaderTenantID, tenantID.String())
	msg.Metadata.Set(messaging.HeaderOccurredAt, evt.OccurredAt().UTC().Format(time.RFC3339Nano))
	if err := pubsub.Publish(integrationevents.Topic, msg); err != nil {
		t.Fatalf("publish: %v", err)
	}
	return msgID
}

func wireRouter(t *testing.T, fx *fixture) (*gochannel.GoChannel, *messaging.Router, func()) {
	t.Helper()
	pubsub := gochannel.NewGoChannel(gochannel.Config{}, watermill.NewSlogLogger(silentLog()))
	t.Cleanup(func() { _ = pubsub.Close() })

	receiver := messaging.NewIdempotentReceiver(fx.pool)
	auditW := audit.NewWriter(fx.pool, silentLog(), time.Now)
	router, err := messaging.NewRouter(messaging.Deps{
		Subscriber:       pubsub,
		Publisher:        pubsub,
		Logger:           silentLog(),
		IdempotencyInbox: receiver,
		AuditWriter:      auditW,
		DeadLetters:      messaging.NewDeadLetterWriter(fx.pool, silentLog(), time.Now),
		CloseTimeout:     2 * time.Second,
		Retry: messaging.RetryConfig{
			MaxRetries:      1,
			InitialInterval: 10 * time.Millisecond,
			MaxInterval:     50 * time.Millisecond,
			Multiplier:      2.0,
		},
	})
	if err != nil {
		t.Fatalf("NewRouter: %v", err)
	}
	subscribers.Register(router, fx.families, fx.stampCache, nil, silentLog(), time.Now)
	stop := runRouter(t, router)
	return pubsub, router, stop
}

// waitFor polls cond until true or timeout.
func waitFor(t *testing.T, cond func() bool, timeout time.Duration, msg string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond) // arch-test:wait-justified - async event-driven test wait
	}
	t.Fatal(msg)
}

// TestRevokeFamilies_PersonLevelCascade is the consolidated cascade
// gate for all four Person-level revocation triggers per ADR 0021 +
// ADR 0029. The four subscribers (password-changed, anonymised,
// globally-suspended, email-changed) share the SAME wiring shape —
// only the event payload + expected `revoke_reason` string differ.
// Table-driving them into one test cuts four pgtest-bound runs to a
// single wired-fixture boot with four t.Run subcases.
//
// SQL-contract coverage retained:
//   - subscriber finds Active families via ListActiveForPerson (RLS-
//     bypassed platform query)
//   - revoke transitions land via UpdateByID (UPDATE → revoked_at +
//     revoke_reason write)
//   - reason string is what the subscriber writes (cross-driver
//     payload propagation contract)
//
// Per-aggregate state-machine + business-rule coverage lives in
// refreshtokentest.FakeRepository unit tests (ADR 0062).
func TestRevokeFamilies_PersonLevelCascade(t *testing.T) {
	t.Parallel()
	fx := newFixture(t)
	pubsub, _, stop := wireRouter(t, fx)
	// t.Cleanup (not defer) — defer fires when the parent's body
	// returns, which happens BEFORE parallel subtests execute (parent's
	// t.Parallel + child's t.Parallel queue children for after-parent).
	// t.Cleanup waits for the parent AND all subtests to complete.
	t.Cleanup(stop)

	// Build one Person + Tenant + Family per subcase upfront so
	// subcases stay parallel-safe under the shared fixture (each
	// subcase has its own PersonID + Family rows; no cross-case
	// shared mutable state).
	type personLevelCase struct {
		name       string
		seedEmail  string
		event      func(pid uuid.UUID) integrationevents.Event
		wantReason string
	}
	cases := []personLevelCase{
		{
			name:      "PasswordChanged",
			seedEmail: "alice@flow.test",
			event: func(pid uuid.UUID) integrationevents.Event {
				return integrationevents.PersonPasswordChangedV1{
					PersonID:      pid,
					OccurredAtUTC: time.Now().UTC(),
				}
			},
			wantReason: "password_changed",
		},
		{
			name:      "Anonymised",
			seedEmail: "anon@flow.test",
			event: func(pid uuid.UUID) integrationevents.Event {
				return integrationevents.PersonAnonymisedV1{
					PersonID:      pid,
					OccurredAtUTC: time.Now().UTC(),
				}
			},
			wantReason: "person_anonymised",
		},
		{
			name:      "GloballySuspended",
			seedEmail: "suspended@flow.test",
			event: func(pid uuid.UUID) integrationevents.Event {
				return integrationevents.PersonGloballySuspendedV1{
					PersonID:      pid,
					Reason:        "compliance: cross-tenant fraud 2026-05-07",
					OccurredAtUTC: time.Now().UTC(),
				}
			},
			wantReason: "globally_suspended",
		},
		{
			name:      "EmailChanged",
			seedEmail: "old-email@flow.test",
			event: func(pid uuid.UUID) integrationevents.Event {
				return integrationevents.PersonEmailChangedV1{
					PersonID:      pid,
					OldEmail:      "old-email@flow.test",
					NewEmail:      "new-email@flow.test",
					OccurredAtUTC: time.Now().UTC(),
				}
			},
			wantReason: "email_changed",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			// Each subcase owns its own Person + Family rows under
			// the shared subscriber + router. PersonID isolation via
			// unique email seed prevents cross-subcase interference.
			p := fx.seedPerson(t, tc.seedEmail)
			tn := fx.seedTenant(t)
			f := fx.seedFamily(t, p, tn)

			pidUUID, _ := uuid.Parse(p.ID().String())
			publishEvent(t, pubsub, tc.event(pidUUID), uuid.Nil)

			waitFor(t, func() bool {
				got, err := fx.families.GetByID(t.Context(), f.ID())
				if err != nil {
					return false
				}
				return got.IsRevoked() && got.RevokeReason() == tc.wantReason
			}, 3*time.Second, "subscriber did not revoke family with reason="+tc.wantReason)
		})
	}
}

// TestRevokeFamilies_PasswordChanged_RevokesAllPersonFamilies — the
// password-changed case is special: a single event must revoke EVERY
// active family for the Person (multi-device logout). Kept as its own
// test because the assertion shape differs from the single-family
// cases above (list-all-active vs. specific-family lookup).
func TestRevokeFamilies_PasswordChanged_RevokesAllPersonFamilies(t *testing.T) {
	t.Parallel()
	fx := newFixture(t)
	pubsub, _, stop := wireRouter(t, fx)
	defer stop()

	p := fx.seedPerson(t, "multi-device@flow.test")
	tn := fx.seedTenant(t)
	f1 := fx.seedFamily(t, p, tn)
	f2 := fx.seedFamily(t, p, tn)

	pidUUID, _ := uuid.Parse(p.ID().String())
	publishEvent(t, pubsub, integrationevents.PersonPasswordChangedV1{
		PersonID:      pidUUID,
		OccurredAtUTC: time.Now().UTC(),
	}, uuid.Nil)

	waitFor(t, func() bool {
		actives, err := fx.families.ListActiveForPerson(t.Context(), p.ID())
		if err != nil {
			return false
		}
		return len(actives) == 0
	}, 3*time.Second, "subscriber did not revoke all families within 3s")

	for _, fid := range []refreshtoken.FamilyID{f1.ID(), f2.ID()} {
		got, err := fx.families.GetByID(t.Context(), fid)
		if err != nil {
			t.Fatalf("GetByID %s: %v", fid, err)
		}
		if !got.IsRevoked() || got.RevokeReason() != "password_changed" {
			t.Fatalf("family %s: revoked=%v reason=%q (want revoked=true reason=password_changed)",
				fid, got.IsRevoked(), got.RevokeReason())
		}
	}
}

func TestRevokeFamilies_OnMembershipDeactivated_NarrowsToTenantScope(t *testing.T) {
	t.Parallel()
	// MembershipDeactivated cascade is narrower than the Person-level
	// events: ONLY families bound to that (PersonID, TenantID) tuple
	// die. Other-tenant families for the same Person stay alive.
	fx := newFixture(t)
	pubsub, _, stop := wireRouter(t, fx)
	defer stop()

	p := fx.seedPerson(t, "multi-tenant@flow.test")
	tnA := fx.seedTenant(t)
	tnB := fx.seedTenant(t)
	famA := fx.seedFamily(t, p, tnA)
	famB := fx.seedFamily(t, p, tnB)

	pidUUID, _ := uuid.Parse(p.ID().String())
	tnAUUID, _ := uuid.Parse(tnA.ID().String())
	publishEvent(t, pubsub, integrationevents.MembershipDeactivatedV1{
		MembershipID:  uuid.New(),
		PersonID:      pidUUID,
		TenantIDClaim: tnAUUID,
		Reason:        "left-the-company",
		OccurredAtUTC: time.Now().UTC(),
	}, tnAUUID)

	// Family A → revoked; Family B → still active.
	waitFor(t, func() bool {
		got, err := fx.families.GetByID(t.Context(), famA.ID())
		if err != nil {
			return false
		}
		return got.IsRevoked() && got.RevokeReason() == "membership_deactivated"
	}, 3*time.Second, "tenant-A family not revoked on membership deactivation")

	gotB, err := fx.families.GetByID(t.Context(), famB.ID())
	if err != nil {
		t.Fatalf("GetByID famB: %v", err)
	}
	if gotB.IsRevoked() {
		t.Fatalf("tenant-B family revoked but should be untouched: reason=%q",
			gotB.RevokeReason())
	}
}

// TestRevokeFamilies_NoActiveFamilies_NoOp covers the empty-list
// short-circuit: a PersonLevel event for a Person with zero active
// families must mark the run successful AND must NOT issue a spurious
// UPDATE-all on the families table (e.g., from a missing predicate).
//
// Two halves of the SQL contract:
//
//  1. Subscriber-ran observable: an audit row exists for the event
//     action (router middleware contract).
//  2. No spurious mutation: a baseline-seeded family from a DIFFERENT
//     Person remains untouched after the subscriber runs — proves
//     the empty-list path didn't fall through to a row-set-bypass
//     UPDATE that would have flipped is_revoked=true on every family.
//
// Without (2) the test reduces to "subscriber ran" — already covered
// by TestRouter_FullStack in common/messaging/router_test.go. The
// no-spurious-mutation assertion is the SQL-specific contract.
func TestRevokeFamilies_NoActiveFamilies_NoOp(t *testing.T) {
	t.Parallel()
	fx := newFixture(t)
	pubsub, _, stop := wireRouter(t, fx)
	defer stop()

	// Sentinel family for an UNRELATED Person — must stay Active after
	// the empty-list noop runs for the target Person.
	other := fx.seedPerson(t, "untouched@flow.test")
	otherTn := fx.seedTenant(t)
	sentinel := fx.seedFamily(t, other, otherTn)

	p := fx.seedPerson(t, "noop@flow.test")
	pidUUID, _ := uuid.Parse(p.ID().String())

	publishEvent(t, pubsub, integrationevents.PersonPasswordChangedV1{
		PersonID:      pidUUID,
		OccurredAtUTC: time.Now().UTC(),
	}, uuid.Nil)

	// SQL-contract part 1: subscriber-ran observable.
	waitFor(t, audittest.HasAtLeastOneByAction(t, fx.pool, "identity.person_password_changed.v1"),
		3*time.Second, "subscriber audit row not written")

	// SQL-contract part 2: no spurious mutation — the sentinel family
	// for the unrelated Person must remain Active. A missing predicate
	// on the subscriber's UPDATE would have flipped is_revoked on
	// every family system-wide.
	got, err := fx.families.GetByID(t.Context(), sentinel.ID())
	if err != nil {
		t.Fatalf("GetByID sentinel: %v", err)
	}
	if got.IsRevoked() {
		t.Fatalf("sentinel family for unrelated Person was revoked — subscriber's empty-list path leaked a row-set-bypass UPDATE")
	}
}

func TestReuseDetectedSIEM_LogsOnReuseRevocation(t *testing.T) {
	t.Parallel()
	fx := newFixture(t)
	// Custom logger that records to a thread-safe buffer so the
	// subscriber goroutine's slog.Write doesn't race the assertion
	// goroutine's bytes.Contains under -race.
	buf := &safeBuffer{}
	siemLog := slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelWarn}))

	pubsub := gochannel.NewGoChannel(gochannel.Config{}, watermill.NewSlogLogger(silentLog()))
	t.Cleanup(func() { _ = pubsub.Close() })
	receiver := messaging.NewIdempotentReceiver(fx.pool)
	auditW := audit.NewWriter(fx.pool, silentLog(), time.Now)
	router, err := messaging.NewRouter(messaging.Deps{
		Subscriber:       pubsub,
		Publisher:        pubsub,
		Logger:           silentLog(),
		IdempotencyInbox: receiver,
		AuditWriter:      auditW,
		DeadLetters:      messaging.NewDeadLetterWriter(fx.pool, silentLog(), time.Now),
		Retry: messaging.RetryConfig{
			MaxRetries: 1, InitialInterval: 10 * time.Millisecond,
			MaxInterval: 50 * time.Millisecond, Multiplier: 2,
		},
	})
	if err != nil {
		t.Fatalf("NewRouter: %v", err)
	}
	subscribers.Register(router, fx.families, fx.stampCache, nil, siemLog, time.Now)
	stop := runRouter(t, router)
	defer stop()

	publishEvent(t, pubsub, integrationevents.RefreshTokenFamilyRevokedV1{
		FamilyID:      uuid.New(),
		PersonID:      uuid.New(),
		TenantIDClaim: uuid.New(),
		Reason:        "reuse_detected",
		OccurredAtUTC: time.Now().UTC(),
	}, uuid.New())

	waitFor(t, func() bool {
		return bytes.Contains(buf.Bytes(), []byte("reuse detected"))
	}, 3*time.Second, "SIEM WARN log not emitted")
}

func TestReuseDetectedSIEM_IgnoresNonReuseRevocations(t *testing.T) {
	t.Parallel()
	fx := newFixture(t)
	// safeBuffer — same race-detector reason as the sister test above.
	buf := &safeBuffer{}
	siemLog := slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelWarn}))

	pubsub := gochannel.NewGoChannel(gochannel.Config{}, watermill.NewSlogLogger(silentLog()))
	t.Cleanup(func() { _ = pubsub.Close() })
	receiver := messaging.NewIdempotentReceiver(fx.pool)
	auditW := audit.NewWriter(fx.pool, silentLog(), time.Now)
	router, err := messaging.NewRouter(messaging.Deps{
		Subscriber:       pubsub,
		Publisher:        pubsub,
		Logger:           silentLog(),
		IdempotencyInbox: receiver,
		AuditWriter:      auditW,
		DeadLetters:      messaging.NewDeadLetterWriter(fx.pool, silentLog(), time.Now),
		Retry: messaging.RetryConfig{
			MaxRetries: 1, InitialInterval: 10 * time.Millisecond,
			MaxInterval: 50 * time.Millisecond, Multiplier: 2,
		},
	})
	if err != nil {
		t.Fatalf("NewRouter: %v", err)
	}
	subscribers.Register(router, fx.families, fx.stampCache, nil, siemLog, time.Now)
	stop := runRouter(t, router)
	defer stop()

	publishEvent(t, pubsub, integrationevents.RefreshTokenFamilyRevokedV1{
		FamilyID:      uuid.New(),
		PersonID:      uuid.New(),
		TenantIDClaim: uuid.New(),
		Reason:        "user_logout",
		OccurredAtUTC: time.Now().UTC(),
	}, uuid.New())

	// Wait for processing to settle, then assert no SIEM log.
	time.Sleep(800 * time.Millisecond) // arch-test:wait-justified - async event-driven test wait
	if bytes.Contains(buf.Bytes(), []byte("reuse detected")) {
		t.Fatalf("SIEM log emitted for non-reuse revocation: %s", buf.String())
	}
}
