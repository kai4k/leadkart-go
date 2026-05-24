//go:build integration

// arch-test:no-synctest — subscriber tests exercise the Watermill router
// goroutine against a real Postgres testcontainer; the polled `families`
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
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/ThreeDotsLabs/watermill"
	"github.com/ThreeDotsLabs/watermill/message"
	"github.com/ThreeDotsLabs/watermill/pubsub/gochannel"
	"github.com/alicebob/miniredis/v2"
	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pressly/goose/v3"
	"github.com/redis/go-redis/v9"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/leadkart/leadkart-go/internal/common/email"
	"github.com/leadkart/leadkart-go/internal/common/ids"
	"github.com/leadkart/leadkart-go/internal/common/slug"
	"github.com/leadkart/leadkart-go/internal/identity/adapters"
	"github.com/leadkart/leadkart-go/internal/identity/domain/person"
	"github.com/leadkart/leadkart-go/internal/identity/domain/refreshtoken"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
	"github.com/leadkart/leadkart-go/internal/identity/integrationevents"
	"github.com/leadkart/leadkart-go/internal/identity/ports/subscribers"
	"github.com/leadkart/leadkart-go/internal/common/audit"
	"github.com/leadkart/leadkart-go/internal/common/cache"
	"github.com/leadkart/leadkart-go/internal/common/messaging"
	"github.com/leadkart/leadkart-go/internal/common/pg"
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
	pool        *pgxpool.Pool
	tenants     *adapters.TenantRepository
	persons     *adapters.PersonRepository
	families    *adapters.RefreshTokenFamilyRepository
	stampCache  *adapters.SecurityStampCache
	miniredis   *miniredis.Miniredis
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()

	c, err := postgres.Run(ctx,
		"postgres:17-alpine",
		postgres.WithDatabase("leadkart_test"),
		postgres.WithUsername("leadkart"),
		postgres.WithPassword("leadkart_test"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).WithStartupTimeout(60*time.Second),
		),
	)
	if err != nil {
		t.Fatalf("start postgres: %v", err)
	}
	t.Cleanup(func() {
		// Cleanup runs after t.Context() is cancelled — must use Background.
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = c.Terminate(ctx)
	})

	ownerDSN, err := c.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("dsn: %v", err)
	}

	gooseDB, err := goose.OpenDBWithDriver("pgx", ownerDSN)
	if err != nil {
		t.Fatalf("goose open: %v", err)
	}
	if err := pg.EnsureGooseDialect(); err != nil {
		_ = gooseDB.Close()
		t.Fatalf("set dialect: %v", err)
	}
	if err := goose.UpContext(ctx, gooseDB, migrationsDir(t)); err != nil {
		_ = gooseDB.Close()
		t.Fatalf("goose up: %v", err)
	}
	for _, s := range []string{
		`CREATE ROLE leadkart_app LOGIN PASSWORD 'leadkart_app_pw' NOSUPERUSER NOINHERIT NOCREATEROLE NOCREATEDB`,
		`GRANT USAGE ON SCHEMA app, identity, buildingblocks TO leadkart_app`,
		`GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA identity TO leadkart_app`,
		`GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA buildingblocks TO leadkart_app`,
		`GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA app TO leadkart_app`,
		`GRANT EXECUTE ON ALL FUNCTIONS IN SCHEMA app TO leadkart_app`,
	} {
		if _, err := gooseDB.ExecContext(ctx, s); err != nil {
			_ = gooseDB.Close()
			t.Fatalf("provision app role: %s: %v", s, err)
		}
	}
	_ = gooseDB.Close()

	host, err := c.Host(ctx)
	if err != nil {
		t.Fatalf("host: %v", err)
	}
	port, err := c.MappedPort(ctx, "5432/tcp")
	if err != nil {
		t.Fatalf("port: %v", err)
	}
	appDSN := "postgres://leadkart_app:leadkart_app_pw@" + host + ":" + port.Port() + "/leadkart_test?sslmode=disable"

	pool, err := pgxpool.New(ctx, appDSN)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close)

	tx := pg.NewTransactor(pool)
	persons := adapters.NewPersonRepository(pool, tx)

	// Real HybridCache + SecurityStampCache against miniredis so the
	// cascade subscriber's invalidation step runs end-to-end against
	// the same facade production wires (audit-checklist.md §12b).
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

func migrationsDir(t *testing.T) string {
	t.Helper()
	_, here, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	return filepath.Join(filepath.Dir(here), "..", "..", "..", "..", "migrations")
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
		Logger:           silentLog(),
		IdempotencyInbox: receiver,
		AuditWriter:      auditW,
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

func TestRevokeFamilies_OnPasswordChanged(t *testing.T) {
	t.Parallel()
	fx := newFixture(t)
	pubsub, _, stop := wireRouter(t, fx)
	defer stop()

	p := fx.seedPerson(t, "alice@flow.test")
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
	}, 3*time.Second, "subscriber did not revoke families within 3s")

	// Verify both were revoked with reason="password_changed".
	for _, fid := range []refreshtoken.FamilyID{f1.ID(), f2.ID()} {
		got, err := fx.families.GetByID(t.Context(), fid)
		if err != nil {
			t.Fatalf("GetByID %s: %v", fid, err)
		}
		if !got.IsRevoked() {
			t.Fatalf("family %s not revoked", fid)
		}
		if got.RevokeReason() != "password_changed" {
			t.Fatalf("family %s reason: got %q want password_changed", fid, got.RevokeReason())
		}
	}
}

func TestRevokeFamilies_OnAnonymised(t *testing.T) {
	t.Parallel()
	fx := newFixture(t)
	pubsub, _, stop := wireRouter(t, fx)
	defer stop()

	p := fx.seedPerson(t, "anon@flow.test")
	tn := fx.seedTenant(t)
	f := fx.seedFamily(t, p, tn)

	pidUUID, _ := uuid.Parse(p.ID().String())
	publishEvent(t, pubsub, integrationevents.PersonAnonymisedV1{
		PersonID:      pidUUID,
		OccurredAtUTC: time.Now().UTC(),
	}, uuid.Nil)

	waitFor(t, func() bool {
		got, err := fx.families.GetByID(t.Context(), f.ID())
		if err != nil {
			return false
		}
		return got.IsRevoked() && got.RevokeReason() == "person_anonymised"
	}, 3*time.Second, "anonymise subscriber did not revoke family")
}

func TestRevokeFamilies_OnGloballySuspended(t *testing.T) {
	t.Parallel()
	fx := newFixture(t)
	pubsub, _, stop := wireRouter(t, fx)
	defer stop()

	p := fx.seedPerson(t, "suspended@flow.test")
	tn := fx.seedTenant(t)
	f := fx.seedFamily(t, p, tn)

	pidUUID, _ := uuid.Parse(p.ID().String())
	publishEvent(t, pubsub, integrationevents.PersonGloballySuspendedV1{
		PersonID:      pidUUID,
		Reason:        "compliance: cross-tenant fraud 2026-05-07",
		OccurredAtUTC: time.Now().UTC(),
	}, uuid.Nil)

	waitFor(t, func() bool {
		got, err := fx.families.GetByID(t.Context(), f.ID())
		if err != nil {
			return false
		}
		return got.IsRevoked() && got.RevokeReason() == "globally_suspended"
	}, 3*time.Second, "globally-suspended subscriber did not revoke family")
}

func TestRevokeFamilies_OnEmailChanged(t *testing.T) {
	t.Parallel()
	fx := newFixture(t)
	pubsub, _, stop := wireRouter(t, fx)
	defer stop()

	p := fx.seedPerson(t, "old-email@flow.test")
	tn := fx.seedTenant(t)
	f := fx.seedFamily(t, p, tn)

	pidUUID, _ := uuid.Parse(p.ID().String())
	publishEvent(t, pubsub, integrationevents.PersonEmailChangedV1{
		PersonID:      pidUUID,
		OldEmail:      "old-email@flow.test",
		NewEmail:      "new-email@flow.test",
		OccurredAtUTC: time.Now().UTC(),
	}, uuid.Nil)

	waitFor(t, func() bool {
		got, err := fx.families.GetByID(t.Context(), f.ID())
		if err != nil {
			return false
		}
		return got.IsRevoked() && got.RevokeReason() == "email_changed"
	}, 3*time.Second, "email-changed subscriber did not revoke family")
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

func TestRevokeFamilies_NoActiveFamilies_NoOp(t *testing.T) {
	t.Parallel()
	fx := newFixture(t)
	pubsub, _, stop := wireRouter(t, fx)
	defer stop()

	p := fx.seedPerson(t, "noop@flow.test")
	pidUUID, _ := uuid.Parse(p.ID().String())

	publishEvent(t, pubsub, integrationevents.PersonPasswordChangedV1{
		PersonID:      pidUUID,
		OccurredAtUTC: time.Now().UTC(),
	}, uuid.Nil)

	// Wait for processing — assert via audit row that subscriber ran
	// + succeeded without error (zero families to revoke is success).
	waitFor(t, func() bool {
		var n int
		_ = fx.pool.QueryRow(t.Context(), ` // arch-test:ignore-err - test fixture setup
			SELECT count(*) FROM buildingblocks.audit_log_entry
			WHERE  action = 'identity.person_password_changed.v1'
			  AND  succeeded = true
		`).Scan(&n)
		return n >= 1
	}, 3*time.Second, "subscriber audit row not written")
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
		Logger:           silentLog(),
		IdempotencyInbox: receiver,
		AuditWriter:      auditW,
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
		Logger:           silentLog(),
		IdempotencyInbox: receiver,
		AuditWriter:      auditW,
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
