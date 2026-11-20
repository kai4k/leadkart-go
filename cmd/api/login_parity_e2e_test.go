//go:build integration

// arch-test:no-timeout-needed — startWiredPostgresForHTTP uses context.WithTimeout(90s)
//   internally; per-request HTTP uses t.Context().
//
// arch-test:raw-sql-justified — the account kinds a login can meet (deactivated,
//   anonymised) have no HTTP path in this module; they are set by UPDATE, and the
//   idempotency table is read to prove nothing was stored.

// ADR 0068 D11/D12 through the REAL public chain (buildPublicChain, the constructor
// main uses): the other e2e fixtures serve the bare mux and never see the limiters,
// the client-address handling or the idempotency middleware.
package main

import (
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/leadkart/leadkart-go/internal/common/config"
	"github.com/leadkart/leadkart-go/internal/common/email"
	"github.com/leadkart/leadkart-go/internal/common/idempotency"
	"github.com/leadkart/leadkart-go/internal/common/ids"
	crmapp "github.com/leadkart/leadkart-go/internal/crm/app"
	"github.com/leadkart/leadkart-go/internal/identity/app/argon2"
	"github.com/leadkart/leadkart-go/internal/identity/domain/person"
	"github.com/leadkart/leadkart-go/internal/identity/ports"
)

// newE2EChainFixture is newE2EFixtureWith behind buildPublicChain: trusted BFF at
// loopback, generous route limits (the parity runs send dozens of logins) and the
// caller's own config tweaks. Each caller picks a distinct X-Forwarded-For so its
// requests land in its own limiter bucket.
func newE2EChainFixture(t *testing.T, mutate func(*config.AppConfig)) (e2eFixture, identityWiring) {
	t.Helper()
	pool := startWiredPostgresForHTTP(t)
	cfg := config.Defaults()
	cfg.JWT = config.JWTConfig{KeyID: "test-k1", SigningKey: "0123456789abcdef0123456789abcdef"}
	cfg.HTTP.TrustedProxies = "127.0.0.0/8"
	cfg.RateLimit.Login = config.RouteLimit{PerMinute: 60000, Burst: 1000}
	if mutate != nil {
		mutate(&cfg)
	}
	wiring, err := buildIdentityApp(pool, newTestHybridCache(t), cfg, time.Now)
	if err != nil {
		t.Fatalf("buildIdentityApp: %v", err)
	}
	mw, err := buildPublicChain(cfg, silentLogger(), idempotency.NewPostgresStore(pool), wiring.Issuer, time.Now)
	if err != nil {
		t.Fatalf("buildPublicChain: %v", err)
	}
	srv := httptest.NewServer(mw(newServer(silentLogger(), wiring.App, buildPlatformApp(pool, time.Now).App, buildInventoryApp(pool, nil), crmapp.Application{}, buildTasksApp(pool, nil), buildDispatchApp(pool, nil), buildOrdersApp(pool, nil), buildNotificationsApp(pool, nil), wiring.Issuer, wiring.StampValidator)))
	t.Cleanup(srv.Close)
	return e2eFixture{URL: srv.URL, Issuer: wiring.Issuer, Pool: pool, app: wiring.App, persons: wiring.Persons, stamps: wiring.StampCache}, wiring
}

// surface is everything a client can see of one answer, minus what legitimately
// differs per request (Date, the correlation id) and Retry-After, which is a
// countdown to a wall-clock instant and is compared separately with a tolerance.
type surface struct {
	status int
	body   string
	header string
}

func surfaceOf(r httpResp) surface {
	h := r.header.Clone()
	for _, k := range []string{"Date", "X-Correlation-Id", "Retry-After"} {
		h.Del(k)
	}
	lines := make([]string, 0, len(h))
	for k, v := range h {
		lines = append(lines, k+": "+strings.Join(v, ", "))
	}
	sort.Strings(lines)
	return surface{status: r.status, body: strings.TrimSpace(string(r.body)), header: strings.Join(lines, "\n")}
}

func retryAfter(t *testing.T, r httpResp) int {
	t.Helper()
	if r.status != http.StatusLocked && r.status != http.StatusServiceUnavailable {
		return 0
	}
	v, err := strconv.Atoi(r.header.Get("Retry-After"))
	if err != nil || v < 1 {
		t.Fatalf("status %d without a usable Retry-After: %q", r.status, r.header.Get("Retry-After"))
	}
	return v
}

type loginKind struct {
	name, email, password string
	xff                   string
}

// seedLoginKinds creates one address of every kind a login can meet.
func seedLoginKinds(t *testing.T, f e2eFixture) []loginKind {
	t.Helper()
	known := f.registerOnly(t, "known")
	deactivated := f.registerOnly(t, "deact")
	anonymised := f.registerOnly(t, "anon")
	for _, q := range []struct{ sql, email string }{
		{`UPDATE identity.persons SET is_active = false WHERE email = $1`, deactivated.Email},
		{`UPDATE identity.persons SET is_anonymised = true, anonymised_at = now() WHERE email = $1`, anonymised.Email},
	} {
		if _, err := f.Pool.Exec(t.Context(), q.sql, q.email); err != nil {
			t.Fatalf("seed kind: %v", err)
		}
	}
	// A person with no membership at all (off-boarded): added through the repository.
	const noMemberPassword = "Correct-Horse-Battery-9!x"
	hash, err := argon2.Hash(noMemberPassword)
	if err != nil {
		t.Fatal(err)
	}
	pwHash, err := person.NewPasswordHash(hash)
	if err != nil {
		t.Fatal(err)
	}
	noMemberEmail := "nomember-" + ids.NewV7().String()[24:] + "@e2e.test"
	addr, _ := email.New(noMemberEmail)
	p, err := person.New(person.ID(ids.NewV7().String()), addr, "No", "Member", pwHash, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := f.persons.Add(t.Context(), p); err != nil {
		t.Fatal(err)
	}
	unknown := "nobody-" + ids.NewV7().String()[24:] + "@e2e.test"
	return []loginKind{
		{"known", known.Email, known.Password, "198.51.100.11"},
		{"deactivated", deactivated.Email, deactivated.Password, "198.51.100.12"},
		{"anonymised", anonymised.Email, anonymised.Password, "198.51.100.13"},
		{"no membership", noMemberEmail, noMemberPassword, "198.51.100.14"},
		{"unknown", unknown, "Whatever-1-password", "198.51.100.15"},
	}
}

func (f e2eFixture) loginAs(t *testing.T, k loginKind, password string) httpResp {
	t.Helper()
	return f.doWithHeaders(t, http.MethodPost, "/api/v1/auth/login", "",
		ports.LoginRequest{Email: k.email, Password: password, DeviceLabel: "parity"},
		map[string]string{"X-Forwarded-For": k.xff})
}

// Through the real chain, every kind of address answers every failed attempt the
// same: status, full body and every header (the countdown within a few seconds),
// for twelve sequential wrong passwords (the tenth locks) and for the correct
// password after the lock.
func TestE2E_Login_EveryAddressKindAnswersIdenticallyThroughTheRealChain(t *testing.T) {
	f, _ := newE2EChainFixture(t, nil)
	kinds := seedLoginKinds(t, f)
	const attempts = 12

	type run struct {
		answers []httpResp
		afterOK httpResp // the correct password, once locked
	}
	runs := map[string]run{}
	for _, k := range kinds {
		var r run
		for range attempts {
			r.answers = append(r.answers, f.loginAs(t, k, "Wrong-Password-1!"))
		}
		r.afterOK = f.loginAs(t, k, k.password)
		runs[k.name] = r
	}
	ref := runs["unknown"]
	if got := ref.answers[person.MaxFailedLogins-2].status; got != http.StatusUnauthorized {
		t.Fatalf("attempt %d on an unknown address = %d, want 401", person.MaxFailedLogins-1, got)
	}
	if got := ref.answers[person.MaxFailedLogins-1].status; got != http.StatusLocked {
		t.Fatalf("attempt %d on an unknown address = %d, want 423", person.MaxFailedLogins, got)
	}
	for _, k := range kinds {
		r := runs[k.name]
		for i := range attempts {
			if got, want := surfaceOf(r.answers[i]), surfaceOf(ref.answers[i]); got != want {
				t.Errorf("%s, attempt %d:\n got  %+v\n want %+v", k.name, i+1, got, want)
			}
			if d := retryAfter(t, r.answers[i]) - retryAfter(t, ref.answers[i]); d < -5 || d > 5 {
				t.Errorf("%s, attempt %d: Retry-After differs by %ds", k.name, i+1, d)
			}
		}
		if got, want := surfaceOf(r.afterOK), surfaceOf(ref.afterOK); got != want || r.afterOK.status != http.StatusLocked {
			t.Errorf("%s, correct password once locked:\n got  %+v\n want %+v (423)", k.name, got, want)
		}
	}
}

// The same ten simultaneous wrong logins, per kind: nine 401s and one 423, with
// identical bodies and headers, whatever the address is.
func TestE2E_Login_ConcurrentWrongPasswordsAnswerIdenticallyForEveryAddressKind(t *testing.T) {
	f, _ := newE2EChainFixture(t, nil)
	kinds := seedLoginKinds(t, f)
	multiset := func(k loginKind) map[surface]int {
		var (
			wg    sync.WaitGroup
			mu    sync.Mutex
			start = make(chan struct{})
			out   = map[surface]int{}
		)
		for range person.MaxFailedLogins {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				r, err := f.tryDo(t.Context(), http.MethodPost, "/api/v1/auth/login", "",
					ports.LoginRequest{Email: k.email, Password: "Wrong-Password-1!", DeviceLabel: "parity"},
					map[string]string{"X-Forwarded-For": k.xff})
				if err != nil {
					t.Errorf("%s: %v", k.name, err)
					return
				}
				mu.Lock()
				out[surfaceOf(r)]++
				mu.Unlock()
			}()
		}
		close(start)
		wg.Wait()
		return out
	}
	ref := multiset(kinds[len(kinds)-1]) // unknown
	statuses := map[int]int{}
	for s, n := range ref {
		statuses[s.status] += n
	}
	if statuses[http.StatusUnauthorized] != person.MaxFailedLogins-1 || statuses[http.StatusLocked] != 1 {
		t.Fatalf("unknown address: %v, want nine 401 and one 423", statuses)
	}
	for _, k := range kinds[:len(kinds)-1] {
		got := multiset(k)
		if len(got) != len(ref) {
			t.Errorf("%s: %d distinct answers, unknown has %d: %v vs %v", k.name, len(got), len(ref), got, ref)
			continue
		}
		for s, n := range ref {
			if got[s] != n {
				t.Errorf("%s: answer %+v seen %d times, unknown %d times", k.name, s, got[s], n)
			}
		}
	}
}

// With every Argon2 slot taken, every login branch answers the same 503 (an
// existence oracle if only some did), and the other password routes refuse too,
// through the production wiring. Releasing the slots restores service.
func TestE2E_PasswordRoutes_Answer503WhenTheArgonGateIsSaturated(t *testing.T) {
	f, wiring := newE2EChainFixture(t, func(c *config.AppConfig) {
		c.Argon2 = config.Argon2Config{MaxConcurrent: 2, QueueTimeout: 100 * time.Millisecond}
	})
	admin := f.registerAndLogin(t, "gate")
	operator := f.mintPlatformTokenWith(t, nil, true) // super user: holds platform.tenants.create
	xff := map[string]string{"X-Forwarded-For": "198.51.100.21"}

	var held []func()
	for range wiring.ArgonGate.Size() {
		release, err := wiring.ArgonGate.Acquire(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		held = append(held, release)
	}
	release := func() {
		for _, r := range held {
			r()
		}
		held = nil
	}
	defer release()

	unknown := "nobody-" + ids.NewV7().String()[24:] + "@e2e.test"
	login := func(addr, pw string) httpResp {
		return f.doWithHeaders(t, http.MethodPost, "/api/v1/auth/login", "", ports.LoginRequest{Email: addr, Password: pw, DeviceLabel: "gate"}, xff)
	}
	answers := map[string]httpResp{
		"real, correct password": login(admin.Email, admin.Password),
		"real, wrong password":   login(admin.Email, "Wrong-Password-1!"),
		"unknown address":        login(unknown, "Wrong-Password-1!"),
	}
	var ref surface
	for name, r := range answers {
		if r.status != http.StatusServiceUnavailable || retryAfter(t, r) != 2 || decodeError(t, r.body).Error != ports.ErrCodeServiceBusy {
			t.Fatalf("login (%s) = %d %s retry-after %q; want 503 service_busy, Retry-After 2", name, r.status, r.body, r.header.Get("Retry-After"))
		}
		if ref == (surface{}) {
			ref = surfaceOf(r)
		} else if got := surfaceOf(r); got != ref {
			t.Errorf("login (%s) differs from the others: %+v vs %+v", name, got, ref)
		}
	}

	token := admin.AccessToken
	for name, r := range map[string]httpResp{
		"change password": f.doWithHeaders(t, http.MethodPost, "/api/v1/auth/change-password", token,
			ports.ChangePasswordRequest{CurrentPassword: admin.Password, NewPassword: "A-Brand-New-Passphrase-77!"}, xff),
		"create user": f.doWithHeaders(t, http.MethodPost, "/api/v1/users", token,
			ports.CreateUserRequest{Email: "gate-user-" + ids.NewV7().String()[24:] + "@e2e.test", Password: "Tr0ub4dor&3-fresh-passphrase", FirstName: "G", LastName: "U"}, xff),
		"register tenant": f.doWithHeaders(t, http.MethodPost, "/api/v1/tenants", operator,
			ports.RegisterTenantRequest{
				Slug: "gate-" + ids.NewV7().String()[24:], LegalName: "Gate Pharma Pvt Ltd", DisplayName: "Gate",
				AdminEmail: "gate-admin-" + ids.NewV7().String()[24:] + "@e2e.test", AdminPassword: "Tr0ub4dor&3-fresh-passphrase",
				AdminFirstName: "G", AdminLastName: "A",
			}, xff),
	} {
		if r.status != http.StatusServiceUnavailable || decodeError(t, r.body).Error != ports.ErrCodeServiceBusy {
			t.Errorf("%s with the gate saturated = %d %s, want 503 service_busy", name, r.status, r.body)
		}
	}

	release()
	if r := login(admin.Email, admin.Password); r.status != http.StatusOK {
		t.Fatalf("login after the slots were freed = %d %s", r.status, r.body)
	}
	// The refused change-password changed nothing: the old password still works.
	if r := login(admin.Email, "A-Brand-New-Passphrase-77!"); r.status == http.StatusOK {
		t.Fatal("the password changed although the gate refused")
	}
}

// POST /auth/change-password checks the current password at most 5 times a minute
// per PERSON: the sixth guess is 429 with a Retry-After, before any password work;
// another person is unaffected, and so is the same person's different client address.
func TestE2E_ChangePassword_IsLimitedPerPerson(t *testing.T) {
	f, _ := newE2EChainFixture(t, nil)
	a := f.registerAndLogin(t, "cpa")
	b := f.registerAndLogin(t, "cpb")
	guess := func(token, xff string) httpResp {
		return f.doWithHeaders(t, http.MethodPost, "/api/v1/auth/change-password", token,
			ports.ChangePasswordRequest{CurrentPassword: "Wrong-Current-1!", NewPassword: "A-Brand-New-Passphrase-77!"},
			map[string]string{"X-Forwarded-For": xff})
	}
	for i := range 5 {
		if r := guess(a.AccessToken, "198.51.100.31"); r.status != http.StatusUnauthorized {
			t.Fatalf("guess %d = %d %s, want 401", i+1, r.status, r.body)
		}
	}
	r := guess(a.AccessToken, "198.51.100.31")
	if r.status != http.StatusTooManyRequests || decodeError(t, r.body).Error != ports.ErrCodeRateLimited || retryAfter429(t, r) < 1 {
		t.Fatalf("sixth guess = %d %s (Retry-After %q), want 429 rate_limited with Retry-After", r.status, r.body, r.header.Get("Retry-After"))
	}
	if r := guess(a.AccessToken, "198.51.100.32"); r.status != http.StatusTooManyRequests {
		t.Fatalf("the same person from another client address = %d, want still 429 (the budget is the person's)", r.status)
	}
	if r := guess(b.AccessToken, "198.51.100.31"); r.status != http.StatusUnauthorized {
		t.Fatalf("another person from the same client address = %d, want 401 (own budget)", r.status)
	}
}

func retryAfter429(t *testing.T, r httpResp) int {
	t.Helper()
	v, err := strconv.Atoi(r.header.Get("Retry-After"))
	if err != nil {
		t.Fatalf("429 without a numeric Retry-After: %q", r.header.Get("Retry-After"))
	}
	return v
}

// X-Command-Id is ignored on the credential routes: the same login sent twice with
// the same key runs twice, is never answered from a stored record, and nothing is
// written to the idempotency table (login answers carry plaintext refresh tokens).
func TestE2E_CommandID_IsIgnoredOnCredentialRoutesAndNothingIsStored(t *testing.T) {
	f, _ := newE2EChainFixture(t, nil)
	admin := f.registerAndLogin(t, "idem")
	key := uuid.NewString()
	var tokens []string
	for range 2 {
		r := f.doWithHeaders(t, http.MethodPost, "/api/v1/auth/login", "",
			ports.LoginRequest{Email: admin.Email, Password: admin.Password, DeviceLabel: "idem"},
			map[string]string{"X-Forwarded-For": "198.51.100.41", idempotency.CommandIDHeader: key})
		if r.status != http.StatusOK || r.header.Get(idempotency.ReplayHeader) != "" {
			t.Fatalf("login with a command id = %d replay=%q, want a fresh 200", r.status, r.header.Get(idempotency.ReplayHeader))
		}
		tokens = append(tokens, string(r.body))
	}
	if tokens[0] == tokens[1] {
		t.Fatal("the second login was served the first one's stored answer")
	}
	// A malformed id on a credential route is ignored too, not a 400.
	r := f.doWithHeaders(t, http.MethodPost, "/api/v1/auth/login", "",
		ports.LoginRequest{Email: admin.Email, Password: "Wrong-Password-1!", DeviceLabel: "idem"},
		map[string]string{"X-Forwarded-For": "198.51.100.41", idempotency.CommandIDHeader: "not-a-uuid"})
	if r.status != http.StatusUnauthorized {
		t.Fatalf("login with a malformed command id = %d, want the ordinary 401", r.status)
	}
	var stored int
	if err := f.Pool.QueryRow(t.Context(), `SELECT count(*) FROM common.command_idempotency`).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored != 0 {
		t.Fatalf("%d idempotency records stored for credential routes", stored)
	}
}
