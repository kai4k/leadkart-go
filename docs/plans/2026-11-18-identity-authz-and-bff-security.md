# LeadKart-Go — Identity authorisation and BFF security fixes: implementation plan

**Goal.** Close ten verified security gaps in the Go API at `D:\Development\leadkart-go`, on branch `fix/identity-authz-and-bff-security`:
- identity routes only check that the caller is logged in
- `X-Tenant-Id` is ignored
- refresh rotation has no grace window
- one shared rate-limit bucket behind the BFF
- `must_change_password` is not enforced
- enumeration leaks on login and password reset
- impersonation tokens have the wrong reach
- the tenant password policy is not enforced
- tenant registration is unauthenticated
- small contract bugs

No new business endpoints, no DTO reshaping, no 204→200 changes.

**Architecture.** HTTP authorisation stays in `internal/identity/ports/authn`. Every module consumes it, and one decision function is extended there. Pure permission semantics, password rules and refresh-family rules go into the domain packages (`permission`, `tenant`, `refreshtoken`). Command handlers take every tenant or actor value as an explicit parameter. Cross-cutting HTTP concerns (client IP, per-route limits, response-time floor) go into `internal/common/httpmw`. No schema migrations are needed.

**Tech stack.** Go 1.26.4 with stdlib `net/http` ServeMux, `golang-jwt/jwt/v5`, pgx/sqlc, koanf, `golang.org/x/time/rate`, and stdlib `crypto/hkdf` and `net/netip`. Tests use stdlib `testing`, per-aggregate fakes (ADR 0062), miniredis, and testcontainers Postgres (`//go:build integration`).

## Global constraints

1. **Doctrine.** Before touching `domain/`, `app/`, `ports/` or `adapters/`, read `CLAUDE.md` and `docs/doctrine/tdl_canon.md` and invoke the **tdl-discipline** skill. Before every commit, invoke **verify-green**. The rules:
   - Interfaces live with their consumer.
   - No cross-module imports, except the existing shared kernel `internal/identity/ports/authn` (allow-listed in `eda_arch_test.go`).
   - `app/` never calls `tenancy.FromContext`; tenant and actor values travel in the command struct.
   - Aggregates expose behaviour only. No public setters, no `Save()`, no business-verb repo methods.
   - Fakes, not mocks.
   - No `time.Sleep` in tests.
   - OpenAPI `api/openapi.yaml` is updated for every contract change (ADR 0046/0050). The identity route↔spec gate `TestArch_RouteHasSpecOperation` must stay green.
   - When a gate goes red, fix the code. Never weaken the gate or add an allowlist entry to silence it.
2. **Docker is available.** Docker Desktop 29.5.3 with the linux engine is running. Integration tests run with testcontainers. Docker Desktop on Windows needs serial package execution, `-p 1` (Taskfile `test:int` comment).
3. **TDD per step.** Write the failing test, run it and see it fail, write the minimum code, run it and see it pass. Each task ends with **one commit**.
4. **Per-task gate.** Run in Git Bash from `D:/Development/leadkart-go`, read every exit code, and never infer one from an earlier run:
   ```
   G1  gofmt -l $(git diff --name-only HEAD -- '*.go')            # no output
   G2  go build ./... ; echo BUILD=$? ; go build -tags=integration ./... ; echo BUILDINT=$?   # 0 / 0
   G3  go vet ./... ; echo VET=$?                                  # 0
   G4  task test ; echo UNIT=$?                                    # 0, no "--- FAIL"
   G5  go clean -testcache ; task test:arch ; echo ARCH=$?         # 0
   G6  task test:int:compile ; echo INTC=$?                        # 0
   G7  go test -tags=integration -p 1 -count=1 -timeout=15m <focused pkgs> ; echo INT=$?   # 0
   ```
   - `task lint` is optional. Run it if golangci-lint is installed and report the result. Same for `task ci:openapi`, which needs node/npx.
   - The full `task test:int` (serial, up to ~15 min) runs **after Task 4** (the end of the identity-authorisation unit) and in **Task 16**.
5. **Commits.** Stage explicit paths with `git add <paths>`, never `-A`. Run the gate and the commit as separate steps. The message states the verified markers (for example `BUILD=0 UNIT=0 ARCH=0 INT=0`). Use a conventional subject and end the message with:
   `Co-Authored-By: Claude Fable 5.2 <noreply@anthropic.com>`
   **Never push.** Branch: `fix/identity-authz-and-bff-security`.
6. **Order.** Tasks are ordered by security impact. Tasks 1–4 (item 1) are self-contained and can ship alone. Every task lists the interfaces it relies on from earlier tasks.

## Decisions (from the ADRs)

- **D1 – what `tenant.admin` grants.** `tenant.admin` satisfies **every** catalogue permission, including other modules' (crm/inventory/orders/tasks/dispatch/notifications/marketplace/lead-credits read), but **only in the caller's own tenant** (claims `tenant_id` must equal the effective tenant). It never satisfies platform-reserved permissions: `platform.tenants.*`, `platform.users.*`, `platform.roles.*`, `platform.unverified_contacts.manage`, `platform.lead_credits.topup`, `identity.tenants.{suspend,activate,delete}`, `identity.users.anonymise`.
  - Grounds: ADR 0061 amendment 1 says CompanyOwner "short-circuits via `Meta.TenantAdmin`". The seed godoc says tenant.admin gives "full operational authority". ADR 0045 says operators "get the full CompanyOwner permission set inside the session", and the impersonation token carries `["tenant.admin"]`.
  - I rejected re-seeding: ADR 0036 Alternative 5 argues against enumerating permissions on a role, and stored roles would need data migrations each time the catalogue grows.
- **D2 – platform equivalents on the unified surface.** On the unified surface (`/v1/users`, `/v1/roles`, `/v1/tenants`, ADR 0039 Context), a slug-anchored, non-impersonation platform operator also satisfies `identity.users|roles|tenants.*` through the unused `platform.*` catalogue entries (view→view|manage, mutations→manage, users.create→create|manage).
  - Grounds: this mirrors the .NET `PlatformTenant{User,Role,Management}Endpoints` gates and the existing E2E `mintPlatformToken` intent.
- **D3 – self-service rules.**
  - `GET /users/{id}` and `PATCH /users/{id}/profile` pass for self (path id equals the caller's `membership_id`) or with `users.view` / `users.update`.
  - Roles, permission overrides, deactivate/reactivate and manager changes are **denied on self** for everyone except super users. This prevents both self-escalation and owner self-lockout.
- **D4 – no grant beyond your own authority.** A caller may only confer permissions they themselves satisfy: role assignment checks the role's permission set, and only super users may assign `is_super_admin` roles. Role permission grant/replace and user override grants are checked the same way.
  - Grounds: ADR 0036 closed set and ADR 0055 "privilege plane". Without this rule, `identity.roles.assign` is equivalent to `tenant.admin`.
- **D5 – permission-request routes stay as they are.** Permission-request list/get/submit/approve/deny/cancel stay authentication-only with handler-inline checks, which is the **design** in ADR 0055 §8. I re-verified it: list is scoped to the caller's own membership and get collapses to 404. They go on the arch-test allowlist.
- **D6 – tenant registration is operator-only.** `POST /api/v1/tenants` is **not** self-service. ADR 0053 says "LeadKart is B2B + operator-onboarded — there is no public sign-up surface". The BRD lists tenant onboarding under PlatformManager/SuperAdmin. The web calls it only from the operator console (`features/operator/tenants`). The `"open signup"` comment in `auth_middleware_arch_test.go` is stale. New gate: `RequirePlatform` + `platform.tenants.create`, plus its own rate limit.
- **D7 – `X-Tenant-Id` (ADR 0039 decision tree).**
  - The header is honoured only for slug-anchored, non-impersonation `is_platform` callers.
  - For everyone else it is **ignored** (the ADR text says "IGNORED"), with a debug log line.
  - Malformed value → 400 `invalid_tenant_scope`. Unknown or deleted tenant → 404 `tenant_scope_not_found`.
  - On `/tenants/{tenantId}` routes the **path wins**, and a header that differs from the path → 400 `tenant_scope_conflict`.
  - Audit is one structured `slog` INFO line per scoped request, using ADR 0039's fields (`scope_mode`, `effective_tenant_id`, `caller_tenant_id`). ADR 0056 defers HTTP-side audit-row writes.
  - Capabilities ignores the header (it uses claims only).
  - Minimum-faithful deviation (owner ruling Q4): without the header, operators act in their own tenant. That means no TxScopePlatform reads and no `ambiguous_scope` 403. Because no-header is never cross-tenant, ADR 0039 Alt-4's blast-radius hazard cannot occur.
- **D8 – `must_change_password` enforcement.** It becomes a JWT claim, as ADR 0053 "Deferred work #1" specifies: a JWT-resident claim plus an allowlist middleware, with impersonation exempt.
  - Login and refresh mint it from the Person row.
  - The allowlist is `POST /auth/change-password` and `GET /auth/me/capabilities`. Logout and refresh are anonymous already.
  - Everything else returns 403 `password_change_required`.
  - The claim cannot go stale, because changing the password rotates the stamp and kills the token.
- **D9 – impersonation tokens (ADR 0045 "Closed-set audience").** They must be refused where they shouldn't be accepted:
  - Self-service identity routes (change-password, email change, sessions, `/me/activity`, permission-request mutations) return 403 `impersonation_not_allowed`.
  - `RequirePlatform` rejects them explicitly.
  - `aud=leadkart-impersonation` must agree with the presence of `act`, otherwise 401.
  - The token is honoured only while its session exists. ADR 0045 step 7 bounds access after the session ends to ≤10 min, but these tokens live up to 4 h, so liveness has to be checked.
  - Their reach comes from D1.
- **D10 – refresh grace window.** A just-rotated token re-presented within `Refresh.GraceWindow` (default 30 s), while its direct successor is still the unconsumed chain head, gets the **same successor** back. No state change and no event.
  - The successor is **derived deterministically**: HMAC-SHA256 with a key derived by HKDF from the JWT signing key, over family id and presented plaintext. So the server never stores plaintext and no schema change is needed.
  - Outside the window, or once the successor has been used, presenting the old token is reuse and the family is revoked (RFC 9700 rotation; Auth0 "reuse interval" / Okta "grace period" canon).
- **D11 – rate limiting.** Stays in memory: ADR 0030 defers Redis, and no Redis limiter wiring exists.
  - Add a trusted-proxy client-IP keyer that reads `X-Forwarded-For` only from trusted CIDRs, walking right-to-left and taking the first untrusted hop.
  - Add configurable tighter per-route limits for login, refresh, request-password-reset, reset-password, confirm-email-change and tenant registration.
  - The idempotency caller scope uses the same client IP.
- **D12 – enumeration.** ADR 0053's argument that "the attacker already knew" is wrong: ten probes against an unknown email never lock, so a 423 reveals that the account exists.
  - Fix: every no-account branch (unknown email, inactive, anonymised) runs a **phantom lockout counter** with the same thresholds. This keeps ADR 0053's 423 UX and restores ADR 0044's non-disclosure.
  - The locked branch runs a dummy argon2 verify, still never verifying the real password.
  - Request-password-reset gets a fixed 300 ms response floor.
- **D13 – password policy.** The stored tenant policy is enforced on create-user (target tenant), change-password (caller's tenant) and reset (active membership's tenant, else default). Registration uses `DefaultPasswordPolicy`.
  - Rule codes: `min_length`, `requires_uppercase`, `requires_lowercase`, `requires_digit`, `requires_symbol`. A symbol is `unicode.IsPunct||IsSymbol`; spaces don't count.
  - Response: 422 `password_policy_violation` with `errors{<field>:[rules]}`.
- **D14 – `WWW-Authenticate`.** Sent on 401 only (RFC 6750 §3). The web's "session expired" detection (401 + the header) keeps working.
- **D15 – new ADR 0068.** ADRs are sealed, so ADR 0068 records D1–D13. Its fitness function is `TestArch_MutatingRoutesRequireAuthorization`.

## Open questions (each has a default so work isn't blocked)

- **Q1.** `GET /users`, `GET /roles` and `GET /tenants/{id}` now need `*.view`. Ordinary members (for example a Sales Executive) lose the tenant directory and tenant profile. **Default: gate them.**
- **Q2.** Is tenant registration operator-only (D6)? **Default: yes.** The alternative is to keep it anonymous with only the route limit.
- **Q3.** Should `X-Tenant-Id` from a non-platform caller be ignored, or rejected with 403? **Default: ignore, per the ADR.**
- **Q4.** Do we accept the D7 deviation (no-header means own tenant; no TxScopePlatform reads; no `ambiguous_scope`)? **Default: accept.**
- **Q5.** The default policy requires all four character classes, which goes against NIST 800-63B composition guidance. **Default: enforce what is stored.** The owner may relax `DefaultPasswordPolicy` later.
- **Q6.** There is no last-CompanyOwner protection in the domain. A `users.deactivate` holder can deactivate the owner. **Default: out of scope now**, recorded in ADR 0068 as follow-up work (a domain invariant).
- **Q7.** The impersonation session store is in memory, so the liveness check (Task 8) is only correct with a single replica. **Default: ship it** (v0.2 is single-replica, ADR 0030); a Redis store is follow-up.
- **Q8.** The repo shows "Cherry-pick currently in progress" with a clean tree and no `CHERRY_PICK_HEAD`. **Default: the owner approves `git cherry-pick --quit`**, which clears the sequencer state and changes no files.

---

## Task 0 — Preflight (no commit)

1. Run `git status`. If it says "Cherry-pick currently in progress", **stop and ask the owner** (Q8). Suggest `git cherry-pick --quit`. Do not run it without their approval.
2. Run `git branch --show-current`. Expect `fix/identity-authz-and-bff-security`.
3. Run `docker version --format '{{.Server.Version}}'`. Expect `29.x`.
4. Run the baseline gate G2–G6 and `task test:int`, and write the markers into your notes. A red baseline is a pre-existing finding: report it and do not fix it inside this plan.

---

## Task 1 — Domain: tenant-admin implication and platform equivalents (D1, D2)

**Files:**
- Create `internal/identity/domain/permission/implication.go`
- Create `internal/identity/domain/permission/implication_test.go`

**Interface produced** (Task 2 consumes it):
```go
func ImpliedByTenantAdmin(name string) bool
func PlatformEquivalents(name string) []string
```

1. Write the failing test `implication_test.go`:
```go
package permission_test

import (
	"slices"
	"testing"

	"github.com/leadkart/leadkart-go/internal/identity/domain/permission"
)

func TestImpliedByTenantAdmin(t *testing.T) {
	t.Parallel()
	p := permission.IdentityPermissions
	cases := []struct {
		name string
		want bool
	}{
		{p.Users.View, true}, {p.Users.UpdatePermissions, true}, {p.Roles.Assign, true},
		{p.Tenants.Update, true}, {p.Tenants.UpdateSettings, true},
		{p.Crm.Leads.ReadAll, true}, {p.Inventory.Stock.Manage, true},
		{p.PlatformMarketplace.Purchase, true}, {p.PlatformLeadCredits.Read, true},
		{p.Meta.TenantAdmin, false},
		{p.Platform.TenantsView, false}, {p.Platform.UsersManage, false}, {p.Platform.RolesManage, false},
		{p.PlatformUnverifiedContacts.Manage, false}, {p.PlatformLeadCredits.Topup, false},
		{p.Tenants.Suspend, false}, {p.Tenants.Activate, false}, {p.Tenants.Delete, false},
		{p.Users.Anonymise, false},
		{"not.a.permission", false}, {"", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := permission.ImpliedByTenantAdmin(tc.name); got != tc.want {
				t.Fatalf("ImpliedByTenantAdmin(%q) = %v, want %v", tc.name, got, tc.want)
			}
		})
	}
}

func TestPlatformEquivalents(t *testing.T) {
	t.Parallel()
	p := permission.IdentityPermissions
	cases := []struct {
		name string
		want []string
	}{
		{p.Users.View, []string{p.Platform.UsersView, p.Platform.UsersManage}},
		{p.Users.Create, []string{p.Platform.UsersCreate, p.Platform.UsersManage}},
		{p.Users.Deactivate, []string{p.Platform.UsersManage}},
		{p.Roles.View, []string{p.Platform.RolesView, p.Platform.RolesManage}},
		{p.Roles.Assign, []string{p.Platform.RolesManage}},
		{p.Tenants.View, []string{p.Platform.TenantsView, p.Platform.TenantsManage}},
		{p.Tenants.UpdateSettings, []string{p.Platform.TenantsManage}},
		{p.Crm.Leads.Read, nil},
		{p.Users.Anonymise, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := permission.PlatformEquivalents(tc.name); !slices.Equal(got, tc.want) {
				t.Fatalf("PlatformEquivalents(%q) = %v, want %v", tc.name, got, tc.want)
			}
		})
	}
}
```
2. Run `go test ./internal/identity/domain/permission/ -run 'TestImpliedByTenantAdmin|TestPlatformEquivalents'`. Expect a compile FAIL: `undefined: permission.ImpliedByTenantAdmin`.
3. Implement `implication.go`:
```go
package permission

import "slices"

// platformReservedNames are catalogue entries that tenant.admin NEVER implies:
// platform-operator authority plus tenant-lifecycle and cross-tenant erasure
// actions reserved for the Platform tier (ADR 0036 + multi-tenancy.md
// "Platform admin endpoints"). tenant.admin itself is listed so a requirement
// for it is satisfied only by holding it explicitly.
func platformReservedNames() []string {
	p := IdentityPermissions
	return []string{
		p.Meta.TenantAdmin,
		p.Platform.TenantsView, p.Platform.TenantsCreate, p.Platform.TenantsManage,
		p.Platform.UsersView, p.Platform.UsersCreate, p.Platform.UsersManage,
		p.Platform.RolesView, p.Platform.RolesManage,
		p.PlatformUnverifiedContacts.Manage,
		p.PlatformLeadCredits.Topup,
		p.Tenants.Suspend, p.Tenants.Activate, p.Tenants.Delete,
		p.Users.Anonymise,
	}
}

// ImpliedByTenantAdmin reports whether holding tenant.admin satisfies a
// requirement for name inside the holder's OWN tenant. Per ADR 0061
// amendment 1 ("CompanyOwner ... short-circuits via Meta.TenantAdmin") and
// ADR 0045 (an impersonating operator gets the full CompanyOwner authority);
// recorded in ADR 0068. Unknown and platform-reserved names return false.
// Callers MUST also check that the effective tenant is the holder's own.
func ImpliedByTenantAdmin(name string) bool {
	if !IsKnown(name) {
		return false
	}
	return !slices.Contains(platformReservedNames(), name)
}

// PlatformEquivalents returns the platform-tier permissions that satisfy a
// tenant-plane identity permission when held by a slug-anchored Platform
// operator on the unified /v1/users, /v1/roles, /v1/tenants surface
// (ADR 0039 Context; mirrors the .NET PlatformTenant*Endpoints gates).
// Nil when name has no platform equivalent.
func PlatformEquivalents(name string) []string {
	p := IdentityPermissions
	switch name {
	case p.Tenants.View:
		return []string{p.Platform.TenantsView, p.Platform.TenantsManage}
	case p.Tenants.Update, p.Tenants.UpdateSettings:
		return []string{p.Platform.TenantsManage}
	case p.Users.View:
		return []string{p.Platform.UsersView, p.Platform.UsersManage}
	case p.Users.Create:
		return []string{p.Platform.UsersCreate, p.Platform.UsersManage}
	case p.Users.Update, p.Users.Deactivate, p.Users.Reactivate, p.Users.Unlock, p.Users.UpdatePermissions:
		return []string{p.Platform.UsersManage}
	case p.Roles.View:
		return []string{p.Platform.RolesView, p.Platform.RolesManage}
	case p.Roles.Create, p.Roles.Update, p.Roles.Delete, p.Roles.Assign, p.Roles.Revoke:
		return []string{p.Platform.RolesManage}
	}
	return nil
}
```
4. Run the test again. Expect `ok`.
5. Run gates G1–G6. G7 does not apply (pure domain).
6. Commit: `feat(identity): tenant.admin implication + platform-equivalent permission semantics (ADR 0036/0061/0039)`.

---

## Task 2 — authn: one authorisation decision and new gates (D1–D3)

**Files:**
- Create `internal/identity/ports/authn/authorize.go`
- Create `internal/identity/ports/authn/authorize_test.go`
- Modify `internal/identity/ports/authn/authn.go`: `RequirePermission`, `RequireAnyPermission`, `RequireTenantContext`

**Interfaces produced** (Tasks 3, 6, 7 and 8 consume them):
```go
func Satisfies(ctx context.Context, permName string) bool
func EffectivePermissions(c *jwt.Claims) []string
func RequireTenantPermission(verifier Verifier, validator StampValidator, pathVar string, permNames ...string) func(http.Handler) http.Handler
func RequirePermissionOrSelf(verifier Verifier, validator StampValidator, pathVar, permName string) func(http.Handler) http.Handler
func DenySelfTarget(pathVar string) func(http.Handler) http.Handler   // inner; requires claims on ctx
// unexported, used by later tasks inside authn:
func isPlatformOperator(c *jwt.Claims) bool
func satisfiesIn(c *jwt.Claims, effective tenancy.ID, required string) bool
```

1. Write the failing tests in `authorize_test.go`, in package `authn_test`, reusing `fakeVerifier`, `alwaysFresh`, `withFreshness`, `sentinel` and `newRequest` from `authn_test.go`:
```go
package authn_test

import (
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/leadkart/leadkart-go/internal/common/tenancy"
	"github.com/leadkart/leadkart-go/internal/identity/app/jwt"
	"github.com/leadkart/leadkart-go/internal/identity/domain/permission"
	"github.com/leadkart/leadkart-go/internal/identity/ports/authn"
)

const (
	tenantA = "0190a000-0000-7000-8000-00000000000a"
	tenantB = "0190a000-0000-7000-8000-00000000000b"
	selfMem = "0190a000-0000-7000-8000-0000000000aa"
)

func TestSatisfies_DecisionTable(t *testing.T) {
	t.Parallel()
	p := permission.IdentityPermissions
	cases := []struct {
		name      string
		claims    *jwt.Claims
		effective string
		perm      string
		want      bool
	}{
		{"explicit grant", &jwt.Claims{TenantID: tenantA, Permissions: []string{p.Users.View}}, tenantA, p.Users.View, true},
		{"missing", &jwt.Claims{TenantID: tenantA}, tenantA, p.Users.View, false},
		{"super user", &jwt.Claims{TenantID: tenantA, IsSuperUser: true}, tenantB, p.Users.Anonymise, true},
		{"tenant.admin own tenant", &jwt.Claims{TenantID: tenantA, Permissions: []string{p.Meta.TenantAdmin}}, tenantA, p.Roles.Assign, true},
		{"tenant.admin other module", &jwt.Claims{TenantID: tenantA, Permissions: []string{p.Meta.TenantAdmin}}, tenantA, p.Crm.Leads.ReadAll, true},
		{"tenant.admin other tenant", &jwt.Claims{TenantID: tenantA, Permissions: []string{p.Meta.TenantAdmin}}, tenantB, p.Roles.Assign, false},
		{"tenant.admin reserved", &jwt.Claims{TenantID: tenantA, Permissions: []string{p.Meta.TenantAdmin}}, tenantA, p.Tenants.Suspend, false},
		{"platform equivalent", &jwt.Claims{TenantID: tenantA, TenantSlug: authn.PlatformTenantSlug, IsPlatform: true, Permissions: []string{p.Platform.UsersView}}, tenantB, p.Users.View, true},
		{"platform equivalent needs slug", &jwt.Claims{TenantID: tenantA, TenantSlug: "acme", IsPlatform: true, Permissions: []string{p.Platform.UsersView}}, tenantB, p.Users.View, false},
		{"platform equivalent not for impersonation", &jwt.Claims{TenantID: tenantA, TenantSlug: authn.PlatformTenantSlug, IsPlatform: true, Permissions: []string{p.Platform.UsersView}, Act: &jwt.ActClaim{Sub: "op"}}, tenantB, p.Users.View, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx := authn.WithClaims(t.Context(), tc.claims)
			ctx = tenancy.WithID(ctx, tenancy.ID(tc.effective))
			if got := authn.Satisfies(ctx, tc.perm); got != tc.want {
				t.Fatalf("Satisfies = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestSatisfies_NoClaims_False(t *testing.T) {
	t.Parallel()
	if authn.Satisfies(t.Context(), permission.IdentityPermissions.Users.View) {
		t.Fatal("Satisfies without claims must be false")
	}
}

func TestEffectivePermissions_TenantAdminExpandsAndSorts(t *testing.T) {
	t.Parallel()
	p := permission.IdentityPermissions
	got := authn.EffectivePermissions(&jwt.Claims{TenantID: tenantA, Permissions: []string{p.Meta.TenantAdmin}})
	if !slices.IsSorted(got) {
		t.Fatal("not sorted")
	}
	for _, want := range []string{p.Meta.TenantAdmin, p.Users.Create, p.Crm.Leads.Read} {
		if !slices.Contains(got, want) {
			t.Errorf("missing %s", want)
		}
	}
	if slices.Contains(got, p.Platform.TenantsManage) {
		t.Error("tenant.admin must not expand to platform.tenants.manage")
	}
}

func TestRequirePermission_TenantAdminPasses(t *testing.T) {
	t.Parallel()
	v := &fakeVerifier{claims: withFreshness(&jwt.Claims{TenantID: tenantA, Permissions: []string{permission.IdentityPermissions.Meta.TenantAdmin}})}
	s := &sentinel{}
	h := authn.RequirePermission(v, alwaysFresh{}, permission.IdentityPermissions.Users.View)(s.handler())
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, newRequest(t, "Bearer tok"))
	if rec.Code != http.StatusOK || !s.called {
		t.Fatalf("status %d called %v, want 200 true", rec.Code, s.called)
	}
}

func tenantReq(t *testing.T, tenantID string) *http.Request {
	t.Helper()
	mux := http.NewServeMux()
	var captured *http.Request
	mux.HandleFunc("GET /t/{tenantId}", func(_ http.ResponseWriter, r *http.Request) { captured = r })
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/t/"+tenantID, nil)
	mux.ServeHTTP(httptest.NewRecorder(), req)
	captured.Header.Set("Authorization", "Bearer tok")
	return captured
}

func TestRequireTenantPermission(t *testing.T) {
	t.Parallel()
	p := permission.IdentityPermissions
	cases := []struct {
		name   string
		claims *jwt.Claims
		path   string
		want   int
	}{
		{"owner own tenant", &jwt.Claims{TenantID: tenantA, Permissions: []string{p.Meta.TenantAdmin}}, tenantA, 200},
		{"member own tenant no perm", &jwt.Claims{TenantID: tenantA}, tenantA, 403},
		{"owner other tenant", &jwt.Claims{TenantID: tenantA, Permissions: []string{p.Meta.TenantAdmin}}, tenantB, 403},
		{"platform view equivalent", &jwt.Claims{TenantID: tenantA, TenantSlug: authn.PlatformTenantSlug, IsPlatform: true, Permissions: []string{p.Platform.TenantsView}}, tenantB, 200},
		{"platform without perm", &jwt.Claims{TenantID: tenantA, TenantSlug: authn.PlatformTenantSlug, IsPlatform: true}, tenantB, 403},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			v := &fakeVerifier{claims: withFreshness(tc.claims)}
			s := &sentinel{}
			h := authn.RequireTenantPermission(v, alwaysFresh{}, "tenantId", p.Tenants.View)(s.handler())
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, tenantReq(t, tc.path))
			if rec.Code != tc.want {
				t.Fatalf("status %d, want %d (body %s)", rec.Code, tc.want, rec.Body.String())
			}
		})
	}
}

func userReq(t *testing.T, userID string) *http.Request {
	t.Helper()
	mux := http.NewServeMux()
	var captured *http.Request
	mux.HandleFunc("GET /u/{userId}", func(_ http.ResponseWriter, r *http.Request) { captured = r })
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/u/"+userID, nil)
	mux.ServeHTTP(httptest.NewRecorder(), req)
	captured.Header.Set("Authorization", "Bearer tok")
	return captured
}

func TestRequirePermissionOrSelf(t *testing.T) {
	t.Parallel()
	p := permission.IdentityPermissions
	member := func() *jwt.Claims { return withFreshness(&jwt.Claims{TenantID: tenantA, MembershipID: selfMem}) }
	for _, tc := range []struct {
		name   string
		claims *jwt.Claims
		path   string
		want   int
	}{
		{"self", member(), selfMem, 200},
		{"other without perm", member(), "0190a000-0000-7000-8000-0000000000bb", 403},
		{"other with perm", withFreshness(&jwt.Claims{TenantID: tenantA, MembershipID: selfMem, Permissions: []string{p.Users.View}}), "0190a000-0000-7000-8000-0000000000bb", 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := &sentinel{}
			h := authn.RequirePermissionOrSelf(&fakeVerifier{claims: tc.claims}, alwaysFresh{}, "userId", p.Users.View)(s.handler())
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, userReq(t, tc.path))
			if rec.Code != tc.want {
				t.Fatalf("status %d want %d", rec.Code, tc.want)
			}
		})
	}
}

func TestDenySelfTarget(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		claims *jwt.Claims
		path   string
		want   int
	}{
		{"self denied", &jwt.Claims{TenantID: tenantA, MembershipID: selfMem}, selfMem, 403},
		{"other allowed", &jwt.Claims{TenantID: tenantA, MembershipID: selfMem}, "0190a000-0000-7000-8000-0000000000bb", 200},
		{"super user self allowed", &jwt.Claims{TenantID: tenantA, MembershipID: selfMem, IsSuperUser: true}, selfMem, 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := &sentinel{}
			h := authn.RequireFreshStamp(&fakeVerifier{claims: withFreshness(tc.claims)}, alwaysFresh{})(authn.DenySelfTarget("userId")(s.handler()))
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, userReq(t, tc.path))
			if rec.Code != tc.want {
				t.Fatalf("status %d want %d", rec.Code, tc.want)
			}
			if tc.want == 403 && !strings.Contains(rec.Body.String(), "self_modification_forbidden") {
				t.Fatalf("body %s", rec.Body.String())
			}
		})
	}
}
```
2. Run `go test ./internal/identity/ports/authn/`. Expect a compile FAIL on the undefined symbols.
3. Implement `authorize.go`:
```go
package authn

import (
	"context"
	"net/http"
	"slices"
	"strings"

	"github.com/leadkart/leadkart-go/internal/common/tenancy"
	"github.com/leadkart/leadkart-go/internal/identity/app/jwt"
	"github.com/leadkart/leadkart-go/internal/identity/domain/permission"
)

const errCodeSelfModificationForbidden = "self_modification_forbidden"

// isPlatformOperator: is_platform AND slug-anchored AND not an impersonation
// token (ADR 0039 slug anchor; ADR 0045 downgrade).
func isPlatformOperator(c *jwt.Claims) bool {
	return c != nil && c.IsPlatform && c.TenantSlug == PlatformTenantSlug && !c.IsImpersonation()
}

// satisfiesIn is THE authorization decision (ADR 0036 + ADR 0068):
// super user → yes; explicit grant → yes; tenant.admin implies every
// non-reserved permission in the holder's OWN tenant; a platform operator's
// platform.* grant satisfies the identity.* equivalent on any tenant.
func satisfiesIn(c *jwt.Claims, effective tenancy.ID, required string) bool {
	if c == nil {
		return false
	}
	if c.IsSuperUser || slices.Contains(c.Permissions, required) {
		return true
	}
	if tenancy.ID(c.TenantID) == effective &&
		slices.Contains(c.Permissions, permission.IdentityPermissions.Meta.TenantAdmin) &&
		permission.ImpliedByTenantAdmin(required) {
		return true
	}
	if isPlatformOperator(c) {
		for _, eq := range permission.PlatformEquivalents(required) {
			if slices.Contains(c.Permissions, eq) {
				return true
			}
		}
	}
	return false
}

// Satisfies reports whether the authenticated caller on ctx may exercise
// permName in the request's effective tenant (tenancy ctx bound by
// RequireAuth; falls back to the claims tenant). Use for handler-inline
// checks (e.g. read_all narrowing) in every module. False without claims.
func Satisfies(ctx context.Context, permName string) bool {
	c, ok := ClaimsFromContext(ctx)
	if !ok {
		return false
	}
	effective, ok := tenancy.FromContext(ctx)
	if !ok || effective.IsZero() {
		effective = tenancy.ID(c.TenantID)
	}
	return satisfiesIn(c, effective, permName)
}

// EffectivePermissions lists every catalogue permission c satisfies in its
// own tenant, sorted — the capabilities projection the frontend gates on.
func EffectivePermissions(c *jwt.Claims) []string {
	out := []string{}
	if c == nil {
		return out
	}
	for _, p := range permission.All() {
		if satisfiesIn(c, tenancy.ID(c.TenantID), p.Name()) {
			out = append(out, p.Name())
		}
	}
	slices.Sort(out)
	return out
}

// RequireTenantPermission gates /tenants/{pathVar}/... routes: freshness-
// checked JWT AND (path tenant == claims tenant OR caller is super user /
// platform operator) AND at least one of permNames satisfied IN THE PATH
// TENANT. Replaces RequireTenantContext for routes that need a permission.
func RequireTenantPermission(verifier Verifier, validator StampValidator, pathVar string, permNames ...string) func(http.Handler) http.Handler {
	if verifier == nil || validator == nil {
		panic("authn: RequireTenantPermission verifier + validator required")
	}
	if strings.TrimSpace(pathVar) == "" {
		panic("authn: RequireTenantPermission pathVar required")
	}
	if len(permNames) == 0 {
		panic("authn: RequireTenantPermission requires ≥1 permission name")
	}
	for _, p := range permNames {
		_ = permission.FromConstant(p)
	}
	auth := RequireFreshStamp(verifier, validator)
	return func(next http.Handler) http.Handler {
		return auth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			c, ok := ClaimsFromContext(r.Context())
			if !ok {
				writeError(w, http.StatusUnauthorized, errCodeUnauthenticated, "missing claims")
				return
			}
			urlTenant := strings.TrimSpace(r.PathValue(pathVar))
			if urlTenant == "" {
				writeError(w, http.StatusBadRequest, errCodeForbidden, "path missing "+pathVar+" parameter")
				return
			}
			if !c.IsSuperUser && !isPlatformOperator(c) && urlTenant != strings.TrimSpace(c.TenantID) {
				writeError(w, http.StatusForbidden, errCodeForbidden, "tenant context does not match url")
				return
			}
			for _, p := range permNames {
				if satisfiesIn(c, tenancy.ID(urlTenant), p) {
					next.ServeHTTP(w, r)
					return
				}
			}
			writeError(w, http.StatusForbidden, errCodeForbidden, "missing any of: "+strings.Join(permNames, ", "))
		}))
	}
}

// RequirePermissionOrSelf passes when the path's membership id is the
// caller's own (self-service read/profile) OR permName is satisfied.
func RequirePermissionOrSelf(verifier Verifier, validator StampValidator, pathVar, permName string) func(http.Handler) http.Handler {
	if verifier == nil || validator == nil {
		panic("authn: RequirePermissionOrSelf verifier + validator required")
	}
	if strings.TrimSpace(pathVar) == "" {
		panic("authn: RequirePermissionOrSelf pathVar required")
	}
	_ = permission.FromConstant(permName)
	auth := RequireFreshStamp(verifier, validator)
	return func(next http.Handler) http.Handler {
		return auth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			c, ok := ClaimsFromContext(r.Context())
			if !ok {
				writeError(w, http.StatusUnauthorized, errCodeUnauthenticated, "missing claims")
				return
			}
			if c.MembershipID != "" && strings.TrimSpace(r.PathValue(pathVar)) == c.MembershipID {
				next.ServeHTTP(w, r)
				return
			}
			if Satisfies(r.Context(), permName) {
				next.ServeHTTP(w, r)
				return
			}
			writeError(w, http.StatusForbidden, errCodeForbidden, "missing permission: "+permName)
		}))
	}
}

// DenySelfTarget refuses requests whose path membership id is the caller's
// own (no self-assigned roles/overrides/status/manager — ADR 0068 D3).
// Super users are exempt. INNER middleware: compose inside an auth gate.
func DenySelfTarget(pathVar string) func(http.Handler) http.Handler {
	if strings.TrimSpace(pathVar) == "" {
		panic("authn: DenySelfTarget pathVar required")
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			c, ok := ClaimsFromContext(r.Context())
			if !ok {
				writeError(w, http.StatusUnauthorized, errCodeUnauthenticated, "missing claims")
				return
			}
			if !c.IsSuperUser && c.MembershipID != "" && strings.TrimSpace(r.PathValue(pathVar)) == c.MembershipID {
				writeError(w, http.StatusForbidden, errCodeSelfModificationForbidden,
					"callers cannot change their own roles, permissions, status or manager")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
```
4. In `authn.go`, change the decision code in three places:
   - **`RequirePermission`:** replace the `if c.IsSuperUser {...}` block and the `slices.Contains` check with:
     ```go
     if !Satisfies(r.Context(), permName) {
         writeError(w, http.StatusForbidden, errCodeForbidden, "missing permission: "+permName)
         return
     }
     next.ServeHTTP(w, r)
     ```
   - **`RequireAnyPermission`:** replace the super-user block and the loop with:
     ```go
     for _, p := range permNames {
         if Satisfies(r.Context(), p) { next.ServeHTTP(w, r); return }
     }
     ```
   - **`RequireTenantContext`:** replace `c.IsPlatform && c.TenantSlug == PlatformTenantSlug` with `isPlatformOperator(c)`.
   - Drop the `slices` import if it becomes unused.
5. Run `go test ./internal/identity/ports/authn/`. Expect `ok`. All existing tests stay green, including `TestRequirePermission_SuperUser_BypassesCheck`.
6. Run G1–G6.
7. Commit: `feat(authn): single Satisfies decision + RequireTenantPermission/RequirePermissionOrSelf/DenySelfTarget (ADR 0036/0068)`.

---

## Task 3 — Gate every identity route, block grants beyond the caller's authority, expose effective_permissions (item 1)

**Files:**
- Modify `internal/identity/ports/http.go` (AddRoutes block)
- Create `internal/identity/ports/authority.go`
- Modify `internal/identity/ports/users_http.go`: `handleAssignUserRole`, `handleReplaceUserPermissionOverrides`
- Modify `internal/identity/ports/roles_http.go`: `handleReplaceRolePermissions`, `handleGrantRolePermission`
- Modify `internal/identity/ports/errcodes.go`
- Modify `internal/identity/ports/dto.go` (`CapabilitiesDto`)
- Modify `internal/identity/ports/me_http.go`
- Modify `internal/crm/ports/http.go` (line ~140), `internal/crm/ports/http_reminders.go` (~134), `internal/tasks/ports/http.go` (~181, ~372)
- Modify `api/openapi.yaml`
- Create `internal/identity/ports/route_authz_test.go`
- Create `cmd/api/authz_e2e_test.go`

**Depends on** Task 2: `authn.Satisfies`, `RequireTenantPermission`, `RequirePermissionOrSelf`, `DenySelfTarget`, `EffectivePermissions`.

1. Write the failing unit test `route_authz_test.go`. It gives a gate-only proof without Docker. The handlers run against a zero `app.Application`, so a request that passes the gate panics and `httpmw.Recover` turns that into 500. Any status other than 401/403 therefore means the gate passed.
```go
package ports_test

import (
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/leadkart/leadkart-go/internal/common/httpmw"
	"github.com/leadkart/leadkart-go/internal/identity/app"
	"github.com/leadkart/leadkart-go/internal/identity/app/jwt"
	"github.com/leadkart/leadkart-go/internal/identity/domain/permission"
	"github.com/leadkart/leadkart-go/internal/identity/ports"
)

type tokenVerifier map[string]*jwt.Claims

func (v tokenVerifier) Verify(tok string) (*jwt.Claims, error) {
	if c, ok := v[tok]; ok {
		return c, nil
	}
	return nil, errors.New("unknown token")
}

const (
	azTenant = "0190a000-0000-7000-8000-000000000001"
	azSelf   = "0190a000-0000-7000-8000-0000000000aa"
	azOther  = "0190a000-0000-7000-8000-0000000000bb"
	azRole   = "0190a000-0000-7000-8000-0000000000cc"
)

func azClaims(perms ...string) *jwt.Claims {
	c := &jwt.Claims{TenantID: azTenant, TenantSlug: "acme", MembershipID: azSelf, SecurityStamp: "stamp", Permissions: perms}
	c.Subject = "0190a000-0000-7000-8000-0000000000ff"
	return c
}

func azServe(t *testing.T, c *jwt.Claims, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	mux := http.NewServeMux()
	ports.AddRoutes(mux, logger, app.Application{}, tokenVerifier{"tok": c}, stubStampValidator{})
	req := httptest.NewRequestWithContext(t.Context(), method, path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer tok")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	httpmw.Recover(logger)(mux).ServeHTTP(rec, req)
	return rec
}

func gatePassed(code int) bool { return code != http.StatusUnauthorized && code != http.StatusForbidden }

func TestRouteAuthz_IdentityRoutesRequirePermissions(t *testing.T) {
	t.Parallel()
	p := permission.IdentityPermissions
	u, r, tn := "/api/v1/users/", "/api/v1/roles/", "/api/v1/tenants/"+azTenant
	routes := []struct{ method, path, perm string }{
		{"GET", "/api/v1/users", p.Users.View},
		{"GET", u + azOther, p.Users.View},
		{"PATCH", u + azOther + "/profile", p.Users.Update},
		{"POST", u + azOther + "/deactivate", p.Users.Deactivate},
		{"POST", u + azOther + "/reactivate", p.Users.Reactivate},
		{"POST", u + azOther + "/roles", p.Roles.Assign},
		{"DELETE", u + azOther + "/roles/" + azRole, p.Roles.Revoke},
		{"PATCH", u + azOther + "/permission-overrides", p.Users.UpdatePermissions},
		{"PUT", u + azOther + "/manager", p.Users.Update},
		{"DELETE", u + azOther + "/manager", p.Users.Update},
		{"POST", "/api/v1/users", p.Users.Create},
		{"GET", "/api/v1/roles", p.Roles.View},
		{"GET", r + azRole, p.Roles.View},
		{"POST", "/api/v1/roles", p.Roles.Create},
		{"PATCH", r + azRole, p.Roles.Update},
		{"PUT", r + azRole + "/permissions", p.Roles.Update},
		{"POST", r + azRole + "/permissions/grant", p.Roles.Update},
		{"POST", r + azRole + "/permissions/revoke", p.Roles.Update},
		{"PATCH", r + azRole + "/parent", p.Roles.Update},
		{"DELETE", r + azRole, p.Roles.Delete},
		{"GET", tn, p.Tenants.View},
		{"PATCH", tn + "/profile", p.Tenants.Update},
		{"PATCH", tn + "/statutory", p.Tenants.Update},
		{"PATCH", tn + "/admin-contact", p.Tenants.Update},
		{"PATCH", tn + "/settings", p.Tenants.UpdateSettings},
		{"PATCH", tn + "/display-preferences", p.Tenants.Update},
		{"GET", tn + "/audit/events", p.Tenants.View},
	}
	for _, rt := range routes {
		t.Run(rt.method+" "+rt.path, func(t *testing.T) {
			t.Parallel()
			if rec := azServe(t, azClaims(), rt.method, rt.path, "{}"); rec.Code != http.StatusForbidden {
				t.Errorf("member without %s: status %d, want 403", rt.perm, rec.Code)
			}
			if rec := azServe(t, azClaims(rt.perm), rt.method, rt.path, "{}"); !gatePassed(rec.Code) {
				t.Errorf("holder of %s: status %d, want gate pass (body %s)", rt.perm, rec.Code, rec.Body.String())
			}
			if rec := azServe(t, azClaims(p.Meta.TenantAdmin), rt.method, rt.path, "{}"); !gatePassed(rec.Code) {
				t.Errorf("tenant.admin: status %d, want gate pass (body %s)", rec.Code, rec.Body.String())
			}
		})
	}
}

func TestRouteAuthz_SelfServiceAndSelfTarget(t *testing.T) {
	t.Parallel()
	p := permission.IdentityPermissions
	u := "/api/v1/users/" + azSelf
	for _, rt := range []struct{ method, path string }{{"GET", u}, {"PATCH", u + "/profile"}} {
		if rec := azServe(t, azClaims(), rt.method, rt.path, "{}"); !gatePassed(rec.Code) {
			t.Errorf("self %s %s: status %d, want gate pass", rt.method, rt.path, rec.Code)
		}
	}
	for _, rt := range []struct{ method, path string }{
		{"POST", u + "/deactivate"}, {"POST", u + "/reactivate"}, {"POST", u + "/roles"},
		{"DELETE", u + "/roles/" + azRole}, {"PATCH", u + "/permission-overrides"},
		{"PUT", u + "/manager"}, {"DELETE", u + "/manager"},
	} {
		rec := azServe(t, azClaims(p.Meta.TenantAdmin), rt.method, rt.path, "{}")
		if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "self_modification_forbidden") {
			t.Errorf("self-target %s %s: status %d body %s, want 403 self_modification_forbidden", rt.method, rt.path, rec.Code, rec.Body.String())
		}
	}
}

func TestRouteAuthz_GrantBeyondAuthorityRejected(t *testing.T) {
	t.Parallel()
	p := permission.IdentityPermissions
	rec := azServe(t, azClaims(p.Users.UpdatePermissions), "PATCH", "/api/v1/users/"+azOther+"/permission-overrides",
		`{"granted":["tenant.admin"],"revoked":[]}`)
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "grant_exceeds_authority") {
		t.Fatalf("override escalation: %d %s", rec.Code, rec.Body.String())
	}
	rec = azServe(t, azClaims(p.Roles.Update), "POST", "/api/v1/roles/"+azRole+"/permissions/grant", `{"permission":"identity.users.view"}`)
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "grant_exceeds_authority") {
		t.Fatalf("role grant escalation: %d %s", rec.Code, rec.Body.String())
	}
	rec = azServe(t, azClaims(p.Roles.Update), "PUT", "/api/v1/roles/"+azRole+"/permissions", `{"permissions":["identity.users.view"]}`)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("role replace escalation: %d %s", rec.Code, rec.Body.String())
	}
}
```
2. Run `go test ./internal/identity/ports/ -run TestRouteAuthz`. Expect FAIL: members get non-403 responses on routes that today only check authentication.
3. Rewire `http.go`: replace the block from `tenantCtx := ...` through the role routes, plus the audit route, as below. The self-service `auth(...)` routes, `platform` routes and permission-request routes stay unchanged. Add the import `"github.com/leadkart/leadkart-go/internal/identity/domain/permission"`.
```go
		p := permission.IdentityPermissions
		tenantView := authn.RequireTenantPermission(verifier, stampValidator, "tenantId", p.Tenants.View)
		tenantUpdate := authn.RequireTenantPermission(verifier, stampValidator, "tenantId", p.Tenants.Update)
		tenantSettings := authn.RequireTenantPermission(verifier, stampValidator, "tenantId", p.Tenants.UpdateSettings)
		mux.Handle("GET /api/v1/tenants/{tenantId}", tenantView(handleGetTenant(log, a)))
		mux.Handle("PATCH /api/v1/tenants/{tenantId}/profile", tenantUpdate(handleUpdateTenantProfile(log, a)))
		mux.Handle("PATCH /api/v1/tenants/{tenantId}/statutory", tenantUpdate(handleUpdateTenantStatutory(log, a)))
		mux.Handle("PATCH /api/v1/tenants/{tenantId}/admin-contact", tenantUpdate(handleUpdateTenantAdminContact(log, a)))
		mux.Handle("PATCH /api/v1/tenants/{tenantId}/settings", tenantSettings(handleUpdateTenantSettings(log, a)))
		mux.Handle("PATCH /api/v1/tenants/{tenantId}/display-preferences", tenantUpdate(handleUpdateTenantDisplayPreferences(log, a)))

		// (platform := ... lifecycle block unchanged)

		// User (Membership) management — permission-gated per ADR 0036/0068.
		// notSelf forbids self-assigned roles/overrides/status/manager (D3).
		usersView := authn.RequirePermission(verifier, stampValidator, p.Users.View)
		usersCreate := authn.RequirePermission(verifier, stampValidator, p.Users.Create)
		usersUpdate := authn.RequirePermission(verifier, stampValidator, p.Users.Update)
		usersDeactivate := authn.RequirePermission(verifier, stampValidator, p.Users.Deactivate)
		usersReactivate := authn.RequirePermission(verifier, stampValidator, p.Users.Reactivate)
		usersUpdatePermissions := authn.RequirePermission(verifier, stampValidator, p.Users.UpdatePermissions)
		rolesAssign := authn.RequirePermission(verifier, stampValidator, p.Roles.Assign)
		rolesRevoke := authn.RequirePermission(verifier, stampValidator, p.Roles.Revoke)
		userSelfOrView := authn.RequirePermissionOrSelf(verifier, stampValidator, "userId", p.Users.View)
		userSelfOrUpdate := authn.RequirePermissionOrSelf(verifier, stampValidator, "userId", p.Users.Update)
		notSelf := authn.DenySelfTarget("userId")
		mux.Handle("GET /api/v1/users", usersView(handleListUsers(log, a)))
		mux.Handle("GET /api/v1/users/{userId}", userSelfOrView(handleGetUser(log, a)))
		mux.Handle("PATCH /api/v1/users/{userId}/profile", userSelfOrUpdate(handleUpdateUserProfile(log, a)))
		mux.Handle("POST /api/v1/users/{userId}/deactivate", usersDeactivate(notSelf(handleDeactivateUser(log, a))))
		mux.Handle("POST /api/v1/users/{userId}/reactivate", usersReactivate(notSelf(handleReactivateUser(log, a))))
		mux.Handle("POST /api/v1/users/{userId}/roles", rolesAssign(notSelf(handleAssignUserRole(log, a))))
		mux.Handle("DELETE /api/v1/users/{userId}/roles/{roleId}", rolesRevoke(notSelf(handleRevokeUserRole(log, a))))
		mux.Handle("PATCH /api/v1/users/{userId}/permission-overrides", usersUpdatePermissions(notSelf(handleReplaceUserPermissionOverrides(log, a))))
		mux.Handle("PUT /api/v1/users/{userId}/manager", usersUpdate(notSelf(handleAssignUserManager(log, a))))
		mux.Handle("DELETE /api/v1/users/{userId}/manager", usersUpdate(notSelf(handleRemoveUserManager(log, a))))
		mux.Handle("POST /api/v1/users", usersCreate(handleCreateUser(log, a)))
		mux.Handle("POST /api/v1/users/{userId}/anonymise", platform(handleAnonymiseUser(log, a)))

		rolesView := authn.RequirePermission(verifier, stampValidator, p.Roles.View)
		rolesCreate := authn.RequirePermission(verifier, stampValidator, p.Roles.Create)
		rolesUpdate := authn.RequirePermission(verifier, stampValidator, p.Roles.Update)
		rolesDelete := authn.RequirePermission(verifier, stampValidator, p.Roles.Delete)
		mux.Handle("GET /api/v1/roles", rolesView(handleListRoles(log, a)))
		mux.Handle("GET /api/v1/roles/{roleId}", rolesView(handleGetRole(log, a)))
		mux.Handle("POST /api/v1/roles", rolesCreate(handleCreateRole(log, a)))
		mux.Handle("PATCH /api/v1/roles/{roleId}", rolesUpdate(handleUpdateRole(log, a)))
		mux.Handle("PUT /api/v1/roles/{roleId}/permissions", rolesUpdate(handleReplaceRolePermissions(log, a)))
		mux.Handle("POST /api/v1/roles/{roleId}/permissions/grant", rolesUpdate(handleGrantRolePermission(log, a)))
		mux.Handle("POST /api/v1/roles/{roleId}/permissions/revoke", rolesUpdate(handleRevokeRolePermission(log, a)))
		mux.Handle("PATCH /api/v1/roles/{roleId}/parent", rolesUpdate(handleSetRoleParent(log, a)))
		mux.Handle("DELETE /api/v1/roles/{roleId}", rolesDelete(handleDeleteRole(log, a)))
		// ...
		mux.Handle("GET /api/v1/tenants/{tenantId}/audit/events", tenantView(handleListTenantAuditEvents(log, a)))
```
   Delete the now-unused `tenantCtx` variable.
4. Create `authority.go` and add the error code:
```go
package ports

import (
	"net/http"
	"strings"

	"github.com/leadkart/leadkart-go/internal/identity/domain/permission"
	"github.com/leadkart/leadkart-go/internal/identity/ports/authn"
)

// requireGrantWithinAuthority enforces "no grant beyond your own authority"
// (ADR 0068 D4): a caller may confer only catalogue permissions they satisfy
// in the effective tenant. Unknown names are skipped so the command still
// answers 422 permission_unknown. Writes 403 + returns false on violation.
func requireGrantWithinAuthority(w http.ResponseWriter, r *http.Request, names []string) bool {
	for _, raw := range names {
		n := strings.TrimSpace(raw)
		if !permission.IsKnown(n) {
			continue
		}
		if !authn.Satisfies(r.Context(), n) {
			writeError(w, http.StatusForbidden, ErrCodeGrantExceedsAuthority, "caller cannot grant "+n)
			return false
		}
	}
	return true
}
```
   In `errcodes.go`, add:
   ```go
   // ErrCodeGrantExceedsAuthority — caller tried to confer a permission (directly, via role permission edit, or via role assignment) they do not hold (ADR 0068 D4). 403.
   ErrCodeGrantExceedsAuthority = "grant_exceeds_authority"
   ```
5. Wire the authority check into the handlers:
   - **`handleReplaceUserPermissionOverrides`:** after the JSON decode, add `if !requireGrantWithinAuthority(w, r, req.Granted) { return }`.
   - **`handleGrantRolePermission`:** after the decode, add `if !requireGrantWithinAuthority(w, r, []string{req.Permission}) { return }`.
   - **`handleReplaceRolePermissions`:** after the decode, add `if !requireGrantWithinAuthority(w, r, req.Permissions) { return }`. This is conservative: the whole replacement set must be within the caller's authority.
   - **`handleAssignUserRole`:** after the `uuid.Parse(req.RoleID)` check, add the block below and add the `query` import:
```go
		view, rerr := a.Queries.GetRole.Handle(r.Context(), query.GetRoleQuery{TenantID: tenant.ID(tid), RoleID: role.ID(req.RoleID)})
		switch {
		case errors.Is(rerr, role.ErrNotFound):
			writeError(w, http.StatusNotFound, ErrCodeRoleNotFound, "")
			return
		case rerr != nil:
			log.ErrorContext(r.Context(), "assign user role: load role failed", "err", rerr)
			writeError(w, http.StatusInternalServerError, ErrCodeInternalError, "")
			return
		}
		if view.IsSuperAdmin {
			if c, ok := authn.ClaimsFromContext(r.Context()); !ok || !c.IsSuperUser {
				writeError(w, http.StatusForbidden, ErrCodeGrantExceedsAuthority, "only a super user can assign a super-admin role")
				return
			}
		}
		if !requireGrantWithinAuthority(w, r, view.Permissions) {
			return
		}
```
6. Make the other modules' inline checks honour D1.
   - In `crm/ports/http.go` and `http_reminders.go`, replace `!slices.Contains(c.Permissions, permission.IdentityPermissions.Crm.Leads.ReadAll)` with `!authn.Satisfies(r.Context(), permission.IdentityPermissions.Crm.Leads.ReadAll)`.
   - In `tasks/ports/http.go`, make the same change at both sites with `Tasks.WorkItems.ReadAll`.
   - Remove the `slices` imports if they become unused.
7. Capabilities:
   - In `dto.go`, add `EffectivePermissions []string `json:"effective_permissions"`` to `CapabilitiesDto`.
   - In `me_http.go`, set `EffectivePermissions: authn.EffectivePermissions(c),` in the dto literal.
   - Add a unit test in the existing capabilities test file (or a new `me_http_test.go`). It serves `GET /api/v1/auth/me/capabilities` through `azServe` with `azClaims(p.Meta.TenantAdmin)` and asserts that the JSON `effective_permissions` contains `identity.users.view`. The enrichment fails on the zero app, which takes the degraded path and still returns 200.
8. OpenAPI (`api/openapi.yaml`):
   - **Each route rewired in step 3:** add `'403': { $ref: '#/components/responses/Forbidden' }` and `x-required-permissions: [<perm>]`. Use `[identity.users.view, self]` / `[identity.users.update, self]` for the two self-or routes. Append a description line: "Requires `<perm>` (satisfied by `tenant.admin` in the caller's own tenant, or by `platform.*` equivalents for platform operators — ADR 0068). Self-target forbidden (`self_modification_forbidden`)." Example:
```yaml
  /api/v1/users:
    get:
      tags: [users]
      summary: List active members of the caller's tenant (cursor-paginated)
      description: Requires `identity.users.view` (implied by `tenant.admin` in the caller's own tenant; `platform.users.view|manage` for platform operators). ADR 0068.
      x-required-permissions: [identity.users.view]
      parameters:
        - $ref: '#/components/parameters/Cursor'
        - $ref: '#/components/parameters/PageSize'
      responses:
        '200':
          description: Users page
          content:
            application/json:
              schema: { $ref: '#/components/schemas/ListUsersResponse' }
        '401': { $ref: '#/components/responses/Unauthorized' }
        '403': { $ref: '#/components/responses/Forbidden' }
```
   - **Role assign/grant/replace and overrides:** mention `grant_exceeds_authority`.
   - **`CapabilitiesDto`:** add `effective_permissions: { type: array, items: { type: string }, description: 'Every catalogue permission the caller satisfies in their own tenant (tenant.admin expanded). Use this — not permissions[] — for UI gating.' }` and add it to `required`.
   - **`info.description`:** add an `**Authorization error codes:**` paragraph listing `forbidden`, `self_modification_forbidden` and `grant_exceeds_authority`.
9. Write the integration test `cmd/api/authz_e2e_test.go`. Copy the `//go:build integration` header and the `arch-test:no-timeout-needed` marker comment from `cross_tenant_e2e_test.go`.
```go
//go:build integration

package main

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/leadkart/leadkart-go/internal/common/ids"
	"github.com/leadkart/leadkart-go/internal/identity/domain/role"
	"github.com/leadkart/leadkart-go/internal/identity/ports"
)

func (f e2eFixture) createMemberAndLogin(t *testing.T, owner registeredTenant, prefix string) (string, string) {
	t.Helper()
	id := ids.NewV7().String()
	suffix := id[len(id)-8:]
	email := prefix + "-" + suffix + "@e2e.test"
	password := "Member-Pass-2026!" + suffix
	resp := f.authedJSON(t, http.MethodPost, "/api/v1/users", owner.AccessToken,
		ports.CreateUserRequest{Email: email, Password: password, FirstName: prefix, LastName: "Member"})
	if resp.status != http.StatusCreated {
		t.Fatalf("create member: %d %s", resp.status, resp.body)
	}
	var cu ports.CreateUserResponse
	if err := json.Unmarshal(resp.body, &cu); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return cu.MembershipID, f.loginToken(t, email, password)
}

func (f e2eFixture) loginToken(t *testing.T, email, password string) string {
	t.Helper()
	resp := f.postJSON(t, "/api/v1/auth/login", ports.LoginRequest{Email: email, Password: password, DeviceLabel: "e2e"})
	if resp.status != http.StatusOK {
		t.Fatalf("login %s: %d %s", email, resp.status, resp.body)
	}
	var lr ports.LoginResponse
	if err := json.Unmarshal(resp.body, &lr); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return lr.AccessToken
}

func (f e2eFixture) roleIDByName(t *testing.T, token, name string) string {
	t.Helper()
	resp := f.authedJSON(t, http.MethodGet, "/api/v1/roles", token, nil)
	if resp.status != http.StatusOK {
		t.Fatalf("list roles: %d %s", resp.status, resp.body)
	}
	var lr ports.ListRolesResponse
	if err := json.Unmarshal(resp.body, &lr); err != nil {
		t.Fatalf("decode: %v", err)
	}
	for _, r := range lr.Roles {
		if r.Name == name {
			return r.ID
		}
	}
	t.Fatalf("role %q not found", name)
	return ""
}

func TestE2E_Authz_MemberCannotEscalateButCanSelfServe(t *testing.T) {
	f := newE2EFixture(t)
	owner := f.registerAndLogin(t, "authz")
	memberID, memberTok := f.createMemberAndLogin(t, owner, "member")
	ownerRole := f.roleIDByName(t, owner.AccessToken, role.SystemRoles.Tenant.CompanyOwner)

	denied := []struct {
		method, path string
		body         any
	}{
		{http.MethodGet, "/api/v1/users", nil},
		{http.MethodGet, "/api/v1/roles", nil},
		{http.MethodPost, "/api/v1/users/" + memberID + "/roles", ports.AssignUserRoleRequest{RoleID: ownerRole}},
		{http.MethodPatch, "/api/v1/users/" + memberID + "/permission-overrides", ports.ReplaceUserPermissionOverridesRequest{Granted: []string{"tenant.admin"}, Revoked: []string{}}},
		{http.MethodPost, "/api/v1/roles", ports.CreateRoleRequest{Name: "Escalator", HierarchyLevel: 50}},
		{http.MethodPost, "/api/v1/roles/" + ownerRole + "/permissions/grant", ports.RolePermissionRequest{Permission: "identity.users.view"}},
		{http.MethodGet, "/api/v1/tenants/" + owner.TenantID, nil},
		{http.MethodGet, "/api/v1/tenants/" + owner.TenantID + "/audit/events", nil},
	}
	for _, d := range denied {
		if resp := f.authedJSON(t, d.method, d.path, memberTok, d.body); resp.status != http.StatusForbidden {
			t.Errorf("member %s %s: %d %s, want 403", d.method, d.path, resp.status, resp.body)
		}
	}
	if resp := f.authedJSON(t, http.MethodGet, "/api/v1/users/"+memberID, memberTok, nil); resp.status != http.StatusOK {
		t.Errorf("self read: %d %s", resp.status, resp.body)
	}
	if resp := f.authedJSON(t, http.MethodPatch, "/api/v1/users/"+memberID+"/profile", memberTok,
		ports.UpdateUserProfileRequest{Designation: "Rep"}); resp.status != http.StatusNoContent {
		t.Errorf("self profile: %d %s", resp.status, resp.body)
	}
}

func TestE2E_Authz_CompanyOwnerManagesViaTenantAdmin(t *testing.T) {
	f := newE2EFixture(t)
	owner := f.registerAndLogin(t, "owner")
	memberID, _ := f.createMemberAndLogin(t, owner, "staff")
	for _, path := range []string{"/api/v1/users", "/api/v1/roles", "/api/v1/tenants/" + owner.TenantID} {
		if resp := f.authedJSON(t, http.MethodGet, path, owner.AccessToken, nil); resp.status != http.StatusOK {
			t.Errorf("owner GET %s: %d %s", path, resp.status, resp.body)
		}
	}
	salesExec := f.roleIDByName(t, owner.AccessToken, role.SystemRoles.Tenant.SalesExecutive)
	if resp := f.authedJSON(t, http.MethodPost, "/api/v1/users/"+memberID+"/roles", owner.AccessToken,
		ports.AssignUserRoleRequest{RoleID: salesExec}); resp.status != http.StatusNoContent {
		t.Errorf("owner assigns role: %d %s", resp.status, resp.body)
	}
	resp := f.authedJSON(t, http.MethodPost, "/api/v1/users/"+owner.MembershipID+"/deactivate", owner.AccessToken,
		ports.DeactivateUserRequest{Reason: "self lockout attempt"})
	if resp.status != http.StatusForbidden || decodeError(t, resp.body).Error != "self_modification_forbidden" {
		t.Errorf("owner self-deactivate: %d %s", resp.status, resp.body)
	}
}
```
   `DeactivateUserRequest` has a `Reason` field; check it in `dto.go`.
10. Run the unit tests: `go test ./internal/identity/... ./internal/crm/... ./internal/tasks/...`. Expect `ok`.
11. Run G1–G6. For G7: `go test -tags=integration -p 1 -count=1 -timeout=15m -run 'TestE2E_' ./cmd/api/... ; go test -tags=integration -p 1 -count=1 ./internal/identity/...`.
    - Existing E2E tests that read routes now gated as a plain member must use the owner token or be updated to expect 403.
    - `TestE2E_PlatformOperator_BypassesTenantGate` stays green through D2 (`platform.tenants.view`).
12. Commit: `fix(identity): permission-gate user/role/tenant routes; self-target + grant-authority rules; effective_permissions (ADR 0036/0068)`.

---

## Task 4 — Arch gate: no mutating route may be authentication-only, plus ADR 0068 (item 1, last part)

**Files:**
- Create `internal/architecture/authz_arch_test.go`
- Create `docs/adr/0068-authorization-hardening.md`
- Modify `docs/adr/README.md` (add an index row)

1. Write the gate. Its failing-first proof: temporarily wrap `POST /api/v1/users` with `auth(` and run the test. It must FAIL. Then revert.
```go
// authz_arch_test.go — ADR 0068: every mutating route is authorised, not
// merely authenticated.
package architecture_test

import (
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// selfServiceMutationAllowList: mutating routes legitimately gated by
// authentication only — they act solely on the caller's own Person /
// session, or carry handler-inline authz mandated by an ADR.
var selfServiceMutationAllowList = map[string]string{
	"POST /api/v1/auth/change-password":                    "self: own credential (ADR 0053)",
	"POST /api/v1/auth/request-email-change":               "self: own email",
	"DELETE /api/v1/auth/sessions/{familyId}":              "self: own refresh family (owner-checked in command)",
	"DELETE /api/v1/auth/sessions":                         "self: own refresh families",
	"POST /api/v1/permission-requests":                     "ADR 0055 §8: any caller may submit",
	"POST /api/v1/permission-requests/{requestId}/approve": "ADR 0055 §8: handler-inline manager-or-platform",
	"POST /api/v1/permission-requests/{requestId}/deny":    "ADR 0055 §8: handler-inline manager-or-platform",
	"POST /api/v1/permission-requests/{requestId}/cancel":  "ADR 0055 §8: handler-inline requester-only",
}

const authzFactories = `RequirePermission|RequireAnyPermission|RequirePlatform|RequireTenantPermission|RequirePermissionOrSelf`

var (
	authzCallRE    = regexp.MustCompile(`authn\.(` + authzFactories + `)\(`)
	authzBindingRE = regexp.MustCompile(`(?m)\b([a-zA-Z_]\w*)\s*:=\s*authn\.(` + authzFactories + `)\(`)
	authzComposeRE = regexp.MustCompile(`(?m)\b([a-zA-Z_]\w*)\s*:=\s*\w+\s*\(([^)]*)\)`)
)

func authzAliases(text string) map[string]bool {
	active := map[string]bool{}
	for _, m := range authzBindingRE.FindAllStringSubmatch(text, -1) {
		active[m[1]] = true
	}
	for round := 0; round < 4; round++ {
		added := false
		for _, m := range authzComposeRE.FindAllStringSubmatch(text, -1) {
			if active[m[1]] {
				continue
			}
			for name := range active {
				if regexp.MustCompile(`\b` + regexp.QuoteMeta(name) + `\b`).MatchString(m[2]) {
					active[m[1]], added = true, true
					break
				}
			}
		}
		if !added {
			break
		}
	}
	return active
}

func exprHasAuthz(expr string, aliases map[string]bool) bool {
	if authzCallRE.MatchString(expr) {
		return true
	}
	for name := range aliases {
		if regexp.MustCompile(`\b` + regexp.QuoteMeta(name) + `\b`).MatchString(expr) {
			return true
		}
	}
	return false
}

// TestArch_MutatingRoutesRequireAuthorization fails when any module
// registers a POST/PUT/PATCH/DELETE route whose handler expression carries
// only authentication (auth/RequireFreshStamp/RequireTenantContext) — the
// bug class that let any tenant member grant themselves roles (ADR 0068).
// Escape hatch: selfServiceMutationAllowList (reason required; stale
// entries fail).
//
// arch-test:no-negative-fixture (RED proof: wrap any identity mutating route in `auth(` → this test fails; recorded in the Task 4 commit message)
func TestArch_MutatingRoutesRequireAuthorization(t *testing.T) {
	t.Parallel()
	type violation struct {
		file string
		line int
		key  string
	}
	var violations []violation
	registered := map[string]bool{}
	for _, mod := range modulesUnderInternal(t) {
		portsDir := filepath.Join(internalDir(t), mod, "ports")
		walkGoFiles(t, portsDir, false, func(path string, src []byte) {
			slash := pathToSlash(path)
			if !strings.HasSuffix(slash, "/ports/http.go") && !strings.HasSuffix(slash, "_http.go") {
				return
			}
			text := string(src)
			aliases := authzAliases(text)
			for _, m := range muxHandleRE.FindAllStringSubmatchIndex(text, -1) {
				key := text[m[2]:m[3]]
				registered[key] = true
				method, _, _ := strings.Cut(key, " ")
				if method == "GET" || publicRoutesAllowList[key] {
					continue
				}
				if _, ok := selfServiceMutationAllowList[key]; ok {
					continue
				}
				if !exprHasAuthz(extractMuxHandlerExpr(text, m[1]), aliases) {
					violations = append(violations, violation{slash, 1 + strings.Count(text[:m[0]], "\n"), key})
				}
			}
		})
	}
	for key := range selfServiceMutationAllowList {
		if !registered[key] {
			t.Errorf("selfServiceMutationAllowList entry %q is not registered by any module — remove it", key)
		}
	}
	for _, v := range violations {
		t.Errorf("%s:%d %s — mutating route gated by authentication only; wrap in authn.RequirePermission/RequireAnyPermission/RequireTenantPermission/RequirePlatform, or (only if it acts solely on the caller's own identity) add it to selfServiceMutationAllowList with a reason", v.file, v.line, v.key)
	}
}
```
2. Write ADR 0068. Header lines: `**Status:** Accepted`, `**Date:** <today>`, `**Supersedes (in part):** ADR 0036 (resolution rule), ADR 0053 (deferred work #1), ADR 0039 (no-header operator scope)`. Body: Context = the audit findings. Decision = D1–D13 copied from this plan's Decisions section. Consequences, plus follow-up work (Q6, Q7, Redis limiter). Then a `## Fitness function` section naming `TestArch_MutatingRoutesRequireAuthorization` (in `internal/architecture/`). Add the README index row `| 0068 | Authorization hardening — tenant.admin implication, X-Tenant-Id, must-change enforcement, refresh grace | Accepted |`.
3. Run G1–G6, then the **full** `task test:int ; echo INT=$?`. Expect 0. This completes item 1.
4. Commit: `test(architecture): forbid authentication-only mutating routes; ADR 0068`. The message includes the RED proof line.

---

## Task 5 — Tenant registration is operator-only (item 9, D6)

**Files:**
- Modify `internal/identity/ports/http.go`
- Modify `internal/architecture/auth_middleware_arch_test.go`: remove `"POST /api/v1/tenants"` from `publicRoutesAllowList`
- Modify `api/openapi.yaml`
- Modify `scripts/smoke.sh`
- Modify `cmd/api/cross_tenant_e2e_test.go` (`registerAndLogin`, a new helper, `TestE2E_RegisterTenant_SameAdminEmailRejected`)
- Create `cmd/api/register_tenant_gate_e2e_test.go`

1. Write the failing integration test `register_tenant_gate_e2e_test.go`:
```go
//go:build integration

package main

import (
	"net/http"
	"testing"

	"github.com/leadkart/leadkart-go/internal/identity/ports"
)

func TestE2E_RegisterTenant_RequiresPlatformOperator(t *testing.T) {
	f := newE2EFixture(t)
	body := ports.RegisterTenantRequest{Slug: "gate-co", LegalName: "Gate Co Pvt Ltd", DisplayName: "Gate",
		AdminEmail: "gate-admin@e2e.test", AdminPassword: "Gate-Admin-2026!x", AdminFirstName: "Gate", AdminLastName: "Admin"}
	if resp := f.postJSON(t, "/api/v1/tenants", body); resp.status != http.StatusUnauthorized {
		t.Fatalf("anonymous: %d, want 401", resp.status)
	}
	member := f.registerAndLogin(t, "notop")
	if resp := f.authedJSON(t, http.MethodPost, "/api/v1/tenants", member.AccessToken, body); resp.status != http.StatusForbidden {
		t.Fatalf("tenant admin: %d, want 403", resp.status)
	}
	op := f.mintPlatformTokenWith(t, nil, true)
	if resp := f.authedJSON(t, http.MethodPost, "/api/v1/tenants", op, body); resp.status != http.StatusCreated {
		t.Fatalf("platform super user: %d %s, want 201", resp.status, resp.body)
	}
}
```
2. Update the fixture in `cross_tenant_e2e_test.go`.
   - Add `mintPlatformTokenWith(t, perms []string, superUser bool) string`. It has the same body as `mintPlatformToken`, but its `IssueArgs` carry `IsSuperUser: superUser` and `Permissions: append([]string{permission.IdentityPermissions.Platform.TenantsCreate}, perms...)`.
   - Make `mintPlatformToken(t, id)` delegate to a shared helper so its behaviour doesn't change.
   - Change `registerAndLogin` so it no longer calls HTTP `POST /api/v1/tenants`. It should call the command directly. This keeps the person counts that `TestE2E_PlatformStats_ReflectsState` relies on.
```go
	regOut, err := f.app.Commands.RegisterTenant.Handle(t.Context(), command.RegisterTenantCommand{
		Slug: slug.MustNew(slugStr), LegalName: namePrefix + " Pharma Pvt Ltd", DisplayName: namePrefix,
		AdminEmail: email.MustNew(adminEmail), AdminPassword: password, AdminFirstName: namePrefix, AdminLastName: "Admin",
	})
	if err != nil {
		t.Fatalf("register %s: %v", namePrefix, err)
	}
```
   - If `slug.MustNew` / `email.MustNew` do not exist, use `slug.New` / `email.New` plus `t.Fatalf`.
   - Set `password := "Correct-Horse-Battery-9!" + suffix` now, so Task 12 needs no further fixture churn.
   - Fill `registeredTenant` from `regOut`.
   - `TestE2E_RegisterTenant_SameAdminEmailRejected` now sends its HTTP calls with `f.mintPlatformTokenWith(t, nil, true)`.
   - Then run `grep -rn '"/api/v1/tenants",' cmd/api internal --include=*_test.go` and update any remaining anonymous POST callers the same way.
3. Run G7 on that file: `go test -tags=integration -p 1 -count=1 -run 'TestE2E_RegisterTenant' ./cmd/api/...`. Expect the anonymous case to FAIL with 201 instead of 401.
4. Implement in `http.go`.
   - Remove the anonymous `mux.Handle("POST /api/v1/tenants", ...)`.
   - Inside the verifier block, after `platform := ...`, add:
```go
		// Tenant onboarding is operator-only (ADR 0053 "no public sign-up
		// surface"; BRD PlatformManager onboarding; ADR 0068 D6).
		tenantsCreate := authn.RequirePermission(verifier, stampValidator, p.Platform.TenantsCreate)
		mux.Handle("POST /api/v1/tenants", platform(tenantsCreate(handleRegisterTenant(log, a))))
```
   - Update the AddRoutes godoc list.
5. Arch: delete the `POST /api/v1/tenants` line from `publicRoutesAllowList`.
6. OpenAPI for `POST /api/v1/tenants`:
   - Add `security: [{ BearerAuth: [] }]`, `x-required-permissions: [platform.tenants.create]`, and the description "Platform operators only (is_platform + slug anchor) — ADR 0068".
   - Add responses `'401'`, `'403'`, `'429'`. For 429, define a `TooManyRequests` component response if none exists, with header `Retry-After: { schema: { type: integer } }`.
7. Update `scripts/smoke.sh`.
   - Before step 2, add a superadmin login:
```bash
SUPERADMIN_EMAIL="${SUPERADMIN_EMAIL:-superadmin@leadkart.local}"
SUPERADMIN_PASSWORD="${SUPERADMIN_PASSWORD:-LeadKart!Dev2026}"
OP_RESP=$(curl -fsS -X POST "${API_URL}/api/v1/auth/login" -H "Content-Type: application/json" \
    -d "{\"email\":\"${SUPERADMIN_EMAIL}\",\"password\":\"${SUPERADMIN_PASSWORD}\",\"device_label\":\"smoke-operator\"}") \
    || fail "superadmin login failed (run task docker:env + bootstrap)"
OP_TOKEN=$(echo "${OP_RESP}" | sed -n 's/.*"access_token":"\([^"]*\)".*/\1/p')
[[ -n "${OP_TOKEN}" ]] || fail "superadmin access_token absent"
```
   - Add `-H "Authorization: Bearer ${OP_TOKEN}"` to the registration curl.
   - Set `ADMIN_PASSWORD="Correct-Horse-Battery-Staple-1234!"`.
8. Run G1–G6. For G7: `go test -tags=integration -p 1 -count=1 -run 'TestE2E_' ./cmd/api/...`.
9. Commit: `fix(identity): tenant registration requires a platform operator with platform.tenants.create (ADR 0053/0068)`.

---

## Task 6 — Honour `X-Tenant-Id` for platform operators (item 2, D7)

**Files:**
- Create `internal/identity/ports/authn/tenant_scope.go`
- Create `internal/identity/ports/authn/tenant_scope_test.go`
- Modify `authn.go`: `RequireAuth`, `RequireTenantContext`
- Modify `authorize.go`: `RequireTenantPermission`
- Create `internal/identity/ports/scope_resolver.go`
- Modify `cmd/api/main.go` (`newServer`)
- Modify `cmd/api/cross_tenant_e2e_test.go` (add a `doWithHeaders` helper)
- Create `cmd/api/tenant_scope_e2e_test.go`
- Modify `api/openapi.yaml`

**Interfaces produced:**
```go
// authn
const TenantScopeHeader = "X-Tenant-Id"
type TenantScopeResolver interface { TenantExists(ctx context.Context, tenantID string) (bool, error) }
func TenantScope(verifier Verifier, resolver TenantScopeResolver, log *slog.Logger) func(http.Handler) http.Handler
// ports
type TenantScopeResolver struct{ /* app.Application */ }
func NewTenantScopeResolver(a app.Application) TenantScopeResolver
```

1. Write the failing unit tests in `tenant_scope_test.go`:
```go
package authn_test

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/leadkart/leadkart-go/internal/identity/app/jwt"
	"github.com/leadkart/leadkart-go/internal/identity/ports/authn"
)

type fakeTenants map[string]bool

func (f fakeTenants) TenantExists(_ context.Context, id string) (bool, error) { return f[id], nil }

func scopeChain(c *jwt.Claims, s *sentinel) http.Handler {
	v := &fakeVerifier{claims: withFreshness(c)}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	return authn.TenantScope(v, fakeTenants{tenantB: true}, log)(authn.RequireFreshStamp(v, alwaysFresh{})(s.handler()))
}

func scopeReq(t *testing.T, header string) *http.Request {
	r := newRequest(t, "Bearer tok")
	if header != "" {
		r.Header.Set(authn.TenantScopeHeader, header)
	}
	return r
}

func TestTenantScope(t *testing.T) {
	t.Parallel()
	op := func() *jwt.Claims { return &jwt.Claims{TenantID: tenantA, TenantSlug: authn.PlatformTenantSlug, IsPlatform: true} }
	member := func() *jwt.Claims { return &jwt.Claims{TenantID: tenantA, TenantSlug: "acme"} }
	imp := func() *jwt.Claims { return &jwt.Claims{TenantID: tenantA, TenantSlug: "acme", Act: &jwt.ActClaim{Sub: "op"}} }
	cases := []struct {
		name       string
		claims     *jwt.Claims
		header     string
		wantStatus int
		wantTenant string
	}{
		{"operator switches", op(), tenantB, 200, tenantB},
		{"operator no header", op(), "", 200, tenantA},
		{"operator malformed", op(), "nope", 400, ""},
		{"operator unknown tenant", op(), "0190a000-0000-7000-8000-0000000000ee", 404, ""},
		{"member header ignored", member(), tenantB, 200, tenantA},
		{"impersonation header ignored", imp(), tenantB, 200, tenantA},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := &sentinel{}
			rec := httptest.NewRecorder()
			scopeChain(tc.claims, s).ServeHTTP(rec, scopeReq(t, tc.header))
			if rec.Code != tc.wantStatus {
				t.Fatalf("status %d want %d body %s", rec.Code, tc.wantStatus, rec.Body.String())
			}
			if tc.wantTenant != "" && string(s.tenantID) != tc.wantTenant {
				t.Fatalf("tenant %q want %q", s.tenantID, tc.wantTenant)
			}
		})
	}
}

func TestTenantScope_SatisfiesHonoursEffectiveTenant(t *testing.T) {
	t.Parallel()
	// Operator holding tenant.admin in the PLATFORM tenant gains nothing in tenant B (D1 own-tenant rule).
	c := &jwt.Claims{TenantID: tenantA, TenantSlug: authn.PlatformTenantSlug, IsPlatform: true, Permissions: []string{"tenant.admin"}}
	var allowed bool
	v := &fakeVerifier{claims: withFreshness(c)}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	h := authn.TenantScope(v, fakeTenants{tenantB: true}, log)(authn.RequireFreshStamp(v, alwaysFresh{})(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { allowed = authn.Satisfies(r.Context(), "identity.users.view") })))
	h.ServeHTTP(httptest.NewRecorder(), scopeReq(t, tenantB))
	if allowed {
		t.Fatal("tenant.admin must not imply permissions in a header-scoped foreign tenant")
	}
}
```
   Also add a test to `authorize_test.go`: a platform operator request to `RequireTenantPermission` whose header (tenantA) differs from the path (tenantB) returns 400 with body containing `tenant_scope_conflict`. Chain it as `TenantScope(...)(RequireTenantPermission(...))` and use `tenantReq`.
2. Run `go test ./internal/identity/ports/authn/`. Expect a compile FAIL.
3. Implement `tenant_scope.go`:
```go
package authn

import (
	"context"
	"log/slog"
	"net/http"
	"strings"

	"github.com/google/uuid"

	"github.com/leadkart/leadkart-go/internal/common/tenancy"
)

// TenantScopeHeader is the ADR 0039 operator scope override header.
const TenantScopeHeader = "X-Tenant-Id"

const (
	errCodeInvalidTenantScope  = "invalid_tenant_scope"
	errCodeTenantScopeNotFound = "tenant_scope_not_found"
	errCodeTenantScopeConflict = "tenant_scope_conflict"
	errCodeInternal            = "internal_error"
)

// TenantScopeResolver validates a header-supplied target tenant (consumer-
// side interface; identity/ports provides the app-backed implementation).
type TenantScopeResolver interface {
	// TenantExists reports whether tenantID names an existing, non-deleted tenant.
	TenantExists(ctx context.Context, tenantID string) (bool, error)
}

type scopeOverrideKey struct{}

type scopeOverride struct {
	personID string
	tenantID tenancy.ID
}

// TenantScope implements ADR 0039 clauses 1-2 (ADR 0068 D7): X-Tenant-Id is
// honoured only for slug-anchored, non-impersonation platform operators;
// ignored for everyone else. A valid override is stashed on ctx and bound
// as the request tenant by RequireAuth. Wrap the whole mux; anonymous and
// invalid-token requests pass through untouched (per-route gates answer).
func TenantScope(verifier Verifier, resolver TenantScopeResolver, log *slog.Logger) func(http.Handler) http.Handler {
	if verifier == nil || resolver == nil || log == nil {
		panic("authn: TenantScope verifier, resolver and log required")
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			raw := strings.TrimSpace(r.Header.Get(TenantScopeHeader))
			token := extractBearer(r.Header)
			if raw == "" || token == "" {
				next.ServeHTTP(w, r)
				return
			}
			c, err := verifier.Verify(token)
			if err != nil {
				next.ServeHTTP(w, r)
				return
			}
			if !isPlatformOperator(c) {
				log.DebugContext(r.Context(), "X-Tenant-Id ignored for non-platform caller", "caller_tenant_id", c.TenantID)
				next.ServeHTTP(w, r)
				return
			}
			id, err := uuid.Parse(raw)
			if err != nil {
				writeError(w, http.StatusBadRequest, errCodeInvalidTenantScope, "X-Tenant-Id must be a tenant UUID")
				return
			}
			ok, err := resolver.TenantExists(r.Context(), id.String())
			if err != nil {
				log.ErrorContext(r.Context(), "tenant scope lookup failed", "err", err)
				writeError(w, http.StatusInternalServerError, errCodeInternal, "")
				return
			}
			if !ok {
				writeError(w, http.StatusNotFound, errCodeTenantScopeNotFound, "X-Tenant-Id does not name an active tenant")
				return
			}
			log.InfoContext(r.Context(), "operator tenant scope",
				"scope_mode", "tenant-via-header", "effective_tenant_id", id.String(),
				"caller_tenant_id", c.TenantID, "operator_person_id", c.Subject,
				"method", r.Method, "path", r.URL.Path)
			ctx := context.WithValue(r.Context(), scopeOverrideKey{}, scopeOverride{personID: c.Subject, tenantID: tenancy.ID(id.String())})
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// scopeOverrideFor returns the validated header tenant for this principal.
func scopeOverrideFor(ctx context.Context, c *jwt.Claims) (tenancy.ID, bool) {
	o, ok := ctx.Value(scopeOverrideKey{}).(scopeOverride)
	if !ok || c == nil || o.personID != c.Subject || !isPlatformOperator(c) {
		return "", false
	}
	return o.tenantID, true
}

// writeScopeConflict answers 400 when a header scope disagrees with a path tenant.
func scopeConflicts(ctx context.Context, c *jwt.Claims, pathTenant string) bool {
	o, ok := scopeOverrideFor(ctx, c)
	return ok && string(o) != pathTenant
}
```
   Add the `jwt` import.
4. Wire the override into the gates:
   - **`RequireAuth`:** replace `ctx = tenancy.WithID(ctx, tenancy.ID(claims.TenantID))` with:
     ```go
     tid := tenancy.ID(claims.TenantID)
     if o, ok := scopeOverrideFor(r.Context(), claims); ok { tid = o }
     ctx = tenancy.WithID(ctx, tid)
     ```
   - **`RequireTenantPermission` and `RequireTenantContext`:** after the `urlTenant == ""` check, add:
     ```go
     if scopeConflicts(r.Context(), c, urlTenant) {
         writeError(w, http.StatusBadRequest, errCodeTenantScopeConflict, "X-Tenant-Id conflicts with the tenant in the URL")
         return
     }
     ```
5. Write the ports resolver `scope_resolver.go`:
```go
package ports

import (
	"context"
	"errors"

	"github.com/leadkart/leadkart-go/internal/identity/app"
	"github.com/leadkart/leadkart-go/internal/identity/app/query"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)

// TenantScopeResolver adapts the GetTenant query to authn.TenantScopeResolver.
type TenantScopeResolver struct{ a app.Application }

// NewTenantScopeResolver wires the resolver used by authn.TenantScope.
func NewTenantScopeResolver(a app.Application) TenantScopeResolver { return TenantScopeResolver{a: a} }

// TenantExists reports whether id is an existing, non-deleted tenant.
func (r TenantScopeResolver) TenantExists(ctx context.Context, id string) (bool, error) {
	v, err := r.a.Queries.GetTenant.Handle(ctx, query.GetTenantQuery{TenantID: tenant.ID(id)})
	if errors.Is(err, tenant.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return v.Status != tenant.StatusDeleted.String(), nil
}
```
6. In `cmd/api/main.go` `newServer`, replace `return mux` with:
```go
	if verifier == nil {
		return mux
	}
	// ADR 0039 per-request scope selection (ADR 0068 D7).
	return authn.TenantScope(verifier, ports.NewTenantScopeResolver(identityApp), log)(mux)
```
7. Write the E2E test `tenant_scope_e2e_test.go` (integration). First add a `doWithHeaders(t, method, path, token string, body any, headers map[string]string) httpResp` helper next to `do` in `cross_tenant_e2e_test.go`. It has the same body as `do`, plus `for k, v := range headers { req.Header.Set(k, v) }`.
```go
//go:build integration

package main

import (
	"encoding/json"
	"net/http"
	"slices"
	"testing"

	"github.com/leadkart/leadkart-go/internal/identity/ports"
)

func userIDs(t *testing.T, body []byte) []string {
	t.Helper()
	var lr ports.ListUsersResponse
	if err := json.Unmarshal(body, &lr); err != nil {
		t.Fatalf("decode: %v", err)
	}
	out := make([]string, 0, len(lr.Users))
	for _, u := range lr.Users {
		out = append(out, u.MembershipID)
	}
	return out
}

func TestE2E_TenantScope_HeaderForOperatorsOnly(t *testing.T) {
	f := newE2EFixture(t)
	a := f.registerAndLogin(t, "scopea")
	b := f.registerAndLogin(t, "scopeb")
	op := f.mintPlatformTokenWith(t, nil, true)
	h := func(tenant string) map[string]string { return map[string]string{"X-Tenant-Id": tenant} }

	resp := f.doWithHeaders(t, http.MethodGet, "/api/v1/users", op, nil, h(a.TenantID))
	if resp.status != http.StatusOK || !slices.Contains(userIDs(t, resp.body), a.MembershipID) {
		t.Fatalf("operator scoped to A: %d %s", resp.status, resp.body)
	}
	if resp := f.doWithHeaders(t, http.MethodGet, "/api/v1/users", op, nil, h("0190a000-0000-7000-8000-0000000000ee")); resp.status != http.StatusNotFound {
		t.Errorf("unknown tenant: %d", resp.status)
	}
	if resp := f.doWithHeaders(t, http.MethodGet, "/api/v1/users", op, nil, h("not-a-uuid")); resp.status != http.StatusBadRequest {
		t.Errorf("malformed: %d", resp.status)
	}
	resp = f.doWithHeaders(t, http.MethodGet, "/api/v1/users", b.AccessToken, nil, h(a.TenantID))
	if resp.status != http.StatusOK || slices.Contains(userIDs(t, resp.body), a.MembershipID) || !slices.Contains(userIDs(t, resp.body), b.MembershipID) {
		t.Errorf("tenant admin header must be ignored: %d %s", resp.status, resp.body)
	}
	if resp := f.doWithHeaders(t, http.MethodGet, "/api/v1/tenants/"+b.TenantID, op, nil, h(a.TenantID)); resp.status != http.StatusBadRequest {
		t.Errorf("path/header conflict: %d", resp.status)
	}
	created := f.doWithHeaders(t, http.MethodPost, "/api/v1/users", op,
		ports.CreateUserRequest{Email: "scoped-new@e2e.test", Password: "Scoped-User-2026!", FirstName: "Scoped", LastName: "User"}, h(a.TenantID))
	if created.status != http.StatusCreated {
		t.Fatalf("operator create in A: %d %s", created.status, created.body)
	}
	var cu ports.CreateUserResponse
	_ = json.Unmarshal(created.body, &cu)
	if resp := f.doWithHeaders(t, http.MethodGet, "/api/v1/users", op, nil, h(a.TenantID)); !slices.Contains(userIDs(t, resp.body), cu.MembershipID) {
		t.Errorf("created user not visible in A")
	}
}
```
   If `CreateUser` fails because of a `created_by_membership_id` FK (the operator's membership belongs to another tenant), record it as a finding. Fix it in `handleCreateUser` by leaving `CreatedByMembershipID` empty when `authn` reports a header-scoped request (`tenancy.FromContext(ctx) != claims.TenantID`), and re-run.
8. OpenAPI:
   - Add a reusable parameter `XTenantId` (header, uuid, optional) with the D7 semantics and its error codes.
   - Reference it from every tenant-plane operation under `/api/v1/users*`, `/api/v1/roles*`, `/api/v1/tenants/{tenantId}*`, `/api/v1/permission-requests*`.
   - Update the `info.description` scope paragraph: "honoured only for platform operators; ignored for others; path tenant wins; 400 invalid_tenant_scope / tenant_scope_conflict, 404 tenant_scope_not_found".
9. Run G1–G6. For G7: `go test -tags=integration -p 1 -count=1 -run 'TestE2E_' ./cmd/api/...`.
10. Commit: `feat(authn): X-Tenant-Id operator scope selection with validation + audit log line (ADR 0039/0068)`.

---

## Task 7 — Enforce `must_change_password` (item 5, D8)

**Files:**
- Modify `internal/identity/app/jwt/jwt.go`: `Claims.MustChangePassword`, `IssueArgs.MustChangePassword`
- Modify `jwt_test.go`
- Modify `internal/identity/app/command/login.go` and `refresh.go` (+ `RefreshResult.MustChangePassword`)
- Modify `login_test.go` and `refresh_test.go`
- Modify `internal/identity/ports/authn/security_stamp.go` and `security_stamp_test.go`
- Modify `internal/identity/ports/http.go` (refresh response, change-password and capabilities routes)
- Modify `dto.go` (`CapabilitiesDto.MustChangePassword`) and `me_http.go`
- Modify `cmd/api/cross_tenant_e2e_test.go` (`registerAndLogin` completes the first change; `createMemberAndLogin` likewise)
- Create `cmd/api/must_change_password_e2e_test.go`
- Modify `api/openapi.yaml`

**Interface produced** (Task 8 extends it):
```go
func RequireFreshStampAllowingPendingPasswordChange(verifier Verifier, validator StampValidator) func(http.Handler) http.Handler
// internal: type sessionPolicy struct{ allowPendingPasswordChange bool }; func requireSession(v, s, sessionPolicy)
```

1. Write the failing tests.
   - **jwt:** round-trip `Issue(IssueArgs{..., MustChangePassword: true})` → `Verify` → `claims.MustChangePassword == true`.
   - **login:** a Person built via `person.NewWithMustChangePassword` → verify the issued token → the claim is true.
   - **refresh:** with the same seeded flag, `RefreshResult.MustChangePassword == true` and the token claim is true.
   - **authn**, in `security_stamp_test.go`:
```go
func TestRequireFreshStamp_MustChangePassword_Blocks(t *testing.T) {
	t.Parallel()
	v := &fakeVerifier{claims: withFreshness(&jwt.Claims{TenantID: "t", MustChangePassword: true})}
	s := &sentinel{}
	rec := httptest.NewRecorder()
	authn.RequireFreshStamp(v, alwaysFresh{})(s.handler()).ServeHTTP(rec, newRequest(t, "Bearer tok"))
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "password_change_required") || s.called {
		t.Fatalf("status %d body %s called %v", rec.Code, rec.Body.String(), s.called)
	}
	s2 := &sentinel{}
	rec = httptest.NewRecorder()
	authn.RequireFreshStampAllowingPendingPasswordChange(v, alwaysFresh{})(s2.handler()).ServeHTTP(rec, newRequest(t, "Bearer tok"))
	if rec.Code != http.StatusOK || !s2.called {
		t.Fatalf("allow-listed: %d", rec.Code)
	}
	imp := &fakeVerifier{claims: withFreshness(&jwt.Claims{TenantID: "t", MustChangePassword: true, Act: &jwt.ActClaim{Sub: "op"}})}
	s3 := &sentinel{}
	rec = httptest.NewRecorder()
	authn.RequireFreshStamp(imp, alwaysFresh{})(s3.handler()).ServeHTTP(rec, newRequest(t, "Bearer tok"))
	if rec.Code != http.StatusOK {
		t.Fatalf("impersonation exempt (ADR 0053): %d", rec.Code)
	}
}
```
2. Run `go test ./internal/identity/...`. Expect compile FAILs.
3. Implement.
   - **jwt:** add `MustChangePassword bool `json:"must_change_password,omitzero"`` to `Claims` and `MustChangePassword bool` to `IssueArgs`. In `Issue`, set `MustChangePassword: args.MustChangePassword`.
   - **login.go:** add `MustChangePassword: p.MustChangePassword(),` to the `IssueArgs`.
   - **refresh.go:** do the same in its `IssueArgs`, add the field `MustChangePassword bool` to `RefreshResult`, and set it from `p.MustChangePassword()`.
   - **http.go `handleRefresh`:** add `MustChangePassword: out.MustChangePassword,` to the response.
   - **security_stamp.go:** rename the body of `RequireFreshStamp` to `requireSession(verifier, validator, pol sessionPolicy)`, keeping the panic messages, and add:
```go
const errCodePasswordChangeRequired = "password_change_required"

// sessionPolicy tunes the post-freshness checks per route family (ADR 0068).
type sessionPolicy struct {
	allowPendingPasswordChange bool
}

// RequireFreshStamp — verified JWT + fresh stamp + ADR 0053 forced-rotation
// enforcement: a token minted with must_change_password=true answers 403
// password_change_required everywhere except the allow-listed routes.
// Impersonation tokens are exempt (the operator's flag doesn't apply).
func RequireFreshStamp(verifier Verifier, validator StampValidator) func(http.Handler) http.Handler {
	return requireSession(verifier, validator, sessionPolicy{})
}

// RequireFreshStampAllowingPendingPasswordChange is the allow-list variant
// for POST /auth/change-password and GET /auth/me/capabilities.
func RequireFreshStampAllowingPendingPasswordChange(verifier Verifier, validator StampValidator) func(http.Handler) http.Handler {
	return requireSession(verifier, validator, sessionPolicy{allowPendingPasswordChange: true})
}
```
     Inside `requireSession`, just before `next.ServeHTTP(w, r)`, add:
```go
			if c.MustChangePassword && !c.IsImpersonation() && !pol.allowPendingPasswordChange {
				writeError(w, http.StatusForbidden, errCodePasswordChangeRequired,
					"password change required before using this endpoint")
				return
			}
```
   - **http.go routes:** add `pendingOK := authn.RequireFreshStampAllowingPendingPasswordChange(verifier, stampValidator)`, and switch `POST /api/v1/auth/change-password` and `GET /api/v1/auth/me/capabilities` to `pendingOK(...)`.
   - **Capabilities:** add `MustChangePassword bool `json:"must_change_password"`` to `CapabilitiesDto` and set it from `c.MustChangePassword`.
4. Update the E2E fixtures.
   - In `registerAndLogin`, after the first login, complete the rotation:
```go
	newPassword := "Rotated-Pass-2026!" + suffix
	if cp := f.authedJSON(t, http.MethodPost, "/api/v1/auth/change-password", login.AccessToken,
		ports.ChangePasswordRequest{CurrentPassword: password, NewPassword: newPassword}); cp.status != http.StatusNoContent {
		t.Fatalf("first-login change-password %s: %d %s", namePrefix, cp.status, cp.body)
	}
	password = newPassword
	// re-login: the change rotated the security stamp (ADR 0028).
	loginResp = f.postJSON(t, "/api/v1/auth/login", ports.LoginRequest{Email: email, Password: password, DeviceLabel: "e2e test device " + suffix})
	// decode into login again as above
```
   - `createMemberAndLogin` does the same.
   - Grep `"/api/v1/auth/login"` in `cmd/api/*_test.go` and `internal/identity/app/command/flow_integration_test.go`. Any login of an admin-provisioned user that then calls a protected route must do the same rotation. `TestE2E_LoginThenRequirePermissionGate` is one of them.
5. Write `must_change_password_e2e_test.go`. Add a `registerOnly` helper alongside: the command-based registration without the login and rotation.
```go
//go:build integration

package main

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/leadkart/leadkart-go/internal/identity/ports"
)

func TestE2E_MustChangePassword_BlocksUntilRotated(t *testing.T) {
	f := newE2EFixture(t)
	reg := f.registerOnly(t, "mcp") // returns Email, Password (no login)
	resp := f.postJSON(t, "/api/v1/auth/login", ports.LoginRequest{Email: reg.Email, Password: reg.Password, DeviceLabel: "e2e"})
	var lr ports.LoginResponse
	_ = json.Unmarshal(resp.body, &lr)
	if !lr.MustChangePassword {
		t.Fatal("login must report must_change_password")
	}
	r := f.authedJSON(t, http.MethodGet, "/api/v1/users", lr.AccessToken, nil)
	if r.status != http.StatusForbidden || decodeError(t, r.body).Error != "password_change_required" {
		t.Fatalf("protected route: %d %s", r.status, r.body)
	}
	caps := f.authedJSON(t, http.MethodGet, "/api/v1/auth/me/capabilities", lr.AccessToken, nil)
	var cd ports.CapabilitiesDto
	_ = json.Unmarshal(caps.body, &cd)
	if caps.status != http.StatusOK || !cd.MustChangePassword {
		t.Fatalf("capabilities: %d %s", caps.status, caps.body)
	}
	ref := f.postJSON(t, "/api/v1/auth/refresh", ports.RefreshRequest{RefreshToken: lr.RefreshToken})
	var rr ports.LoginResponse
	_ = json.Unmarshal(ref.body, &rr)
	if !rr.MustChangePassword {
		t.Fatal("refresh must carry must_change_password forward")
	}
	newPw := "Rotated-Pass-2026!mcp"
	if cp := f.authedJSON(t, http.MethodPost, "/api/v1/auth/change-password", rr.AccessToken,
		ports.ChangePasswordRequest{CurrentPassword: reg.Password, NewPassword: newPw}); cp.status != http.StatusNoContent {
		t.Fatalf("change-password: %d %s", cp.status, cp.body)
	}
	if r := f.authedJSON(t, http.MethodGet, "/api/v1/users", rr.AccessToken, nil); r.status != http.StatusUnauthorized {
		t.Fatalf("old token after change: %d, want 401 stale_token", r.status)
	}
	fresh := f.loginToken(t, reg.Email, newPw)
	if r := f.authedJSON(t, http.MethodGet, "/api/v1/users", fresh, nil); r.status != http.StatusOK {
		t.Fatalf("after rotation: %d %s", r.status, r.body)
	}
}
```
   `ports.RefreshRequest` has a `RefreshToken` field.
6. OpenAPI:
   - Add `must_change_password: { type: boolean }` to `CapabilitiesDto` and to `required`.
   - Note on the `LoginResponse` field that refresh returns it too.
   - Every authenticated operation's 403 now also includes `password_change_required`; say this once in the `info.description` error-codes paragraph.
   - Add the description to change-password and capabilities: "allowed while must_change_password is pending".
7. Run G1–G6. For G7: `go test -tags=integration -p 1 -count=1 ./internal/identity/... ; go test -tags=integration -p 1 -count=1 -run 'TestE2E_' ./cmd/api/...`.
8. Commit: `feat(identity): enforce must_change_password via JWT claim + allow-list middleware (ADR 0053 deferred #1)`.

---

## Task 8 — Impersonation tokens: refusal list, audience consistency, session liveness (item 7, D9)

**Files:**
- Modify `authn/security_stamp.go` (policy `rejectImpersonation` plus two factories)
- Modify `authn/authn.go` (`RequireAuth` aud/act check; `RequirePlatform`)
- Create `authn/impersonation_liveness.go` (+ test)
- Modify `internal/identity/app/query/impersonation.go` (+ test) and `app/app.go` (`Queries.ImpersonationSessionActive`)
- Create `internal/identity/ports/impersonation_checker.go`
- Modify `http.go` routes
- Modify `cmd/api/main.go` (wire the query and the middleware)
- Modify `cmd/api/impersonation_scoped_token_e2e_test.go`
- Modify `api/openapi.yaml`

**Interfaces produced:**
```go
// authn
func RequireSelfService(verifier Verifier, validator StampValidator) func(http.Handler) http.Handler
func RequireSelfServiceAllowingPendingPasswordChange(verifier Verifier, validator StampValidator) func(http.Handler) http.Handler
type ImpersonationSessionChecker interface { SessionActive(ctx context.Context, sessionID, operatorID string) (bool, error) }
func ImpersonationLiveness(verifier Verifier, checker ImpersonationSessionChecker) func(http.Handler) http.Handler
// query
type ImpersonationSessionActiveQuery struct{ SessionID, OperatorID string }
func NewImpersonationSessionActiveHandler(store impersonation.Store) ImpersonationSessionActiveHandler // Handle(ctx, q) (bool, error)
```

1. Write the failing tests.
   - **authn (unit):**
     - Claims with `Act` on `RequireSelfService` → 403 `impersonation_not_allowed`. Regular claims → 200.
     - `RequirePlatform` with claims `{IsPlatform: true, TenantSlug: "platform", Act: &ActClaim{}}` → 403.
     - `RequireAuth` with `Audience: {jwt.ImpersonationAudienceClaim}` and `Act == nil` → 401. With `Audience: {jwt.AudienceClaim}` and `Act != nil` → 401. Set `c.Audience = jwtv5.ClaimStrings{...}` via the promoted field, importing `jwtv5 "github.com/golang-jwt/jwt/v5"`.
     - `ImpersonationLiveness` with a fake checker that returns false → 401 `stale_token`. Returns true → passes. Non-impersonation claims → the checker is never called.
   - **query (unit):** using `adapters`' in-memory store is not allowed from the `app` tests. Use a small in-file fake `impersonation.Store` in `query/impersonation_test.go` (the existing tests show the pattern). Cases: missing session → false; operator mismatch → false; match → true.
2. Run `go test ./internal/identity/...`. Expect FAILs.
3. Implement in authn.
   - **security_stamp.go:** add `rejectImpersonation bool` to `sessionPolicy`, the constant `errCodeImpersonationNotAllowed = "impersonation_not_allowed"`, and the factories:
```go
// RequireSelfService gates routes that act on the caller's OWN Person /
// session (password, email, sessions, own activity, own permission
// requests). Impersonation tokens are refused (ADR 0045 "refuse
// impersonation tokens at endpoints that shouldn't accept them").
func RequireSelfService(verifier Verifier, validator StampValidator) func(http.Handler) http.Handler {
	return requireSession(verifier, validator, sessionPolicy{rejectImpersonation: true})
}

// RequireSelfServiceAllowingPendingPasswordChange — POST /auth/change-password.
func RequireSelfServiceAllowingPendingPasswordChange(verifier Verifier, validator StampValidator) func(http.Handler) http.Handler {
	return requireSession(verifier, validator, sessionPolicy{rejectImpersonation: true, allowPendingPasswordChange: true})
}
```
     Before the must-change check in `requireSession`, add:
     ```go
     if pol.rejectImpersonation && c.IsImpersonation() {
         writeError(w, http.StatusForbidden, errCodeImpersonationNotAllowed, "not available during an impersonation session")
         return
     }
     ```
   - **`RequireAuth`:** after a successful `Verify`, add:
```go
			if slices.Contains(claims.Audience, jwt.ImpersonationAudienceClaim) != (claims.Act != nil) {
				writeError(w, http.StatusUnauthorized, errCodeUnauthenticated, "invalid token")
				return
			}
```
   - **`RequirePlatform`:** change the condition to `if !isPlatformOperator(c) {`.
   - **`impersonation_liveness.go`:**
```go
package authn

import (
	"context"
	"net/http"
)

// ImpersonationSessionChecker reports whether an impersonation session is
// still open for the operator (consumer-side; ports adapts the app query).
type ImpersonationSessionChecker interface {
	SessionActive(ctx context.Context, sessionID, operatorID string) (bool, error)
}

// ImpersonationLiveness rejects impersonation tokens whose session has been
// ended or has expired (ADR 0045 step 7 bounds post-end access; tokens live
// up to 4h). Wrap the whole mux. Non-impersonation / anonymous / invalid
// tokens pass through untouched.
func ImpersonationLiveness(verifier Verifier, checker ImpersonationSessionChecker) func(http.Handler) http.Handler {
	if verifier == nil || checker == nil {
		panic("authn: ImpersonationLiveness verifier + checker required")
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			token := extractBearer(r.Header)
			if token == "" {
				next.ServeHTTP(w, r)
				return
			}
			c, err := verifier.Verify(token)
			if err != nil || !c.IsImpersonation() {
				next.ServeHTTP(w, r)
				return
			}
			ok, err := checker.SessionActive(r.Context(), c.Act.SessionID, c.Act.Sub)
			if err != nil || !ok {
				writeError(w, http.StatusUnauthorized, errCodeStaleToken, "impersonation session has ended")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
```
4. Add the query to `query/impersonation.go`, register it in `app.Queries` as `ImpersonationSessionActive query.ImpersonationSessionActiveHandler`, and wire it in `main.go` `buildIdentityApp` as `query.NewImpersonationSessionActiveHandler(impersonationStore)`:
```go
// ImpersonationSessionActiveQuery asks whether a session is open for an operator.
type ImpersonationSessionActiveQuery struct {
	SessionID  string
	OperatorID string
}

// ImpersonationSessionActiveHandler resolves liveness from the session store.
type ImpersonationSessionActiveHandler struct{ store impersonation.Store }

// NewImpersonationSessionActiveHandler wires the handler.
func NewImpersonationSessionActiveHandler(store impersonation.Store) ImpersonationSessionActiveHandler {
	if store == nil {
		panic("query: NewImpersonationSessionActiveHandler store required")
	}
	return ImpersonationSessionActiveHandler{store: store}
}

// Handle returns true only for an existing, unexpired session owned by the operator.
func (h ImpersonationSessionActiveHandler) Handle(ctx context.Context, q ImpersonationSessionActiveQuery) (bool, error) {
	if q.SessionID == "" || q.OperatorID == "" {
		return false, nil
	}
	s, err := h.store.Get(ctx, q.SessionID)
	if errors.Is(err, impersonation.ErrSessionNotFound) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("impersonation_session_active: %w", err)
	}
	return s.OperatorID() == q.OperatorID, nil
}
```
   Ports adapter `impersonation_checker.go`:
```go
package ports

// ImpersonationSessionChecker adapts the liveness query to authn.
type ImpersonationSessionChecker struct{ a app.Application }

// NewImpersonationSessionChecker wires the checker used by authn.ImpersonationLiveness.
func NewImpersonationSessionChecker(a app.Application) ImpersonationSessionChecker {
	return ImpersonationSessionChecker{a: a}
}

// SessionActive reports whether sessionID is open for operatorID.
func (c ImpersonationSessionChecker) SessionActive(ctx context.Context, sessionID, operatorID string) (bool, error) {
	return c.a.Queries.ImpersonationSessionActive.Handle(ctx, query.ImpersonationSessionActiveQuery{SessionID: sessionID, OperatorID: operatorID})
}
```
   Add the `context`, `app` and `query` imports. In `newServer`, add:
   ```go
   h := authn.TenantScope(verifier, ports.NewTenantScopeResolver(identityApp), log)(mux)
   return authn.ImpersonationLiveness(verifier, ports.NewImpersonationSessionChecker(identityApp))(h)
   ```
5. Switch the routes in `http.go`:
   - Add `self := authn.RequireSelfService(verifier, stampValidator)` and `selfPending := authn.RequireSelfServiceAllowingPendingPasswordChange(verifier, stampValidator)`.
   - `POST /auth/change-password` → `selfPending`.
   - `POST /auth/request-email-change`, `GET /auth/sessions`, `DELETE /auth/sessions/{familyId}`, `DELETE /auth/sessions`, `GET /auth/me/activity`, and `POST /permission-requests` plus its `/approve`, `/deny`, `/cancel` → `self(...)`.
   - Capabilities stays `pendingOK`, and GET permission-requests stays `auth`.
   - In `handleGetPermissionRequest`, change `isPlatform := c.IsPlatform` to `isPlatform := c.IsPlatform && c.TenantSlug == authn.PlatformTenantSlug && !c.IsImpersonation()`.
6. Add the E2E test to `impersonation_scoped_token_e2e_test.go`:
```go
func TestE2E_Impersonation_ScopedToken_ReachAndRefusals(t *testing.T) {
	f := newE2EFixture(t)
	tenantA := f.registerAndLogin(t, "imp")
	platformTok := f.mintPlatformToken(t, "")
	resp := f.authedJSON(t, http.MethodPost, "/api/v1/platform/impersonation/sessions", platformTok,
		ports.CreateImpersonationSessionRequest{TargetTenantID: tenantA.TenantID, Reason: "support ticket 4711 reach test", DurationMinutes: 30})
	if resp.status != http.StatusCreated {
		t.Fatalf("create session: %d %s", resp.status, resp.body)
	}
	var out ports.CreateImpersonationSessionResponse
	_ = json.Unmarshal(resp.body, &out)

	if r := f.authedJSON(t, http.MethodGet, "/api/v1/users", out.AccessToken, nil); r.status != http.StatusOK {
		t.Fatalf("tenant.admin reach in target: %d %s", r.status, r.body)
	}
	for _, rt := range []struct{ method, path string }{
		{http.MethodPost, "/api/v1/auth/change-password"},
		{http.MethodGet, "/api/v1/auth/sessions"},
		{http.MethodPost, "/api/v1/permission-requests"},
	} {
		r := f.authedJSON(t, rt.method, rt.path, out.AccessToken, map[string]string{})
		if r.status != http.StatusForbidden || decodeError(t, r.body).Error != "impersonation_not_allowed" {
			t.Errorf("%s %s: %d %s", rt.method, rt.path, r.status, r.body)
		}
	}
	if r := f.authedJSON(t, http.MethodDelete, "/api/v1/platform/impersonation/sessions/"+out.SessionID, platformTok, nil); r.status != http.StatusNoContent {
		t.Fatalf("end session: %d %s", r.status, r.body)
	}
	if r := f.authedJSON(t, http.MethodGet, "/api/v1/users", out.AccessToken, nil); r.status != http.StatusUnauthorized {
		t.Fatalf("after session end: %d, want 401", r.status)
	}
}
```
   The JSON field names (`SessionID`, `AccessToken`) come from `CreateImpersonationSessionResponse`; confirm them in `dto.go`.
7. OpenAPI: document `impersonation_not_allowed` on the self-service routes, the post-end 401 `stale_token` on `BearerAuth`, and the aud/act consistency rule.
8. Run G1–G6. For G7: `go test -tags=integration -p 1 -count=1 -run 'TestE2E_' ./cmd/api/... ; go test -tags=integration -p 1 -count=1 ./internal/identity/...`.
9. Commit: `fix(authn): refuse impersonation tokens on self-service + platform routes; enforce aud/act + session liveness (ADR 0045/0068)`.

---

## Task 9 — Trusted-proxy client IP and per-route rate limits (item 4, D11)

**Files:**
- Create `internal/common/httpmw/clientip.go` (+ `clientip_test.go`)
- Create `internal/common/httpmw/route_ratelimit.go` (+ test)
- Modify `internal/common/httpmw/chain.go`
- Modify `internal/common/config/config.go` and `validate.go` (+ tests)
- Modify `cmd/api/main.go`
- Modify `docker/compose.yml`
- Create `cmd/api/ratelimit_e2e_test.go`

**Interfaces produced:**
```go
func ParseTrustedProxies(csv string) ([]netip.Prefix, error)
func ClientIP(r *http.Request, trusted []netip.Prefix) string
func ClientIPKeyer(trusted []netip.Prefix) LimiterKeyer
type RouteRateLimits map[string]LimiterConfig   // key "METHOD /exact/path"
func RouteRateLimit(routes RouteRateLimits, keyer LimiterKeyer) Middleware
// PublicChainConfig gains: TrustedProxies []netip.Prefix; RouteRateLimits RouteRateLimits
// config: AppConfig.HTTP HTTPConfig{TrustedProxies string}; AppConfig.RateLimit RateLimitConfig{Login, Refresh, PasswordResetRequest, PasswordReset, EmailChangeConfirm, TenantRegistration RouteLimit}; RouteLimit{PerMinute float64; Burst int}
```

1. Write the failing tests `clientip_test.go`:
```go
package httpmw_test

import (
	"net/http/httptest"
	"testing"

	"github.com/leadkart/leadkart-go/internal/common/httpmw"
)

func TestClientIP(t *testing.T) {
	t.Parallel()
	trusted, err := httpmw.ParseTrustedProxies("10.0.0.0/8, 127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct{ name, remote, xff, want string }{
		{"untrusted peer ignores XFF", "203.0.113.9:5000", "1.2.3.4", "203.0.113.9"},
		{"trusted peer uses rightmost untrusted", "127.0.0.1:5000", "198.51.100.7, 10.1.1.1", "198.51.100.7"},
		{"spoofed left part ignored", "10.0.0.5:5000", "6.6.6.6, 198.51.100.7", "198.51.100.7"},
		{"trusted peer no XFF", "127.0.0.1:5000", "", "127.0.0.1"},
		{"malformed hop stops at peer", "127.0.0.1:5000", "garbage", "127.0.0.1"},
		{"ipv4-mapped peer", "[::ffff:127.0.0.1]:5000", "198.51.100.7", "198.51.100.7"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := httptest.NewRequest("GET", "/", nil)
			r.RemoteAddr = tc.remote
			if tc.xff != "" {
				r.Header.Set("X-Forwarded-For", tc.xff)
			}
			if got := httpmw.ClientIP(r, trusted); got != tc.want {
				t.Fatalf("ClientIP = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestParseTrustedProxies_Rejects(t *testing.T) {
	t.Parallel()
	if _, err := httpmw.ParseTrustedProxies("10.0.0.0/33"); err == nil {
		t.Fatal("want error")
	}
}
```
   Add `route_ratelimit_test.go`: with `RouteRateLimits{"POST /login": {RatePerSecond: 0.001, Burst: 2}}` and `ClientIPKeyer(nil)`:
   - The third `POST /login` from `192.0.2.1` → 429 with `Retry-After`.
   - The first `POST /login` from `192.0.2.2` → 200.
   - `GET /login` and `POST /other` are never limited.
2. Run `go test ./internal/common/httpmw/`. Expect a compile FAIL.
3. Implement `clientip.go`:
```go
package httpmw

import (
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"strings"
)

// ParseTrustedProxies parses a comma-separated list of CIDRs / bare IPs.
func ParseTrustedProxies(csv string) ([]netip.Prefix, error) {
	var out []netip.Prefix
	for _, raw := range strings.Split(csv, ",") {
		s := strings.TrimSpace(raw)
		if s == "" {
			continue
		}
		if strings.Contains(s, "/") {
			p, err := netip.ParsePrefix(s)
			if err != nil {
				return nil, fmt.Errorf("httpmw: trusted proxy %q: %w", s, err)
			}
			out = append(out, p.Masked())
			continue
		}
		a, err := netip.ParseAddr(s)
		if err != nil {
			return nil, fmt.Errorf("httpmw: trusted proxy %q: %w", s, err)
		}
		out = append(out, netip.PrefixFrom(a.Unmap(), a.Unmap().BitLen()))
	}
	return out, nil
}

func isTrusted(a netip.Addr, trusted []netip.Prefix) bool {
	a = a.Unmap()
	for _, p := range trusted {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

// ClientIP returns the originating client IP. X-Forwarded-For is consulted
// ONLY when the TCP peer is a trusted proxy; hops are walked right-to-left
// skipping trusted proxies and the first untrusted hop is the client (the
// left part of XFF is client-controlled — OWASP / MDN "rightmost
// untrusted" rule; ADR 0030 "XFF trust requires a proxy allowlist").
func ClientIP(r *http.Request, trusted []netip.Prefix) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = strings.TrimSpace(r.RemoteAddr)
	}
	peer, err := netip.ParseAddr(host)
	if err != nil {
		return host
	}
	peer = peer.Unmap()
	if len(trusted) == 0 || !isTrusted(peer, trusted) {
		return peer.String()
	}
	var hops []string
	for _, h := range r.Header.Values("X-Forwarded-For") {
		for _, part := range strings.Split(h, ",") {
			if s := strings.TrimSpace(part); s != "" {
				hops = append(hops, s)
			}
		}
	}
	for i := len(hops) - 1; i >= 0; i-- {
		a, err := netip.ParseAddr(hops[i])
		if err != nil {
			return peer.String()
		}
		if !isTrusted(a, trusted) {
			return a.Unmap().String()
		}
	}
	return peer.String()
}

// ClientIPKeyer keys rate-limit buckets by [ClientIP].
func ClientIPKeyer(trusted []netip.Prefix) LimiterKeyer {
	return func(r *http.Request) LimiterKey { return LimiterKey(ClientIP(r, trusted)) }
}
```
   `route_ratelimit.go`:
```go
package httpmw

import "net/http"

// RouteRateLimits maps exact "METHOD /path" keys to tighter buckets.
type RouteRateLimits map[string]LimiterConfig

// RouteRateLimit applies a per-route, per-client token bucket on top of the
// global limiter for credential + abuse-prone anonymous endpoints (login,
// refresh, password reset, email-change confirm, tenant registration).
// Exact-match only (these routes have no path wildcards).
func RouteRateLimit(routes RouteRateLimits, keyer LimiterKeyer) Middleware {
	if keyer == nil {
		panic("httpmw: RouteRateLimit keyer required")
	}
	limiters := make(map[string]*RateLimiter, len(routes))
	for route, cfg := range routes {
		limiters[route] = NewRateLimiter(cfg, keyer)
	}
	return func(next http.Handler) http.Handler {
		wrapped := make(map[string]http.Handler, len(limiters))
		for route, l := range limiters {
			wrapped[route] = l.Middleware()(next)
		}
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if h, ok := wrapped[r.Method+" "+r.URL.Path]; ok {
				h.ServeHTTP(w, r)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
```
   In `chain.go`:
   - Add the fields `TrustedProxies []netip.Prefix` and `RouteRateLimits RouteRateLimits`, documented.
   - In `PublicChain`, add `keyer := ClientIPKeyer(cfg.TrustedProxies)` and `ipLimiter := NewRateLimiter(cfg.IPRateLimit, keyer)`.
   - The default idempotency keyer, used when `cfg.IdempotencyKeyer == nil`:
```go
	idemKeyer := cfg.IdempotencyKeyer
	if idemKeyer == nil {
		idemKeyer = func(r *http.Request) string {
			if id, ok := tenancy.FromContext(r.Context()); ok && !id.IsZero() {
				return "tenant:" + id.String()
			}
			if ip := ClientIP(r, cfg.TrustedProxies); ip != "" {
				return "anon:" + ip
			}
			return ""
		}
	}
```
   - Build the chain slice with `RouteRateLimit(cfg.RouteRateLimits, keyer)` inserted after `ipLimiter.Middleware()` when `len(cfg.RouteRateLimits) > 0`. Update the order godoc.
4. Config:
   - In `config.go`, add `HTTP HTTPConfig `koanf:"http"`` and `RateLimit RateLimitConfig `koanf:"rate_limit"``:
```go
// HTTPConfig — public listener behaviour. TrustedProxies is a comma-separated
// CIDR/IP list of reverse proxies (web BFF, ingress) whose X-Forwarded-For is
// believed (ADR 0030/0068). Empty = trust nobody.
type HTTPConfig struct {
	TrustedProxies string `koanf:"trusted_proxies"`
}

// RouteLimit is one per-route, per-client token bucket.
type RouteLimit struct {
	PerMinute float64 `koanf:"per_minute"`
	Burst     int     `koanf:"burst"`
}

// RateLimitConfig — tighter per-route limits on anonymous credential routes.
type RateLimitConfig struct {
	Login                RouteLimit `koanf:"login"`
	Refresh              RouteLimit `koanf:"refresh"`
	PasswordResetRequest RouteLimit `koanf:"password_reset_request"`
	PasswordReset        RouteLimit `koanf:"password_reset"`
	EmailChangeConfirm   RouteLimit `koanf:"email_change_confirm"`
	TenantRegistration   RouteLimit `koanf:"tenant_registration"`
}
```
   - Defaults: `Login{30,10}`, `Refresh{120,30}`, `PasswordResetRequest{5,5}`, `PasswordReset{10,5}`, `EmailChangeConfirm{10,5}`, `TenantRegistration{10,5}`.
   - In `structToMap`, add `"http": map[string]any{"trusted_proxies": cfg.HTTP.TrustedProxies}` and `"rate_limit": map[string]any{"login": rl(cfg.RateLimit.Login), ...}` with `rl := func(l RouteLimit) map[string]any { return map[string]any{"per_minute": l.PerMinute, "burst": l.Burst} }`.
   - In `validate.go`, add `if _, err := httpmw.ParseTrustedProxies(cfg.HTTP.TrustedProxies); err != nil { return fmt.Errorf("config: HTTP.TrustedProxies: %w", err) }`, and for each RouteLimit require `PerMinute > 0 && Burst > 0`. If importing httpmw into config creates a cycle, duplicate the CIDR parse loop with `netip`. Check with `go list -deps`.
   - Tests in `validate_test.go` and `config_test.go`:
     - A bad CIDR is rejected.
     - `LEADKART_HTTP__TRUSTED_PROXIES=10.0.0.0/8` loads.
     - `LEADKART_RATE_LIMIT__LOGIN__PER_MINUTE=600` overrides the default.
5. `main.go`: parse `cfg.HTTP.TrustedProxies` (already validated) and pass:
```go
	trustedProxies, err := httpmw.ParseTrustedProxies(cfg.HTTP.TrustedProxies)
	if err != nil {
		return fmt.Errorf("trusted proxies: %w", err)
	}
	perSecond := func(l config.RouteLimit) httpmw.LimiterConfig {
		return httpmw.LimiterConfig{RatePerSecond: l.PerMinute / 60, Burst: l.Burst}
	}
	// ... PublicChainConfig{ ..., TrustedProxies: trustedProxies,
	RouteRateLimits: httpmw.RouteRateLimits{
		"POST /api/v1/auth/login":                  perSecond(cfg.RateLimit.Login),
		"POST /api/v1/auth/refresh":                perSecond(cfg.RateLimit.Refresh),
		"POST /api/v1/auth/request-password-reset": perSecond(cfg.RateLimit.PasswordResetRequest),
		"POST /api/v1/auth/reset-password":         perSecond(cfg.RateLimit.PasswordReset),
		"POST /api/v1/auth/confirm-email-change":   perSecond(cfg.RateLimit.EmailChangeConfirm),
		"POST /api/v1/tenants":                     perSecond(cfg.RateLimit.TenantRegistration),
	},
```
   - Log at WARN on boot when `trustedProxies` is empty: "behind a BFF all clients share one rate-limit bucket".
   - In `docker/compose.yml` api env, add `LEADKART_HTTP__TRUSTED_PROXIES: "${LEADKART_HTTP__TRUSTED_PROXIES:-}"`.
6. Integration test `cmd/api/ratelimit_e2e_test.go`. It wires the real chain and the real Postgres idempotency store:
```go
//go:build integration

package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/leadkart/leadkart-go/internal/common/httpmw"
	"github.com/leadkart/leadkart-go/internal/common/idempotency"
)

func TestE2E_RateLimit_PerRouteKeyedByForwardedClient(t *testing.T) {
	f := newE2EFixture(t)
	trusted, _ := httpmw.ParseTrustedProxies("127.0.0.1/32,::1/128")
	chain := httpmw.PublicChain(httpmw.PublicChainConfig{
		Logger: silentLogger(), IdempotencyStore: idempotency.NewPostgresStore(f.Pool), Now: time.Now,
		IPRateLimit:     httpmw.LimiterConfig{RatePerSecond: 100, Burst: 100},
		TrustedProxies:  trusted,
		RouteRateLimits: httpmw.RouteRateLimits{"POST /api/v1/auth/login": {RatePerSecond: 0.001, Burst: 3}},
	})
	srv := httptest.NewServer(chain(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusUnauthorized) })))
	t.Cleanup(srv.Close)
	post := func(client string) int {
		req, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, srv.URL+"/api/v1/auth/login", strings.NewReader(`{}`))
		req.Header.Set("X-Forwarded-For", client)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		return resp.StatusCode
	}
	for i := 0; i < 3; i++ {
		if got := post("198.51.100.1"); got == http.StatusTooManyRequests {
			t.Fatalf("attempt %d limited early", i+1)
		}
	}
	if got := post("198.51.100.1"); got != http.StatusTooManyRequests {
		t.Fatalf("4th from same client: %d, want 429", got)
	}
	if got := post("198.51.100.2"); got == http.StatusTooManyRequests {
		t.Fatal("different client behind the same BFF must have its own bucket")
	}
}
```
   - This test exercises the real chain and Postgres idempotency store around a stub handler.
   - The `f.Pool` field already exists on `e2eFixture`.
   - If `PublicChain` cannot accept an `http.Handler` stub, wrap `newServer(...)` exactly as `run()` does.
7. Run G1–G6. For G7: `go test -tags=integration -p 1 -count=1 -run 'TestE2E_RateLimit' ./cmd/api/... ; go test -tags=integration -p 1 -count=1 ./internal/common/...`.
8. Commit: `feat(httpmw): trusted-proxy client IP + per-route limits on credential routes (ADR 0030/0068)`.

---

## Task 10 — Enumeration: phantom lockout, timing-flat lock branch, reset response floor (item 6, D12)

**Files:**
- Modify `internal/identity/app/command/login.go`: `PhantomLockout` interface, `NewLoginHandler` gets a trailing `phantom PhantomLockout` parameter, `resolveAndVerify`
- Modify `login_test.go`
- Create `internal/identity/adapters/phantom_lockout_inmemory.go` (+ `_test.go`)
- Create `internal/common/httpmw/minduration.go` (+ `_test.go`)
- Modify `internal/identity/ports/http.go` (reset route)
- Modify `cmd/api/main.go` (wiring)
- Modify `internal/identity/app/command/fixture_integration_test.go` and any other `NewLoginHandler` callers (`grep -rn NewLoginHandler`)
- Create `cmd/api/enumeration_e2e_test.go`

**Interfaces produced:**
```go
// command (consumer-side)
type PhantomLockout interface {
	LockedUntil(ctx context.Context, key string, now time.Time) time.Time
	RegisterFailure(ctx context.Context, key string, now time.Time) time.Time
}
// adapters
func NewInMemoryPhantomLockout(maxEntries int) *InMemoryPhantomLockout
// httpmw
func MinDuration(floor time.Duration) Middleware
```

1. Write the failing tests.
   - **`login_test.go`.** Use a test-local fake `PhantomLockout`, a map counter that locks at `person.MaxFailedLogins`. Cases:
     - (a) Ten failed logins for an **unknown** email: attempts 1–9 return `ErrInvalidCredentials`, attempt 10 returns `ErrAccountLocked` and `LockedUntilFromError` is non-zero, attempt 11 returns `ErrAccountLocked`.
     - (b) The same sequence for a **real** account with wrong passwords produces the identical error kinds.
     - (c) An inactive Person behaves like (a).
     - (d) A locked real account given the **correct** password still returns `ErrAccountLocked` and the Person's `FailedLoginCount` does not change.
   - **`phantom_lockout_inmemory_test.go`:**
     - The counter locks at `person.MaxFailedLogins`.
     - A gap greater than `person.LockoutWindow` resets the count.
     - The lock expires after `person.LockoutDuration`.
     - At `maxEntries` with nothing expired, `RegisterFailure` returns zero (fail-open).
   - **`minduration_test.go`:**
```go
func TestMinDuration_HoldsFastResponse(t *testing.T) {
	t.Parallel()
	h := httpmw.MinDuration(40*time.Millisecond)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Probe", "1")
		w.WriteHeader(http.StatusNoContent)
	}))
	start := time.Now()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/", nil))
	if el := time.Since(start); el < 40*time.Millisecond {
		t.Fatalf("returned after %v, want ≥40ms", el)
	}
	if rec.Code != http.StatusNoContent || rec.Header().Get("X-Probe") != "1" {
		t.Fatalf("response not preserved: %d %v", rec.Code, rec.Header())
	}
}

func TestMinDuration_ClientCancelEndsWait(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	h := httpmw.MinDuration(time.Hour)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(204) }))
	done := make(chan struct{})
	go func() { h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequestWithContext(ctx, "POST", "/", nil)); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("cancelled request still waiting")
	}
}
```
2. Run `go test ./internal/identity/... ./internal/common/httpmw/`. Expect FAILs.
3. Implement `login.go`.
   - Add the interface near `AuthRouter`. Add the `phantom PhantomLockout` field and constructor parameter: `if phantom == nil { panic("command: NewLoginHandler phantom lockout required") }`.
```go
// PhantomLockout mirrors the per-account lockout for login attempts that
// have no usable account (unknown email, inactive, anonymised) so 423 vs
// 401 carries no existence signal (ADR 0044 + ADR 0053; ADR 0068 D12).
// Keys are SHA-256 hex of the normalised email — never the email itself.
// Best-effort like the real counter: implementations swallow storage errors.
type PhantomLockout interface {
	// LockedUntil returns the active lockout expiry for key, zero if none.
	LockedUntil(ctx context.Context, key string, now time.Time) time.Time
	// RegisterFailure records a failed attempt; returns the lockout expiry
	// when this attempt crosses person.MaxFailedLogins, zero otherwise.
	RegisterFailure(ctx context.Context, key string, now time.Time) time.Time
}
```
   - In `resolveAndVerify`, the unknown-email branch becomes `return nil, nil, h.failWithoutAccount(ctx, cmd)`.
   - The locked branch becomes:
     ```go
     if p.IsLocked(h.now()) {
         _ = argon2.Verify(cmd.Password, h.dummyHash) // timing-flat; real password never checked while locked
         return nil, nil, newLockedError(p.LockedUntil())
     }
     ```
   - The `!p.IsActive() || p.IsAnonymised()` branch becomes `return nil, nil, h.failWithoutAccount(ctx, cmd)`.
   - Add:
```go
// failWithoutAccount is the single failure surface for login attempts with
// no usable account. Always one dummy verify (timing parity with the real
// wrong-password path) and the phantom lockout (surface parity with the
// real counter: the 10th attempt in the window answers 423).
func (h LoginHandler) failWithoutAccount(ctx context.Context, cmd LoginCommand) error {
	key := phantomKey(cmd.Email)
	now := h.now()
	_ = argon2.Verify(cmd.Password, h.dummyHash)
	if until := h.phantom.LockedUntil(ctx, key, now); !until.IsZero() {
		return newLockedError(until)
	}
	if until := h.phantom.RegisterFailure(ctx, key, now); !until.IsZero() {
		return newLockedError(until)
	}
	return ErrInvalidCredentials
}

func phantomKey(e email.Address) string {
	sum := sha256.Sum256([]byte(strings.ToLower(strings.TrimSpace(e.String()))))
	return hex.EncodeToString(sum[:])
}
```
   Add the `crypto/sha256`, `encoding/hex` and `strings` imports.
4. Implement the adapter `phantom_lockout_inmemory.go`:
```go
package adapters

import (
	"context"
	"sync"
	"time"

	"github.com/leadkart/leadkart-go/internal/identity/domain/person"
)

// InMemoryPhantomLockout is the single-replica PhantomLockout (v0.2 deploy
// shape, ADR 0030). Same thresholds as the Person aggregate. Bounded: when
// full and nothing has expired it stops registering (fail-open, matching
// ADR 0053 best-effort semantics). Redis-backed variant is follow-up work.
type InMemoryPhantomLockout struct {
	mu      sync.Mutex
	entries map[string]phantomEntry
	max     int
}

type phantomEntry struct {
	count       int
	lastFailed  time.Time
	lockedUntil time.Time
}

// NewInMemoryPhantomLockout wires the store; maxEntries ≥ 1.
func NewInMemoryPhantomLockout(maxEntries int) *InMemoryPhantomLockout {
	if maxEntries < 1 {
		panic("adapters: NewInMemoryPhantomLockout maxEntries must be ≥1")
	}
	return &InMemoryPhantomLockout{entries: make(map[string]phantomEntry), max: maxEntries}
}

// LockedUntil implements command.PhantomLockout.
func (l *InMemoryPhantomLockout) LockedUntil(_ context.Context, key string, now time.Time) time.Time {
	l.mu.Lock()
	defer l.mu.Unlock()
	e, ok := l.entries[key]
	if !ok || !now.Before(e.lockedUntil) {
		return time.Time{}
	}
	return e.lockedUntil
}

// RegisterFailure implements command.PhantomLockout.
func (l *InMemoryPhantomLockout) RegisterFailure(_ context.Context, key string, now time.Time) time.Time {
	l.mu.Lock()
	defer l.mu.Unlock()
	e, ok := l.entries[key]
	if !ok && len(l.entries) >= l.max {
		l.sweep(now)
		if len(l.entries) >= l.max {
			return time.Time{}
		}
	}
	if ok && now.Sub(e.lastFailed) > person.LockoutWindow {
		e.count = 0
	}
	e.count++
	e.lastFailed = now
	locked := time.Time{}
	if e.count >= person.MaxFailedLogins {
		e.lockedUntil = now.Add(person.LockoutDuration)
		e.count = 0
		locked = e.lockedUntil
	}
	l.entries[key] = e
	return locked
}

func (l *InMemoryPhantomLockout) sweep(now time.Time) {
	for k, e := range l.entries {
		if now.Sub(e.lastFailed) > person.LockoutWindow && !now.Before(e.lockedUntil) {
			delete(l.entries, k)
		}
	}
}
```
   Wire it in `main.go`: `adapters.NewInMemoryPhantomLockout(100_000)` as the last `NewLoginHandler` argument. Update the other callers, using a test fake or `adapters.NewInMemoryPhantomLockout(1000)` in integration fixtures.
5. `minduration.go`:
```go
package httpmw

import (
	"bytes"
	"net/http"
	"time"
)

// MinDuration buffers the response and releases it no earlier than floor
// after arrival, so branch-dependent work ("email exists → persist reset
// token" vs "unknown email → return") is not observable by timing
// (ADR 0044; OWASP Forgot Password Cheat Sheet "consistent response time").
// Client cancellation ends the wait. Use only on small-bodied routes.
func MinDuration(floor time.Duration) Middleware {
	if floor <= 0 {
		panic("httpmw: MinDuration floor must be positive")
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			buf := &bufferedResponse{header: http.Header{}}
			next.ServeHTTP(buf, r)
			if wait := floor - time.Since(start); wait > 0 {
				timer := time.NewTimer(wait)
				select {
				case <-timer.C:
				case <-r.Context().Done():
					timer.Stop()
				}
			}
			for k, v := range buf.header {
				w.Header()[k] = v
			}
			if buf.status == 0 {
				buf.status = http.StatusOK
			}
			w.WriteHeader(buf.status)
			_, _ = w.Write(buf.body.Bytes())
		})
	}
}

type bufferedResponse struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func (b *bufferedResponse) Header() http.Header { return b.header }
func (b *bufferedResponse) WriteHeader(s int) {
	if b.status == 0 {
		b.status = s
	}
}
func (b *bufferedResponse) Write(p []byte) (int, error) {
	if b.status == 0 {
		b.status = http.StatusOK
	}
	return b.body.Write(p)
}
```
   In `http.go`:
   - Add `const passwordResetResponseFloor = 300 * time.Millisecond` with a godoc noting it must exceed the p99 of the known-email path.
   - Change the route to `mux.Handle("POST /api/v1/auth/request-password-reset", httpmw.MinDuration(passwordResetResponseFloor)(handleRequestPasswordReset(log, a)))`.
   - Add the import `github.com/leadkart/leadkart-go/internal/common/httpmw`.
6. Integration test `enumeration_e2e_test.go`: real Postgres, so the real Person counter runs against the phantom counter.
```go
//go:build integration

package main

import (
	"net/http"
	"slices"
	"testing"

	"github.com/leadkart/leadkart-go/internal/identity/ports"
)

func TestE2E_Login_LockoutSurfaceIdenticalForRealAndUnknownEmail(t *testing.T) {
	f := newE2EFixture(t)
	real := f.registerAndLogin(t, "enum")
	seq := func(email string) []int {
		var out []int
		for i := 0; i < 11; i++ {
			resp := f.postJSON(t, "/api/v1/auth/login", ports.LoginRequest{Email: email, Password: "Wrong-Password-1!", DeviceLabel: "enum"})
			out = append(out, resp.status)
		}
		return out
	}
	realSeq := seq(real.Email)
	unknownSeq := seq("nobody-here-0001@e2e.test")
	if !slices.Equal(realSeq, unknownSeq) {
		t.Fatalf("status sequences differ: real=%v unknown=%v", realSeq, unknownSeq)
	}
	if realSeq[9] != http.StatusLocked {
		t.Fatalf("10th attempt: %d, want 423", realSeq[9])
	}
}
```
7. Run G1–G6. For G7: `go test -tags=integration -p 1 -count=1 ./internal/identity/... ; go test -tags=integration -p 1 -count=1 -run 'TestE2E_Login' ./cmd/api/...`.
8. Commit: `fix(identity): enumeration-safe lockout surface + timing-flat locked branch + reset response floor (ADR 0044/0053)`.

---

## Task 11 — Refresh grace window with a deterministic successor (item 3, D10)

**Files:**
- Modify `internal/identity/domain/refreshtoken/family.go` (+ `family_test.go`)
- Modify `internal/identity/app/refreshmint/refreshmint.go` (+ test)
- Modify `internal/identity/app/command/refresh.go` (+ `refresh_test.go`)
- Modify `cmd/api/main.go`, `internal/identity/app/command/fixture_integration_test.go`, and every `NewRefreshHandler` caller
- Create `cmd/api/refresh_grace_e2e_test.go`

**Interfaces produced:**
```go
// refreshtoken
type RotationOutcome int  // RotationIssued, RotationGraceReplay
func (f *Family) RotateWithGrace(presentedHash, candidateSuccessor TokenHash, ttl, graceWindow time.Duration, now time.Time) (RotationOutcome, error)
// Rotate(presented, new, ttl, now) error keeps its signature (= grace 0)
// refreshmint
func Successor(key []byte, familyID refreshtoken.FamilyID, presentedPlain string) (Pair, error)
func DeriveSuccessorKey(jwtSigningKey []byte) ([]byte, error)
// command
func NewRefreshHandler(families, persons, memberships, tenants, resolver, jwtIssuer, now, refreshTTL time.Duration, graceWindow time.Duration, successorKey []byte) RefreshHandler
```

1. Write the failing domain tests in `family_test.go`, using the existing `newFamily`, `hash` and `baseTime` helpers:
```go
func TestRotateWithGrace(t *testing.T) {
	t.Parallel()
	const ttl, grace = 14 * 24 * time.Hour, 30 * time.Second
	t.Run("replay inside window returns same successor without change", func(t *testing.T) {
		t.Parallel()
		f := newFamily(t)
		_ = f.PullEvents()
		first := f.CurrentToken().Hash()
		if out, err := f.RotateWithGrace(first, hash("s1"), ttl, grace, baseTime); err != nil || out != refreshtoken.RotationIssued {
			t.Fatalf("rotate: %v %v", out, err)
		}
		_ = f.PullEvents()
		out, err := f.RotateWithGrace(first, hash("s1"), ttl, grace, baseTime.Add(10*time.Second))
		if err != nil || out != refreshtoken.RotationGraceReplay {
			t.Fatalf("replay: %v %v", out, err)
		}
		if f.IsRevoked() || len(f.AllTokens()) != 2 || len(f.PullEvents()) != 0 {
			t.Fatal("grace replay must not mutate")
		}
	})
	t.Run("replay after window is reuse", func(t *testing.T) {
		t.Parallel()
		f := newFamily(t)
		first := f.CurrentToken().Hash()
		_, _ = f.RotateWithGrace(first, hash("s1"), ttl, grace, baseTime)
		if _, err := f.RotateWithGrace(first, hash("s1"), ttl, grace, baseTime.Add(31*time.Second)); !errors.Is(err, refreshtoken.ErrReuseDetected) || !f.IsRevoked() {
			t.Fatalf("want reuse+revoke, got %v revoked=%v", err, f.IsRevoked())
		}
	})
	t.Run("replay after successor used is reuse", func(t *testing.T) {
		t.Parallel()
		f := newFamily(t)
		first := f.CurrentToken().Hash()
		_, _ = f.RotateWithGrace(first, hash("s1"), ttl, grace, baseTime)
		_, _ = f.RotateWithGrace(hash("s1"), hash("s2"), ttl, grace, baseTime.Add(time.Second))
		if _, err := f.RotateWithGrace(first, hash("s1"), ttl, grace, baseTime.Add(2*time.Second)); !errors.Is(err, refreshtoken.ErrReuseDetected) {
			t.Fatalf("want reuse, got %v", err)
		}
	})
	t.Run("mismatched candidate is reuse", func(t *testing.T) {
		t.Parallel()
		f := newFamily(t)
		first := f.CurrentToken().Hash()
		_, _ = f.RotateWithGrace(first, hash("s1"), ttl, grace, baseTime)
		if _, err := f.RotateWithGrace(first, hash("other"), ttl, grace, baseTime.Add(time.Second)); !errors.Is(err, refreshtoken.ErrReuseDetected) {
			t.Fatalf("want reuse, got %v", err)
		}
	})
}
```
   refreshmint tests:
   - `Successor` is deterministic: same inputs give the same plaintext.
   - It differs across family ids and across presented tokens.
   - It is 43 characters of base64url (32 bytes).
   - A key shorter than 32 bytes returns an error.
   - `DeriveSuccessorKey` returns 32 bytes, deterministically.

   Handler tests in `refresh_test.go`:
   - Change `newRefreshRig` to take `(t, grace time.Duration, clock *time.Time)`. The existing tests pass `0` and a fixed clock, so `TestRefresh_ReuseDetected...` stays unchanged.
   - New test: with grace 30 s, refreshing the same plaintext twice returns an **identical** `RefreshTokenPlain` and the family is still active (`ListActiveForPerson` length 1).
   - New test: advance `*clock` by 31 s, present the original again → `ErrRefreshRejected` and the family is revoked.
2. Run `go test ./internal/identity/...`. Expect compile FAILs.
3. Implement in `family.go`:
```go
// RotationOutcome distinguishes a fresh rotation from an idempotent replay.
type RotationOutcome int

const (
	// RotationIssued — the presented token was consumed and a successor appended.
	RotationIssued RotationOutcome = iota + 1
	// RotationGraceReplay — the presented token was consumed ≤ graceWindow ago
	// and its successor is still the unconsumed head; nothing changed.
	RotationGraceReplay
)

// Rotate keeps the original strict contract (no grace window).
func (f *Family) Rotate(presentedHash TokenHash, newHash TokenHash, ttl time.Duration, now time.Time) error {
	_, err := f.RotateWithGrace(presentedHash, newHash, ttl, 0, now)
	return err
}

// RotateWithGrace consumes the presented token and appends
// candidateSuccessor — or, when the presented token was consumed at most
// graceWindow ago AND its direct successor is still the unconsumed chain
// head AND equals candidateSuccessor, returns RotationGraceReplay with no
// state change (concurrent-refresh tolerance; ADR 0068 D10). Every other
// consumed-token presentation is reuse: the family is revoked
// (ErrReuseDetected, RFC 9700 rotation canon).
func (f *Family) RotateWithGrace(presentedHash, candidateSuccessor TokenHash, ttl, graceWindow time.Duration, now time.Time) (RotationOutcome, error) {
	if f.IsRevoked() {
		return 0, fmt.Errorf("%w", ErrRevoked)
	}
	if candidateSuccessor.IsZero() {
		return 0, fmt.Errorf("%w: new token hash required", ErrInvalid)
	}
	if ttl <= 0 {
		return 0, fmt.Errorf("%w: ttl must be positive", ErrInvalid)
	}
	idx := f.findToken(presentedHash)
	if idx < 0 {
		return 0, fmt.Errorf("%w", ErrUnknownToken)
	}
	now = now.UTC()
	if f.tokens[idx].IsConsumed() {
		if f.isGraceReplay(idx, candidateSuccessor, graceWindow, now) {
			return RotationGraceReplay, nil
		}
		f.revokedAt = now
		f.revokeReason = "reuse_detected"
		f.recordEvent(RevokedEvent{FamilyID: f.id, PersonID: f.personID, TenantID: f.tenantID, Reason: "reuse_detected", At: now})
		return 0, fmt.Errorf("%w", ErrReuseDetected)
	}
	if f.tokens[idx].IsExpired(now) {
		return 0, fmt.Errorf("%w: token expired at %v", ErrExpired, f.tokens[idx].ExpiresAt())
	}
	// ... existing "Mint next generation" body unchanged, using candidateSuccessor as the hash ...
	return RotationIssued, nil
}

func (f *Family) isGraceReplay(idx int, candidate TokenHash, grace time.Duration, now time.Time) bool {
	if grace <= 0 {
		return false
	}
	consumed := f.tokens[idx]
	if now.Sub(consumed.consumedAt) > grace {
		return false
	}
	head := f.CurrentToken()
	return head != nil && head.id.Equal(consumed.replacedByID) && head.hash.Equal(candidate) && !head.IsExpired(now)
}
```
   Remove the old `Rotate` body, which now lives in `RotateWithGrace`. If `TokenID.Equal` doesn't exist, use `==`.

   In `refreshmint.go`, add the imports `crypto/hkdf`, `crypto/hmac` and `errors`:
```go
// Successor deterministically derives the refresh token that replaces
// presentedPlain in familyID: HMAC-SHA256(key, label‖familyID‖presented).
// Deterministic so a re-presentation inside the grace window re-derives the
// SAME successor while only hashes are ever persisted (ADR 0068 D10).
func Successor(key []byte, familyID refreshtoken.FamilyID, presentedPlain string) (Pair, error) {
	if len(key) < 32 {
		return Pair{}, errors.New("refreshmint: successor key must be ≥32 bytes")
	}
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte("leadkart-refresh-successor-v1\x00"))
	mac.Write([]byte(familyID.String()))
	mac.Write([]byte{0})
	mac.Write([]byte(presentedPlain))
	plaintext := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	hash, err := refreshtoken.NewTokenHash(HashOf(plaintext))
	if err != nil {
		return Pair{}, fmt.Errorf("refreshmint: wrap successor hash: %w", err)
	}
	return Pair{Plaintext: plaintext, Hash: hash}, nil
}

// DeriveSuccessorKey derives the successor-HMAC key from the JWT signing
// secret via HKDF-SHA256 with a dedicated info label (RFC 5869 domain
// separation) — no new secret to provision. Rotating the JWT key ends any
// in-flight grace replays (they answer refresh_rejected; harmless).
func DeriveSuccessorKey(jwtSigningKey []byte) ([]byte, error) {
	return hkdf.Key(sha256.New, jwtSigningKey, nil, "leadkart refresh-token successor v1", 32)
}
```
   In `refresh.go`:
   - Add the fields `graceWindow time.Duration` and `successorKey []byte` and the matching constructor parameters, with `if len(successorKey) < 32 { panic("command: NewRefreshHandler successor key ≥32 bytes required") }`.
   - In `Handle`, replace step 3 (`refreshmint.Mint()`) with:
     ```go
     mintPair, err := refreshmint.Successor(h.successorKey, family.ID(), cmd.RefreshTokenPlain)
     if err != nil { return RefreshResult{}, fmt.Errorf("refresh: derive successor: %w", err) }
     ```
   - In the closure, replace `f.Rotate(...)` with:
     ```go
     outcome, err := f.RotateWithGrace(presentedHash, mintPair.Hash, h.refreshTTL, h.graceWindow, now)
     if err == nil { return outcome == refreshtoken.RotationIssued, nil }
     ```
     Keep the rest of the closure. A grace replay continues to steps 5–7 and returns `mintPair.Plaintext`, the same successor.

   In `main.go`:
   ```go
   succKey, err := refreshmint.DeriveSuccessorKey([]byte(cfg.JWT.SigningKey))
   if err != nil { return identityWiring{}, fmt.Errorf("derive refresh successor key: %w", err) }
   ```
   and `command.NewRefreshHandler(..., cfg.Refresh.AbsoluteTTL, cfg.Refresh.GraceWindow, succKey)`. Update the other callers; test rigs use `[]byte("0123456789abcdef0123456789abcdef")`.
4. Integration test `refresh_grace_e2e_test.go`. The fixture clock is fixed, so every replay falls inside the window.
```go
//go:build integration

package main

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/leadkart/leadkart-go/internal/identity/ports"
)

func TestE2E_Refresh_GraceReplayThenReuseRevokes(t *testing.T) {
	f := newE2EFixture(t)
	tn := f.registerAndLogin(t, "grace")
	refresh := func(tok string) (int, ports.LoginResponse) {
		resp := f.postJSON(t, "/api/v1/auth/refresh", ports.RefreshRequest{RefreshToken: tok})
		var lr ports.LoginResponse
		_ = json.Unmarshal(resp.body, &lr)
		return resp.status, lr
	}
	s1, a := refresh(tn.RefreshToken)
	s2, b := refresh(tn.RefreshToken) // concurrent BFF refresh with the same token
	if s1 != http.StatusOK || s2 != http.StatusOK || a.RefreshToken != b.RefreshToken {
		t.Fatalf("grace replay: %d %d same=%v", s1, s2, a.RefreshToken == b.RefreshToken)
	}
	s3, c := refresh(a.RefreshToken) // successor used → chain moves on
	if s3 != http.StatusOK {
		t.Fatalf("rotate successor: %d", s3)
	}
	if s4, _ := refresh(tn.RefreshToken); s4 != http.StatusUnauthorized { // head moved → reuse
		t.Fatalf("stale replay: %d, want 401", s4)
	}
	if s5, _ := refresh(c.RefreshToken); s5 != http.StatusUnauthorized { // family revoked
		t.Fatalf("after reuse the family must be revoked: %d", s5)
	}
}
```
5. Run G1–G6. For G7: `go test -tags=integration -p 1 -count=1 ./internal/identity/... ; go test -tags=integration -p 1 -count=1 -run 'TestE2E_Refresh|TestE2E_' ./cmd/api/...`. `refresh_token_repository_pg_test` proves that `(false, nil)` persists nothing.
6. OpenAPI for `POST /auth/refresh`: add the description "Re-presenting a just-rotated token within the grace window (default 30 s) returns the same successor; outside it, or after the successor was used, the family is revoked (401 refresh_rejected)."
7. Commit: `fix(identity): refresh rotation grace window with deterministic successor (ADR 0068 D10)`.

---

## Task 12 — Enforce the tenant password policy (item 8, D13)

**Files:**
- Modify `internal/identity/domain/tenant/settings.go` (+ `tenant_test.go` or a new `password_policy_test.go`)
- Modify `internal/identity/app/command/register_tenant.go`, `user_create.go`, `change_password.go`, `confirm_password_reset.go` (+ their tests)
- Modify `internal/identity/ports/http.go` and `users_http.go`, plus `errcodes.go`
- Modify `cmd/api/main.go` and the other constructor callers
- Modify `api/openapi.yaml`
- Create `cmd/api/password_policy_e2e_test.go`

**Interfaces produced:**
```go
// tenant
var ErrPasswordPolicy error
type PasswordPolicyViolation struct{ Rules []string }   // Unwrap() == ErrPasswordPolicy
const PasswordRuleMinLength, PasswordRuleUppercase, PasswordRuleLowercase, PasswordRuleDigit, PasswordRuleSymbol = "min_length","requires_uppercase","requires_lowercase","requires_digit","requires_symbol"
func (p PasswordPolicy) Check(plaintext string) error
// command constructors (new deps)
NewCreateUserHandler(uow, persons, memberships, tenants tenant.Repository, now, newPersonID, newMembershipID)
NewChangePasswordHandler(persons, tenants tenant.Repository, breachChecker, now)       // ChangePasswordCommand gains TenantID tenant.ID
NewConfirmPasswordResetHandler(persons, memberships membership.Repository, tenants tenant.Repository, breachChecker, now)
```

1. Write the failing domain test:
```go
func TestPasswordPolicy_Check(t *testing.T) {
	t.Parallel()
	strict := tenant.DefaultPasswordPolicy()
	lenient, err := tenant.NewPasswordPolicy(8, false, false, false, false, 5, 15)
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name   string
		policy tenant.PasswordPolicy
		pw     string
		rules  []string
	}{
		{"strict ok", strict, "Correct-Horse-9!", nil},
		{"strict short+classes", strict, "abc", []string{tenant.PasswordRuleMinLength, tenant.PasswordRuleUppercase, tenant.PasswordRuleDigit, tenant.PasswordRuleSymbol}},
		{"spaces are not symbols", strict, "Correct Horse Battery 9", []string{tenant.PasswordRuleSymbol}},
		{"lenient length only", lenient, "abcdefgh", nil},
		{"zero policy = default", tenant.PasswordPolicy{}, "abc", []string{tenant.PasswordRuleMinLength, tenant.PasswordRuleUppercase, tenant.PasswordRuleDigit, tenant.PasswordRuleSymbol}},
		{"rune length", lenient, "ééééééé", []string{tenant.PasswordRuleMinLength}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := tc.policy.Check(tc.pw)
			var v *tenant.PasswordPolicyViolation
			if tc.rules == nil {
				if err != nil {
					t.Fatalf("unexpected %v", err)
				}
				return
			}
			if !errors.As(err, &v) || !errors.Is(err, tenant.ErrPasswordPolicy) || !slices.Equal(v.Rules, tc.rules) {
				t.Fatalf("got %v, want rules %v", err, tc.rules)
			}
		})
	}
}
```
   Command tests:
   - `RegisterTenant` with `AdminPassword: "weak"` returns an error satisfying `errors.Is(err, tenant.ErrPasswordPolicy)` and adds no tenant.
   - `CreateUser` uses a tenant in `tenanttest.FakeRepository` whose settings policy is lenient 8. `"short"` fails and `"longenough"` succeeds.
   - `ChangePassword` with `TenantID` set and a weak new password returns `ErrPasswordPolicy`, and the stamp is unchanged.
   - `ConfirmPasswordReset` with a Person whose active membership's tenant has the default policy and a weak password returns `ErrPasswordPolicy`.
2. Run `go test ./internal/identity/...`. Expect FAILs.
3. Implement in `settings.go`, adding the imports `errors`, `strings`, `unicode`, `unicode/utf8`:
```go
// ErrPasswordPolicy is the sentinel for a password failing the tenant policy.
var ErrPasswordPolicy = errors.New("tenant: password does not satisfy the password policy")

// Wire-stable rule codes surfaced in the 422 `errors` map.
const (
	PasswordRuleMinLength = "min_length"
	PasswordRuleUppercase = "requires_uppercase"
	PasswordRuleLowercase = "requires_lowercase"
	PasswordRuleDigit     = "requires_digit"
	PasswordRuleSymbol    = "requires_symbol"
)

// PasswordPolicyViolation lists every failed rule.
type PasswordPolicyViolation struct{ Rules []string }

func (v *PasswordPolicyViolation) Error() string {
	return ErrPasswordPolicy.Error() + ": " + strings.Join(v.Rules, ", ")
}

// Unwrap lets errors.Is(err, ErrPasswordPolicy) match.
func (v *PasswordPolicyViolation) Unwrap() error { return ErrPasswordPolicy }

// Check validates plaintext against the policy (rune length; classes via
// unicode; symbol = punctuation or symbol, NOT whitespace). A zero policy
// (rows predating settings) is evaluated as DefaultPasswordPolicy.
func (p PasswordPolicy) Check(plaintext string) error {
	if p.IsZero() {
		p = DefaultPasswordPolicy()
	}
	var upper, lower, digit, symbol bool
	for _, r := range plaintext {
		switch {
		case unicode.IsUpper(r):
			upper = true
		case unicode.IsLower(r):
			lower = true
		case unicode.IsDigit(r):
			digit = true
		case unicode.IsPunct(r) || unicode.IsSymbol(r):
			symbol = true
		}
	}
	var failed []string
	if utf8.RuneCountInString(plaintext) < p.minLength {
		failed = append(failed, PasswordRuleMinLength)
	}
	if p.requireUppercase && !upper {
		failed = append(failed, PasswordRuleUppercase)
	}
	if p.requireLowercase && !lower {
		failed = append(failed, PasswordRuleLowercase)
	}
	if p.requireDigit && !digit {
		failed = append(failed, PasswordRuleDigit)
	}
	if p.requireSymbol && !symbol {
		failed = append(failed, PasswordRuleSymbol)
	}
	if len(failed) > 0 {
		return &PasswordPolicyViolation{Rules: failed}
	}
	return nil
}
```
   Commands:
   - **register_tenant.go:** at the top of `Handle`, before `hashAdminPassword`, add:
     ```go
     if err := tenant.DefaultPasswordPolicy().Check(cmd.AdminPassword); err != nil { return RegisterTenantResult{}, err }
     ```
   - **user_create.go:** add the `tenants tenant.Repository` dep (include it in the nil-panic). Before `hashPassword`, add:
     ```go
     tn, err := h.tenants.GetByID(ctx, cmd.TenantID)
     if err != nil { return CreateUserResult{}, fmt.Errorf("create user: load tenant: %w", err) }
     if err := tn.Settings().PasswordPolicy().Check(cmd.Password); err != nil { return CreateUserResult{}, err }
     ```
   - **change_password.go:** add `TenantID tenant.ID` to the command and a `tenants` dep. After the "same as current" check (step 4), add:
     ```go
     policy := tenant.DefaultPasswordPolicy()
     if tn, terr := h.tenants.GetByID(ctx, cmd.TenantID); terr == nil {
         policy = tn.Settings().PasswordPolicy()
     } else if !errors.Is(terr, tenant.ErrNotFound) {
         return fmt.Errorf("change_password: load tenant: %w", terr)
     }
     if err := policy.Check(cmd.NewPassword); err != nil { return err }
     ```
     Skip the lookup when `cmd.TenantID.IsZero()`.
   - **confirm_password_reset.go:** add the `memberships` and `tenants` deps. Before the breach check, add:
     ```go
     policy := tenant.DefaultPasswordPolicy()
     if m, merr := h.memberships.GetActiveForPerson(ctx, p.ID()); merr == nil {
         if tn, terr := h.tenants.GetByID(ctx, m.TenantID()); terr == nil { policy = tn.Settings().PasswordPolicy() }
     }
     if err := policy.Check(cmd.NewPassword); err != nil { return err }
     ```

   Ports:
   - In `errcodes.go`, add `ErrCodePasswordPolicyViolation = "password_policy_violation"`.
   - In `http.go`, add the helper:
```go
// writePasswordPolicyError answers 422 password_policy_violation with the
// failed rule codes under field (RFC 9457 errors map). Reports false when
// err is not a policy violation.
func writePasswordPolicyError(w http.ResponseWriter, field string, err error) bool {
	var v *tenant.PasswordPolicyViolation
	if !errors.As(err, &v) {
		return false
	}
	const detail = "password does not satisfy the tenant password policy"
	writeJSON(w, http.StatusUnprocessableEntity, ErrorResponse{
		Type: problemType(ErrCodePasswordPolicyViolation), Title: http.StatusText(http.StatusUnprocessableEntity),
		Status: http.StatusUnprocessableEntity, Detail: detail, Error: ErrCodePasswordPolicyViolation,
		Message: detail, Errors: map[string][]string{field: v.Rules},
	})
	return true
}
```
   - Call it as the first error check: `handleRegisterTenant` with `"admin_password"`, `handleCreateUser` with `"password"`, `handleChangePassword` with `"new_password"`, `handleResetPassword` with `"new_password"`. For example: `if writePasswordPolicyError(w, "new_password", err) { return }`.
   - `handleChangePassword` passes `TenantID: tenant.ID(c.TenantID)`.

   Wiring: update the `main.go` constructor calls to pass `tenants` / `memberships`, and update the test rigs.
4. Integration test `password_policy_e2e_test.go`:
```go
//go:build integration

package main

import (
	"net/http"
	"slices"
	"testing"

	"github.com/leadkart/leadkart-go/internal/identity/ports"
)

func TestE2E_PasswordPolicy_EnforcedOnCreateUser(t *testing.T) {
	f := newE2EFixture(t)
	owner := f.registerAndLogin(t, "pwpol")
	resp := f.authedJSON(t, http.MethodPost, "/api/v1/users", owner.AccessToken,
		ports.CreateUserRequest{Email: "weak@e2e.test", Password: "weak", FirstName: "W", LastName: "K"})
	if resp.status != http.StatusUnprocessableEntity {
		t.Fatalf("weak password: %d %s", resp.status, resp.body)
	}
	er := decodeError(t, resp.body)
	if er.Error != "password_policy_violation" || !slices.Contains(er.Errors["password"], "min_length") {
		t.Fatalf("body: %s", resp.body)
	}
}
```
   Then fix the fixture passwords repo-wide. Run `go test ./... 2>&1 | grep -i "password"` and `grep -rn 'Password:\s*"' --include=*_test.go internal cmd`, and replace weak literals with policy-compliant ones (for example `"Correct-Horse-Battery-9!"`).
5. OpenAPI:
   - Add `'422'` responses referencing `ValidationError` on `POST /tenants`, `POST /users`, `POST /auth/change-password`, `POST /auth/reset-password`, described as `password_policy_violation` with the `errors.<field>` rule codes.
   - Document the rule codes in the `info.description` error-codes paragraph.
6. Run G1–G6. For G7: `go test -tags=integration -p 1 -count=1 ./internal/identity/... ; go test -tags=integration -p 1 -count=1 -run 'TestE2E_' ./cmd/api/...`.
7. Commit: `feat(identity): enforce tenant password policy on register/create/change/reset (ADR 0068 D13)`.

---

## Task 13 — Small correctness fixes (item 10)

**Files:**
- Modify `authn/authn.go` (`writeError`) and `authn_test.go`
- Modify `api/openapi.yaml`
- Modify `internal/common/openapi/all_routes.yaml` (sync)
- Modify `Taskfile.yml`
- Create `internal/architecture/openapi_embed_arch_test.go`

1. Write the failing tests.
   - **authn:** `TestRequirePermission_ClaimsLackPermission_Returns403` additionally asserts `rec.Header().Get("WWW-Authenticate") == ""`. The 401 tests keep asserting a `Bearer` challenge.
   - **arch:**
```go
package architecture_test

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// TestArch_EmbeddedSpecMatchesCanonical — ADR 0050 (OpenAPI as code of
// record): the spec served at GET /openapi.yaml (embedded
// internal/common/openapi/all_routes.yaml) must equal api/openapi.yaml
// (modulo CRLF). Fix: `task openapi:sync`.
//
// arch-test:no-negative-fixture (pure file equality; RED proof = edit either file)
func TestArch_EmbeddedSpecMatchesCanonical(t *testing.T) {
	t.Parallel()
	canonical, err := os.ReadFile(apiSpecPath(t))
	if err != nil {
		t.Fatalf("read api/openapi.yaml: %v", err)
	}
	embedded, err := os.ReadFile(filepath.Join(internalDir(t), "common", "openapi", "all_routes.yaml"))
	if err != nil {
		t.Fatalf("read embedded spec: %v", err)
	}
	norm := func(b []byte) []byte { return bytes.ReplaceAll(b, []byte("\r\n"), []byte("\n")) }
	if !bytes.Equal(norm(canonical), norm(embedded)) {
		t.Fatal("internal/common/openapi/all_routes.yaml is stale vs api/openapi.yaml — run `task openapi:sync`")
	}
}
```
2. Run `go test ./internal/identity/ports/authn/ ; go test -run TestArch_EmbeddedSpecMatchesCanonical ./internal/architecture/`. Expect both to FAIL.
3. Implement.
   - **authn.go `writeError`:** guard the header: `if status == http.StatusUnauthorized { w.Header().Set("WWW-Authenticate", `Bearer realm="leadkart"`) }`.
   - **OpenAPI:** the `TenantDto` status enum becomes `[pending, active, suspended, pending_deletion, deleted]`, matching `tenant.Status.String()`.
   - **OpenAPI `info.description` Auth paragraph:** "Access tokens live **600 seconds (10 min)** (`jwt.AccessTokenTTL`); `access_token_expires_at` in the login/refresh body is authoritative — refresh before it. 401 + `WWW-Authenticate: Bearer` = re-authenticate (`unauthenticated` / `stale_token`); 403 never carries the challenge."
   - **OpenAPI `LoginResponse.access_token_expires_at`:** add `description: 'UTC expiry of access_token (issued + 600 s).'`.
   - **Taskfile.yml:** add the task below, then run `task openapi:sync`.
```yaml
  openapi:sync:
    desc: 'Copy api/openapi.yaml into the embedded spec served at GET /openapi.yaml (ADR 0050). Run after every spec edit — TestArch_EmbeddedSpecMatchesCanonical enforces.'
    cmds:
      - cp api/openapi.yaml internal/common/openapi/all_routes.yaml
```
4. Run G1–G6. For G7: `go test -tags=integration -p 1 -count=1 -run 'TestE2E_Auth' ./cmd/api/...`.
5. Commit: `fix: WWW-Authenticate only on 401; tenant status enum; embedded spec synced + gated; document 10-min access TTL`.

---

## Task 14 — Local real-backend runbook for the web BFF

**File:** create `docs/LOCAL_BACKEND.md`. These are documentation steps only; build nothing else.

1. Write the doc with these exact commands, run in Git Bash from `D:/Development/leadkart-go`:
```bash
task docker:env          # mints docker/.env (JWT key + SuperAdmin creds); prints them
# BFF on the host reaches the api container via the docker bridge: trust it for X-Forwarded-For
echo 'LEADKART_HTTP__TRUSTED_PROXIES=172.16.0.0/12,192.168.0.0/16,127.0.0.1/32,::1/128' >> docker/.env
# optional: relax credential-route limits for Playwright runs
echo 'LEADKART_RATE_LIMIT__LOGIN__PER_MINUTE=600' >> docker/.env
task docker:up:full      # postgres:5432 + redis:6379 + migrate + bootstrap + api (:8080 public, :9090 admin)
curl -fsS http://localhost:9090/ready && echo READY
# optional async side (emails, notifications, read models): worker on the host
LEADKART_POSTGRES__DSN='postgres://leadkart:leadkart_dev@localhost:5432/leadkart?sslmode=disable' \
LEADKART_REDIS__ADDR=localhost:6379 \
LEADKART_JWT__KEY_ID=$(sed -n 's/^LEADKART_JWT__KEY_ID=//p' docker/.env) \
LEADKART_JWT__SIGNING_KEY=$(sed -n 's/^LEADKART_JWT__SIGNING_KEY=//p' docker/.env) \
go run ./cmd/worker
```
2. Document the seeded logins.
   - **Platform operator:** the bootstrap SuperAdmin `superadmin@leadkart.local` / `LeadKart!Dev2026` (from `scripts/devenv`). `is_super_user=true`, `must_change_password=false`.
   - **Tenant admin:** no seed exists, so create one through the API:
```bash
OP=$(curl -fsS -X POST localhost:8080/api/v1/auth/login -H 'Content-Type: application/json' \
  -d '{"email":"superadmin@leadkart.local","password":"LeadKart!Dev2026","device_label":"dev"}' | sed -n 's/.*"access_token":"\([^"]*\)".*/\1/p')
curl -fsS -X POST localhost:8080/api/v1/tenants -H "Authorization: Bearer $OP" -H 'Content-Type: application/json' \
  -d '{"slug":"acme-pharma","legal_name":"Acme Pharma Pvt Ltd","display_name":"Acme Pharma","admin_email":"owner@acme.local","admin_password":"Acme-Owner-2026!","admin_first_name":"Asha","admin_last_name":"Owner"}'
TA=$(curl -fsS -X POST localhost:8080/api/v1/auth/login -H 'Content-Type: application/json' \
  -d '{"email":"owner@acme.local","password":"Acme-Owner-2026!","device_label":"dev"}' | sed -n 's/.*"access_token":"\([^"]*\)".*/\1/p')
curl -fsS -X POST localhost:8080/api/v1/auth/change-password -H "Authorization: Bearer $TA" -H 'Content-Type: application/json' \
  -d '{"current_password":"Acme-Owner-2026!","new_password":"Acme-Owner-2026!b"}'   # first-login rotation (must_change_password)
# tenant admin login from now on: owner@acme.local / Acme-Owner-2026!b
```
3. Web side: set `GO_API_URL=http://localhost:8080` in leadkart-web, and send `X-Forwarded-For` (see the BFF section). Teardown: `task docker:down` (keeps data) or `task docker:wipe` (resets it). Smoke check: `task test:smoke`, or `bash scripts/smoke.sh`.
4. Add a link in `README.md` under the dev section.
5. Validate by running the commands once and pasting the outputs into the commit message. Gates G1, G2 and G5.
6. Commit: `docs: local Go API + Postgres runbook for the web BFF (seeded operator + tenant admin)`.

---

## Task 15 — Close-out of the decision record (no code)

Re-read ADR 0068 against what actually shipped. If any decision changed during implementation, write a superseding note in a **new** ADR. ADRs are sealed; never edit 0068 in place. If nothing changed, skip this task.

## Task 16 — Final verification (no new code)

Run G1–G6, then `task test:int ; echo INT=$?` (serial, about 15 min), then `task test:smoke` against `task docker:up:full`. Report every marker. Do not push.

---

## Consequences for the web BFF (leadkart-web)

1. **New 403s on identity routes.**
   - `GET /users`, `GET /roles`, `GET /tenants/{id}`, the audit feed and every mutation now need permissions. CompanyOwner passes through `tenant.admin`.
   - Self-service profile `GET`/`PATCH /users/{own membership}` still works for everyone.
   - Gate UI on the new `capabilities.effective_permissions`, not on `permissions[]`, which only contains `tenant.admin` for owners.
   - New codes: `forbidden`, `self_modification_forbidden` (own roles/overrides/deactivate/manager), `grant_exceeds_authority` (granting or assigning beyond your own authority), plus 404 `role_not_found` on role assignment with an unknown role.
2. **`must_change_password` is enforced.**
   - Every route except change-password and capabilities returns 403 `password_change_required` while the flag is set.
   - `capabilities.must_change_password` (always present) and the refresh response carry it.
   - After change-password the old access token returns 401 `stale_token`. Log in again with the new password.
3. **`X-Tenant-Id` is now honoured** for platform operators.
   - Before this change it was silently ignored, so operator actions hit the platform tenant.
   - Non-platform callers' headers are ignored.
   - New codes: 400 `invalid_tenant_scope`, 404 `tenant_scope_not_found`, 400 `tenant_scope_conflict`. The conflict fires when the header differs from `/tenants/{id}` in the path; use `scoped:false` for cross-tenant path calls.
   - Capabilities ignores the header.
4. **Tenant registration** (`POST /api/v1/tenants`) now needs a platform operator token with `platform.tenants.create`. The web's operator console already sends a Bearer token; anonymous calls get 401.
5. **Rate limits.**
   - The BFF **must** send `X-Forwarded-For: <browser IP>` (SvelteKit `event.getClientAddress()`) on every Go call.
   - Ops must set `LEADKART_HTTP__TRUSTED_PROXIES` to the BFF's address or CIDR. Until both are in place, all users share one bucket. The new per-route limits (login 30/min, refresh 120/min, reset-request 5/min, …) would then throttle everyone together.
   - Handle 429 + `Retry-After` on login and refresh.
6. **Refresh** is now tolerant of concurrent refreshes. Two refreshes with the same token inside 30 s get the same successor, so the "random logout" class of bugs goes away. A stale token after that still revokes the family (401 `refresh_rejected`).
7. **Impersonation.**
   - The scoped token acts as the target tenant's admin on tenant routes.
   - Self-service routes (change-password, email change, sessions, `/me/activity`, permission-request submit/approve/deny/cancel) return 403 `impersonation_not_allowed`. Hide those UIs during impersonation.
   - After "Stop impersonating" (`DELETE` session) the scoped token returns 401 `stale_token` immediately. Fall back to the operator tokens.
8. **Password policy.** Create-user, change-password, reset and registration can return 422 `password_policy_violation` with `errors.{password|new_password|admin_password}: [min_length, requires_uppercase, requires_lowercase, requires_digit, requires_symbol]`. The default policy is 12+ characters with all four classes; spaces don't count as symbols. Update test fixtures.
9. **Login enumeration.** The 10th failed attempt in 15 min returns 423 `account_locked` + `Retry-After` for *any* email, real or not. Keep the countdown UX. Request-password-reset always takes ≥300 ms.
10. **`WWW-Authenticate`** is sent on 401 only. The "session expired" heuristic (401 + header) still works; 403 no longer triggers it.
11. **Contract.**
    - The tenant `status` enum is now `pending_deletion`/`deleted`. Regenerate the types from `api/openapi.yaml`; the embedded `/openapi.yaml` matches it from now on.
    - `CapabilitiesDto` gains the required fields `effective_permissions` and `must_change_password`. If the zod schema is `.strict()`, relax it.
    - The access-token TTL is documented as 600 s; `access_token_expires_at` is authoritative.
12. **Local backend:** see `docs/LOCAL_BACKEND.md` (Task 14). `GO_API_URL=http://localhost:8080`. Operator `superadmin@leadkart.local` / `LeadKart!Dev2026`; tenant admin created via the API (`owner@acme.local`, password rotated on first login).

### Critical files for implementation
- `D:\Development\leadkart-go\internal\identity\ports\authn\authn.go` (with the new `authorize.go`, `tenant_scope.go` and `security_stamp.go` beside it)
- `D:\Development\leadkart-go\internal\identity\ports\http.go`
- `D:\Development\leadkart-go\internal\identity\domain\permission\permission.go` (and the new `implication.go`)
- `D:\Development\leadkart-go\internal\identity\domain\refreshtoken\family.go` and `D:\Development\leadkart-go\internal\identity\app\command\refresh.go`
- `D:\Development\leadkart-go\cmd\api\main.go` and `D:\Development\leadkart-go\cmd\api\cross_tenant_e2e_test.go` (the E2E fixture every integration task extends)
