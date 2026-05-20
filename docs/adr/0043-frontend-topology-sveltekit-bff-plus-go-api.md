# ADR 0043 — Frontend topology: SvelteKit BFF (adapter-node) + Go API

**Status:** Accepted
**Date:** 2026-05-19

## Context

Phase 1.5 shipped the Go API as a pure bearer-token surface. JWT auth via
`Authorization: Bearer <access>` header, refresh tokens returned in
response body, no cookies, no CSRF. This matches Stripe / GitHub / Auth0 /
Microsoft Graph canon for any **API** that serves a multi-client
audience (mobile apps, partner integrations, server-to-server callers).

The frontend is **SvelteKit + Svelte 5**, deployed via `adapter-node`.

Phase 1.5 also surfaced the question: *how should the browser hold the
refresh token without exposing it to XSS?* The naïve answers are all
unacceptable:

- `localStorage` — XSS-exfiltratable. Industry-recognised antipattern.
- `sessionStorage` — same risk, slightly smaller window.
- In-memory only — survives tab lifetime; forces re-login on tab close.

The correct answer is `HttpOnly + Secure + SameSite=Lax` cookies. But
cookies can't be the Go API's job — that would tightly couple the API
to one specific UI mechanism, breaking the multi-client property (mobile
apps don't send cookies; partner integrations use bearer keys).

The canonical resolution: **a Node-runtime BFF (Backend-For-Frontend)
sits between the browser and the Go API**, holding the cookie session
and forwarding bearer-token requests server-to-server.

Production evidence at scale:

| Company | System-of-record backend | BFF for web frontend |
|---|---|---|
| Stripe | Ruby on Rails | Node BFF for dashboard.stripe.com |
| LinkedIn | Java | Node BFF (introduced 2011, expanded) |
| PayPal | Java | Node BFF — open-sourced "Kraken" framework |
| Walmart | Java | Node BFF for walmart.com |
| Netflix | Java microservices | Node BFF for netflix.com web |
| Airbnb | Ruby/Java/Kotlin | Node BFF for airbnb.com |
| Etsy | PHP | Node BFF |
| Slack | PHP (Hack/HHVM) | Node BFF |
| Twitter (early) | Ruby | Node BFF, then full Node migration |

In every case the system-of-record backend is **not** Node, and a Node
BFF wraps it for the browser-facing tier. The BFF must be Node
specifically because React / Svelte / Vue components render natively in
the Node runtime (no other runtime gives SSR + hydration parity).

For SvelteKit specifically: **the BFF doesn't need to be a separate
service**. SvelteKit's own server runtime (when deployed via
`adapter-node` / `adapter-vercel` / `adapter-cloudflare` / `adapter-bun`)
IS a Node BFF. The framework is structurally a BFF + frontend in one
codebase. The only configuration that makes SvelteKit NOT a BFF is
`adapter-static` (pre-rendered HTML only, no server runtime) — which
LeadKart explicitly rejects for that reason.

Constraints inherited from preceding ADRs:

- ADR 0001 — modular monolith (Go side; the BFF is a separate deployable).
- ADR 0007 — Go API uses stdlib `net/http` ServeMux; pure bearer auth.
- ADR 0011 — Go API issues JWT + refresh token families; bearer-only.
- ADR 0039 — Per-request scope selection via JWT.is_platform + X-Tenant-Id
  header. The BFF reads scope from the session cookie, forwards as
  header to Go API. Unchanged from the Go side.

Non-goals:

- Re-implementing SSR in Go (Hashicorp tried embedding V8 in Go around
  2018 with Terraform's plugin system — operationally unsustainable;
  abandoned). SSR happens in Node.
- Two-process deployment of a single binary (Go + Node fused). Each runs
  as its own process; co-located on the same host when ops simplicity
  matters, separated when team autonomy matters.
- Cookie-based auth on the Go API. The Go API stays pure bearer.

## Decision

**SvelteKit (adapter-node) is the canonical web BFF in front of the Go
API. The Go API stays bearer-only. The browser only ever sees
HttpOnly cookies; the Go API only ever sees Bearer tokens; the BFF is
the translator.**

### The architecture diagram

```
┌──────────────────────────────────────────────────────────────┐
│  Browser                                                       │
│  ┌──────────────────────────────────────────────────────┐    │
│  │ HttpOnly cookies (managed by BFF):                   │    │
│  │   lk_session     (encrypted: refresh+access tokens)  │    │
│  │   lk_csrf        (CSRF token, readable by JS for     │    │
│  │                   double-submit pattern)             │    │
│  └──────────────────────────────────────────────────────┘    │
│  Svelte 5 client runtime — SPA navigation after first paint  │
└──────────────────────────┬───────────────────────────────────┘
                           │ HTTPS, same-origin
                           ▼
┌──────────────────────────────────────────────────────────────┐
│  SvelteKit BFF (adapter-node)                                 │
│                                                                │
│  hooks.server.ts ───── reads + decrypts session cookie        │
│                        refreshes access token if expired       │
│                        attaches Authorization: Bearer to       │
│                        outbound fetches                        │
│                                                                │
│  +page.server.ts ──── server-side data loads;                  │
│                       form actions for mutations               │
│                                                                │
│  +server.ts ────────── REST-style server endpoints (when       │
│                       client needs fetch() that hits BFF)      │
│                                                                │
│  +layout.svelte ───── SSR'd shell with identity baked in       │
│                       (first paint shows sidebar + role chip)  │
│                                                                │
│  CSRF middleware ──── verifies double-submit token on every    │
│                       state-mutating request                   │
└──────────────────────────┬───────────────────────────────────┘
                           │ HTTPS or mTLS or plain HTTP if
                           │ co-located; Authorization: Bearer
                           ▼
┌──────────────────────────────────────────────────────────────┐
│  Go API (cmd/api)                                              │
│                                                                │
│  • Pure /api/v1/* surface — exactly as it is today             │
│  • Bearer-token auth, refresh-token family rotation            │
│  • Multi-tenant RLS, X-Tenant-Id header support (ADR 0039)     │
│  • No cookies, no CSRF, no HTML rendering                      │
│  • Same surface also serves mobile + integrations              │
└──────────────────────────────────────────────────────────────┘
```

### Token + cookie boundary

| Layer | Holds what | Why |
|---|---|---|
| Browser | HttpOnly `lk_session` cookie | JS can't read; immune to XSS exfiltration |
| BFF (SvelteKit server) | Cookie ↔ tokens translation; refresh-token rotation | One trusted process; HttpOnly = belt + braces |
| Go API | Stateless JWT verification | Same shape as Stripe / Auth0 — bearer in, bearer out |

### Cookie names + flags

```
lk_session  → HttpOnly; Secure; SameSite=Lax; Path=/; Max-Age=1209600 (14d)
              Encrypted blob (AES-GCM) containing:
              {
                access_token,
                access_token_expires_at,
                refresh_token,
                refresh_token_family_id,
                csrf_token_hash
              }

lk_csrf     → Secure; SameSite=Lax; Path=/; (NOT HttpOnly — JS reads it
              for double-submit). Holds an HMAC-signed CSRF token; the
              hash lives inside lk_session, matched per request.
```

`SameSite=Lax` over `Strict` chosen because operator-paste-link UX
(opening a tenant URL from a Slack message) is meaningful for LeadKart's
operator persona; Strict breaks that.

### Login / logout / refresh flow

**Login:**
1. Browser POSTs `{email, password}` to SvelteKit form action
2. SvelteKit calls Go `POST /api/v1/auth/login` server-to-server
3. Go returns `{access_token, refresh_token, ...}` in body
4. SvelteKit encrypts into `lk_session` cookie, mints fresh `lk_csrf`,
   sets `Set-Cookie` on the response
5. Browser navigates with cookies; tokens never visible to JS

**Authenticated read:**
1. Browser sends cookies on every same-origin request
2. SvelteKit `hooks.server.ts` decrypts session, checks `access_token`
   TTL, calls Go's `/api/v1/auth/refresh` silently if < 30s left
3. SvelteKit attaches `Authorization: Bearer <access>` + forwards request
4. Go returns data; SvelteKit shapes / aggregates / returns to browser

**Authenticated write:**
1. Same as read, PLUS browser submits `X-CSRF-Token` header (read from
   `lk_csrf` cookie via JS)
2. SvelteKit verifies double-submit: HMAC-hash of `X-CSRF-Token` matches
   `csrf_token_hash` inside `lk_session`
3. Mismatch → 403 by BFF; never reaches Go

**Logout:**
1. Browser POSTs `/logout` to SvelteKit
2. SvelteKit calls Go `POST /api/v1/auth/logout` (revokes refresh family)
3. SvelteKit clears both cookies via `Set-Cookie: ...; Max-Age=0`

### What the Go API explicitly does NOT need

- **No `Set-Cookie` support** — cookies live entirely at the BFF
- **No CSRF middleware** — bearer-token APIs are immune to CSRF (the
  token isn't ambiently sent like a cookie)
- **No CORS config for browser** — browser never talks to Go API
  directly; CORS for partner integrations is a separate consideration
- **No knowledge of the BFF's existence** — Go API treats BFF as any
  other bearer-holding client

This is the load-bearing property of the design: **the Go API is
unchanged**. Adding a BFF doesn't refactor the API surface; it adds a
layer ABOVE it. Mobile apps + partner integrations continue calling Go
directly with bearer tokens, ignoring the BFF entirely.

### Deployment shape

Two viable topologies; pick by team-autonomy needs:

**Co-located** (recommended for v0.2-Phase 5):
- SvelteKit BFF + Go API on the same host (Docker compose, K8s pod, or
  systemd units sharing a VM)
- BFF → Go traffic stays on `localhost:8080` — sub-millisecond, no TLS
  needed, no public internet exposure
- One nginx / Caddy / Cloudflare front-door terminates TLS + routes
  by path: `/` → BFF (port 3000), explicit allowlist for
  `/api/v1/...` if needed for external clients
- Single deploy unit (compose / pod) — frontend + backend ship together

**Federated** (Phase 6+ when teams scale):
- BFF + Go on separate hosts behind their own load balancers
- BFF → Go over private subnet (VPC) with mTLS
- Independent deploy cadence; frontend team owns BFF, backend team owns
  API
- More moving parts; only justified when team coordination cost > the
  deploy-together convenience

LeadKart targets **co-located** until the team scales past ~10 engineers.

### What changes vs the current setup

| Concern | Current | After BFF migration |
|---|---|---|
| Browser auth storage | (TBD — frontend not yet talking to Go API in prod) | HttpOnly cookies via BFF |
| Refresh token security | XSS-exfiltratable if frontend uses localStorage | HttpOnly cookie — XSS-immune |
| First-paint identity | Frontend renders empty shell, fetches `/me/capabilities` (CLS) | SvelteKit SSR bakes identity into HTML — no flash |
| Multi-call page renders | Browser fan-out (waterfall) | BFF fan-out server-to-server (parallel) |
| Mobile + integration auth | Bearer | Bearer (unchanged) |
| Go API code | Pure bearer surface | Pure bearer surface (zero changes) |
| New deploy unit | Just Go API | Go API + SvelteKit Node server |

### Migration scope (frontend repo, NOT this repo)

This ADR documents the contract from the Go side. The actual work lives
in the SvelteKit frontend repo:

1. Ensure SvelteKit uses `adapter-node` (or `adapter-vercel` /
   `adapter-cloudflare`) — NOT `adapter-static`
2. Implement `src/lib/server/session.ts` — encrypted cookie helper
   (iron-session, or hand-rolled AES-GCM via `@oslojs/encoding`)
3. Implement `src/hooks.server.ts` — read cookie, refresh access token,
   attach bearer to outbound fetches, set CSRF middleware
4. Convert login/logout/refresh routes to SvelteKit form actions
5. Convert page data loads from client-side `fetch` to
   `+page.server.ts` load functions
6. Add CSRF middleware (double-submit token) to mutating handlers
7. Deploy: SvelteKit Node server on port 3000, Go API on port 8080,
   front-door (Caddy / nginx / Cloudflare) routes paths to either

Estimated 1 week of work for a single engineer fluent in SvelteKit.

## Consequences

**Positive:**

- **Industry-canonical architecture.** Matches Stripe / Netflix / Airbnb
  / Walmart / LinkedIn / PayPal / Etsy / Slack. Anyone hiring a senior
  full-stack engineer expects this shape; no onboarding tax.
- **XSS-immune refresh tokens.** HttpOnly cookies categorically defeat
  the "attacker reads localStorage" class of attack.
- **SSR'd first paint.** Identity baked into HTML on initial render;
  no CLS as the sidebar populates. Operator UX feels "native app".
- **API aggregation for free.** BFF can `Promise.all` 5-8 Go API calls
  per page render (parallel, server-to-server, sub-ms each), ship one
  bundle to the browser. Beats waterfall fetches by orders of magnitude
  on dashboard-style pages.
- **Auth boundary narrowed.** Public surface = BFF only. Go API can sit
  on a private subnet (eventually). Smaller attack surface for partial
  compromises.
- **Tech-org alignment.** Frontend team owns BFF in Node-fluent
  TypeScript. Backend team owns Go API. Each ships independently.
- **Go API stays pure.** No HTML, no cookies, no CSRF. Continues to
  serve mobile + integrations + the BFF identically.
- **Multi-client property preserved.** A future mobile app uses the
  same `/api/v1/*` surface with bearer tokens.

**Negative:**

- **Two runtimes to operate.** Node (for BFF) + Go (for API). Both
  binaries, both deploys, both observability surfaces. Mitigated by
  co-located deployment (one compose / one pod).
- **Two languages in the stack.** TypeScript (BFF) + Go (API). Team
  needs both skills, OR clear ownership boundaries. Industry pattern
  is "frontend team owns BFF" — solo engineers feel the cost more.
- **Cookie size limits.** Browsers cap cookies at 4KB. The encrypted
  session payload (refresh token + access token + claims) must fit.
  At LeadKart's claim sizes (~30 permissions, short tenant IDs) we're
  well under 1KB encrypted. Headroom is fine.
- **CSRF complexity.** Cookie auth needs CSRF protection (bearer
  doesn't). Adds the double-submit token pattern at the BFF — well-
  understood, ~50 LoC in SvelteKit middleware.
- **Two deploy units in CI.** Frontend repo CI + Go repo CI run
  separately. Co-located deployment requires coordinating versions —
  contract-test gates the BFF's expected API shape against the Go
  API's OpenAPI spec (lands as a follow-up).

## Alternatives considered

1. **Go serves SvelteKit static + browser → Go API directly.** Rejected
   per the production-org evidence above. Loses SSR (no Node runtime to
   render Svelte components), forces localStorage for tokens, no API
   aggregation, larger public attack surface. Acceptable only for
   throwaway internal tools.

2. **Go embeds JS runtime (V8) for SSR.** Hashicorp tried this with
   Terraform's HCL2 prelude (2018-2020); operationally unsustainable
   (V8 versioning + memory + GC integration nightmare). Cloudflare
   Workers do it commercially but with a forked V8 + custom isolate
   model — way out of scope. Rejected.

3. **Pure SPA (no SSR), browser holds tokens in memory.** Tokens lost
   on tab close = forced re-login UX. Plus loses the SSR'd first paint.
   Acceptable for ops admin tools at tiny scale; sub-canon for any
   product-facing SPA.

4. **Cookie-based auth on the Go API.** Would tightly couple the API to
   the one specific UI mechanism. Breaks the multi-client property
   (mobile apps don't send cookies). Forces CSRF middleware in Go.
   Couples API release cadence to UI release cadence. Rejected — this
   is the wrong layer for cookie concerns.

5. **Server-rendered HTML in Go (no JS frontend).** Acceptable for
   content sites; awkward for admin dashboards with rich UX (multi-step
   forms, optimistic updates, real-time data). LeadKart's operator + 
   tenant-admin surfaces benefit from rich client-side state, so
   server-rendered-HTML-only is sub-canon.

6. **Separate Go BFF binary (cmd/bff).** Possible if frontend is NOT
   SvelteKit (or uses `adapter-static`). Adds an extra deploy unit + a
   second Go binary to maintain. Reasonable for orgs with no Node
   expertise + pure-SPA frontends. Rejected for LeadKart since we
   already commit to SvelteKit with `adapter-node`.

## Sources

- [SvelteKit Adapter Node docs](https://svelte.dev/docs/kit/adapter-node) — adapter-node deployment shape (the Node BFF runtime)
- [SvelteKit hooks documentation](https://svelte.dev/docs/kit/hooks) — `hooks.server.ts` request lifecycle
- [SvelteKit form actions](https://svelte.dev/docs/kit/form-actions) — `+page.server.ts` action pattern for login/logout
- [iron-session](https://github.com/vvo/iron-session) — encrypted cookie session helper (reference for the lk_session shape)
- [OWASP Cheat Sheet — Cross-Site Request Forgery Prevention](https://cheatsheetseries.owasp.org/cheatsheets/Cross-Site_Request_Forgery_Prevention_Cheat_Sheet.html) — double-submit token pattern
- [OWASP Cheat Sheet — Authentication](https://cheatsheetseries.owasp.org/cheatsheets/Authentication_Cheat_Sheet.html) — HttpOnly + Secure cookie canon
- [Stripe Engineering — Bringing Engineering Together at Stripe](https://stripe.com/blog/) — public posts referencing their Node BFF for dashboard
- [PayPal Engineering — node.js at PayPal](https://medium.com/paypal-tech/node-js-at-paypal-4e2d1d08ce4f) — open-sourced the "Kraken" framework around this pattern
- [LinkedIn Engineering — From 30 lines to 30K](https://engineering.linkedin.com/) — Node BFF history (2011+)
- ADR 0007 — HTTP: stdlib `net/http` ServeMux (the Go-API surface this ADR layers on top of)
- ADR 0011 — Auth: golang-jwt/jwt/v5 + refresh-token families (the bearer-only contract)
- ADR 0039 — Per-request scope selection: JWT.is_platform + X-Tenant-Id header (the scope mechanism the BFF forwards through)
- ADR 0042 — Cache TTL strategy (CapabilitiesTTL keyed by security_stamp; the BFF benefits from this for its server-side `/me/capabilities` load)
