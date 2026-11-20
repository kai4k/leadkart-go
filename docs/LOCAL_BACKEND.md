# Local real backend for the web BFF (leadkart-web)

Run the real Go API, Postgres, Redis and (optionally) the worker on this machine, and
point the SvelteKit BFF at it. **Development only**: every credential below is minted by
the dev tooling or created by the commands here, and the trusted-proxy and rate-limit
values are deliberately loose. Production is `docs/deploy/production-checklist.md`.

Validated start to finish on 2026-11-20 (Windows 11, Docker Desktop 29.5.3 with the Linux
engine, Git Bash, Task 3.50). Run every command in Git Bash from the repository root.
Outputs shown are trimmed from that run.

## 1. Start the stack

```bash
task docker:env        # mints docker/.env: JWT key + the bootstrap SuperAdmin (idempotent)
```

`docker/.env` is git-ignored. `task docker:env` keeps an existing good file, so lines you
append are safe; `task docker:env:reset` rewrites it.

Append the local settings (the compose files pass these through to the api container):

```bash
cat >> docker/.env <<'EOF'
# The BFF runs on the host and reaches the api through Docker's published port, so the
# api sees the Docker network gateway (section 2). DEVELOPMENT ONLY: wide ranges.
LEADKART_HTTP__TRUSTED_PROXIES=172.16.0.0/12,192.168.0.0/16,127.0.0.1/32,::1/128
# Relaxed limits so Playwright runs from one address do not throttle themselves (section 6).
LEADKART_RATE_LIMIT__LOGIN__PER_MINUTE=600
LEADKART_RATE_LIMIT__LOGIN__BURST=200
LEADKART_RATE_LIMIT__REFRESH__PER_MINUTE=1200
LEADKART_RATE_LIMIT__REFRESH__BURST=200
LEADKART_RATE_LIMIT__CHANGE_PASSWORD__PER_MINUTE=60
LEADKART_RATE_LIMIT__CHANGE_PASSWORD__BURST=30
LEADKART_ARGON2__MAX_PER_CLIENT=8
LEADKART_ARGON2__QUEUE_TIMEOUT=10s
# Idle timeout: a session not refreshed for this long ends (default 2h; the web refreshes
# whenever its 10-minute access token runs out, so only a parked tab idles). A day keeps a
# development tab signed in overnight.
LEADKART_REFRESH__SLIDING_TTL=24h
EOF
task docker:up:full    # postgres:5432, redis:6379, migrate, bootstrap, api (:8080 public, :9090 admin)
sleep 8; curl -fsS http://localhost:9090/ready ; echo
```

```
{"postgres":"ok","redis":"ok"}
```

`LEADKART_ENV` is not set by the dev compose file (the api logs `"env":"dev"`). **Do not
set `LEADKART_ENV=production` locally**: production mode refuses a wide or empty
trusted-proxy list, requires `LEADKART_IDENTITY__LOCKOUT_KEY`, and refuses a database role
that owns its tables. The dev api logs one WARN about the missing lockout key; that is expected.

The compose services are `postgres`, `redis`, `migrate` (runs once), `bootstrap` (runs
once) and `api`. **There is no worker service in the dev compose file** (section 7).

## 2. Trusted proxies: which address the api sees

The api believes `X-Forwarded-For` only when the TCP peer is in
`LEADKART_HTTP__TRUSTED_PROXIES`. With the BFF on the host and the api in a container,
the peer is the gateway of the compose network. Observed on this machine (Docker Desktop
for Windows), from the api's request log:

```bash
curl -s -o /dev/null -H 'X-Forwarded-For: 203.0.113.9' http://127.0.0.1:8080/openapi.yaml
docker logs leadkart-api 2>&1 | grep remote_addr | tail -1
docker network inspect docker_default --format '{{range .IPAM.Config}}{{.Subnet}} gw={{.Gateway}}{{end}}'
```

```
..."remote_addr":"172.18.0.1:33234","client_ip":"203.0.113.9"...
172.18.0.0/16 gw=172.18.0.1
```

So on this machine the BFF reaches the api as **172.18.0.1** (the `docker_default` network
gateway; `localhost`, `127.0.0.1` and `[::1]` all arrive that way, because the connection
goes through Docker's port publishing). `172.18.0.0/16` is whichever subnet Docker picked
when it created the network, and `task docker:wipe` recreates the network, so it can
change. **Check it with the two commands above instead of trusting this number.** The wide
range in the `.env` block covers Docker's default pools; a narrower
`LEADKART_HTTP__TRUSTED_PROXIES=<gateway>/32` works too.

**Wide ranges are development only.** `172.16.0.0/12` trusts every Docker network on the
machine. Production lists the BFF's own fixed address or a dedicated subnet and refuses a
prefix wider than `/8`; see `docs/deploy/production-checklist.md`.

The api logs the effective list at boot (`trusted proxies in effect`). If a trusted peer
sends no usable `X-Forwarded-For`, the api logs a WARN (at most once a minute) and every
user then shares one rate-limit bucket.

## 3. Web (leadkart-web) settings

In leadkart-web's `.env.local` (see its `.env.example`):

```
GO_API_URL=http://localhost:8080
NODE_ENV=development
```

- `GO_API_URL` is the only variable that points the BFF at Go. For a built server
  (preview, e2e, production) `SCOPE_COOKIE_SECRET` is also required, see the web's
  `.env.example`.
- The BFF already sends `X-Forwarded-For: <the browser's address>` on every call to Go
  (web commit "every call to Go carries the caller's address"). Nothing to configure for it.
- **Leave `ADDRESS_HEADER` and `XFF_DEPTH` unset locally.** They are for a BFF behind a
  proxy that appends to `X-Forwarded-For`; with nothing in front of the BFF, the browser
  connects to it directly and its socket address is the client.

## 4. Passwords and the forced change (read before writing fixtures)

- **Every password must satisfy the default tenant policy**: at least 12 characters with
  an upper-case letter, a lower-case letter, a digit and a symbol (a space is not a
  symbol). A weaker one is `422 password_policy_violation` with the failed rule codes in
  `errors.<field>`. The bootstrap SuperAdmin password `LeadKart!Dev2026` (from
  `scripts/devenv`) complies; so do the example passwords here.
- A newly registered tenant admin and every user created through `POST /users` are
  flagged `must_change_password`. Until the password is changed, **every route except
  `GET /api/v1/auth/me/capabilities` and `POST /api/v1/auth/change-password` answers
  `403 password_change_required`**. The login body carries `must_change_password: true`.
- A successful change **ends every session of that person**: the token you used is
  `401 stale_token` (with `WWW-Authenticate: Bearer`), refresh tokens are dead
  (`401 refresh_rejected`). Sign in again with the new password. `change-password`
  answers `204` with no body.

Real output of that sequence (a freshly registered owner):

```
login owner@acme.local / Acme-Owner-2026!
  {"access_token":"<jwt>","refresh_token":"<opaque>","access_token_expires_at":"2026-11-20T13:57:12Z","token_type":"Bearer","must_change_password":true}
GET /users with that token
  403  {"error":"password_change_required","message":"password change required before using this endpoint"}
GET /auth/me/capabilities  -> 200, "must_change_password":true
change-password with new_password "short"
  422  {"error":"password_policy_violation",...,"errors":{"new_password":["min_length","requires_uppercase","requires_digit","requires_symbol"]}}
change-password to Acme-Owner-2026!b
  204
GET /auth/me/capabilities with the OLD access token
  401  Www-Authenticate: Bearer realm="leadkart"  {"error":"stale_token","message":"session has been revoked"}
POST /auth/refresh with the OLD refresh token
  401  {"error":"refresh_rejected"}
login owner@acme.local / Acme-Owner-2026!b
  {"access_token":"<jwt>",...,"must_change_password":false}
```

## 5. Seed the three personas (copy-paste)

After bootstrap only the platform tenant and the SuperAdmin exist (the migrations seed the
reference data and the permission catalogue). `POST /api/v1/tenants` needs a platform
operator token (anonymous is `401`). Tenant roles are seeded when the tenant is
registered: the script uses **Sales Manager** (manager tier, rank 20) and **Sales
Executive** (user tier, rank 50). Needs only `curl` and `sed`.

```bash
B=http://localhost:8080/api/v1
J='Content-Type: application/json'
jget() { sed -n "s/.*\"$1\":\"\([^\"]*\)\".*/\1/p"; }
login() { curl -fsS -X POST $B/auth/login -H "$J" -d "{\"email\":\"$1\",\"password\":\"$2\",\"device_label\":\"dev\"}" | jget access_token; }
# first sign-in of a flagged account, then the forced change
rotate() { T=$(login "$1" "$2"); curl -fsS -o /dev/null -w "change-password %{http_code}\n" -X POST $B/auth/change-password -H "Authorization: Bearer $T" -H "$J" -d "{\"current_password\":\"$2\",\"new_password\":\"$3\"}"; }

# 1. the platform operator (seeded; must_change_password is false)
OP=$(login superadmin@leadkart.local 'LeadKart!Dev2026')

# 2. a tenant with an owner, and the owner's forced password change
curl -fsS -X POST $B/tenants -H "Authorization: Bearer $OP" -H "$J" -d '{"slug":"acme-pharma","legal_name":"Acme Pharma Pvt Ltd","display_name":"Acme Pharma","admin_email":"owner@acme.local","admin_password":"Acme-Owner-2026!","admin_first_name":"Asha","admin_last_name":"Owner"}'; echo
rotate owner@acme.local 'Acme-Owner-2026!' 'Acme-Owner-2026!b'
OWNER=$(login owner@acme.local 'Acme-Owner-2026!b')

# 3. a manager and an ordinary user created by the owner, roles assigned, own password changes
ROLES=$(curl -fsS $B/roles -H "Authorization: Bearer $OWNER")
MGR_ROLE=$(echo "$ROLES" | sed -n 's/.*"id":"\([^"]*\)","tenant_id":"[^"]*","name":"Sales Manager".*/\1/p')
USR_ROLE=$(echo "$ROLES" | sed -n 's/.*"id":"\([^"]*\)","tenant_id":"[^"]*","name":"Sales Executive".*/\1/p')
MGR=$(curl -fsS -X POST $B/users -H "Authorization: Bearer $OWNER" -H "$J" -d '{"email":"manager@acme.local","password":"Acme-Manager-2026!","first_name":"Mohan","last_name":"Manager"}' | jget membership_id)
USR=$(curl -fsS -X POST $B/users -H "Authorization: Bearer $OWNER" -H "$J" -d '{"email":"user@acme.local","password":"Acme-User-2026!","first_name":"Uma","last_name":"User"}' | jget membership_id)
curl -fsS -X POST $B/users/$MGR/roles -H "Authorization: Bearer $OWNER" -H "$J" -d "{\"role_id\":\"$MGR_ROLE\"}"
curl -fsS -X POST $B/users/$USR/roles -H "Authorization: Bearer $OWNER" -H "$J" -d "{\"role_id\":\"$USR_ROLE\"}"
curl -fsS -X PUT  $B/users/$USR/manager -H "Authorization: Bearer $OWNER" -H "$J" -d "{\"manager_id\":\"$MGR\"}"
rotate manager@acme.local 'Acme-Manager-2026!' 'Acme-Manager-2026!b'
rotate user@acme.local    'Acme-User-2026!'    'Acme-User-2026!b'

# 4. proof: the four accounts sign in with their final passwords and are unflagged
for p in "superadmin@leadkart.local:LeadKart!Dev2026" "owner@acme.local:Acme-Owner-2026!b" "manager@acme.local:Acme-Manager-2026!b" "user@acme.local:Acme-User-2026!b"; do
  T=$(login "${p%%:*}" "${p#*:}"); curl -fsS $B/auth/me/capabilities -H "Authorization: Bearer $T" | sed -n 's/.*"email":"\([^"]*\)".*"must_change_password":\([a-z]*\).*"rank":\([0-9]*\),"tier":"\([a-z_]*\)".*/\1 must_change_password=\2 rank=\3 tier=\4/p'
done
```

Output of the run (the role and user assignments print nothing, they answer `204`):

```
{"tenant_id":"01a1f936-ee1e-...","person_id":"01a1f936-ee20-...","membership_id":"01a1f936-ee21-..."}
change-password 204
change-password 204
change-password 204
superadmin@leadkart.local must_change_password=false rank=1 tier=admin
owner@acme.local must_change_password=false rank=0 tier=owner
manager@acme.local must_change_password=false rank=20 tier=manager
user@acme.local must_change_password=false rank=50 tier=user
```

The script is not idempotent (the tenant slug and the emails exist after one run): to run
it again, reset first (section 9). Through the API the manager can list users (`GET /users`
is `200`); the ordinary user gets `403` there.

**Who may add users (owner ruling 2026-11-21):** only the tenant owner (and an Administrator) can
`POST /users`; the manager and the ordinary user get `403` there, even though the manager can list
users. Create more accounts as `owner@acme.local` (as the script does), as the SuperAdmin through
`X-Tenant-Id`, or through an impersonation session. Nothing in this stack's database needs
repair: the personas above never held the permission (the seeded Sales Manager and Sales
Executive do not carry `identity.users.create`; had it been granted by hand, the migration would remove it), and the `migrate` step applies the cleaning
migration `20261121000001` on the next start, which finds nothing to change here.

**Personas for the web's real-backend smoke test (development-only credentials):**

| Persona | Email | Password (after the forced change) | Rank / tier |
|---|---|---|---|
| Platform operator | `superadmin@leadkart.local` | `LeadKart!Dev2026` | 1 / admin, `is_super_user` |
| Tenant owner (acme-pharma) | `owner@acme.local` | `Acme-Owner-2026!b` | 0 / owner |
| Manager (Sales Manager) | `manager@acme.local` | `Acme-Manager-2026!b` | 20 / manager |
| Ordinary user (Sales Executive, reports to the manager) | `user@acme.local` | `Acme-User-2026!b` | 50 / user |

The first password of each created account (`Acme-Owner-2026!`, `Acme-Manager-2026!`,
`Acme-User-2026!`) stops working at the change. `task test:smoke` (`scripts/smoke.sh`) also
works against this stack; it registers its own throwaway tenant each time.

## 6. Rate limits and the Argon2 gate during Playwright runs

Everything from one machine is one client address (the BFF sends the browser's address, and
local browsers are all `127.0.0.1` or `::1`), so parallel workers share every per-client budget.
The `.env` block in section 1 relaxes what can be relaxed, with values that pass validation
(rate: `PER_MINUTE` finite and above 0, `BURST` 1..1000; Argon2: `MAX_PER_CLIENT` 1..16,
`QUEUE_TIMEOUT` 100ms..30s):

| Setting | Default | Local value | Why |
|---|---|---|---|
| `LEADKART_RATE_LIMIT__LOGIN__PER_MINUTE` / `BURST` | 30 / 10 | 600 / 200 | every test signs in |
| `LEADKART_RATE_LIMIT__REFRESH__PER_MINUTE` / `BURST` | 120 / 30 | 1200 / 200 | silent refresh |
| `LEADKART_RATE_LIMIT__CHANGE_PASSWORD__PER_MINUTE` / `BURST` | 5 / 5 (per person) | 60 / 30 | the persona set-up rotates passwords |
| `LEADKART_ARGON2__MAX_PER_CLIENT` | 2 slots (+ up to 8 waiting) | 8 | parallel workers sign in at once |
| `LEADKART_ARGON2__QUEUE_TIMEOUT` | 2s | 10s | a waiting sign-in is served instead of `503 service_busy` |

Effect observed with these values: 40 consecutive logins from the host all answered `200`
(the defaults throttle after a burst of 10). Other per-route limits (password reset request,
reset, email-change confirm, tenant registration) keep their defaults; the same
`LEADKART_RATE_LIMIT__<ROUTE>__PER_MINUTE` / `__BURST` variables are passed through for
them. **The global per-client limiter (10 requests/s, burst 60) is fixed in `cmd/api` and
cannot be relaxed**: a test run that fans out more than that from one address gets `429`
with `Retry-After`; keep parallelism modest or pace it. After editing `docker/.env`, run
`task docker:up:full` again (it recreates the api container).

## 7. The worker

The dev compose file has no worker service (`docker/compose.prod.yml` does). Without a
worker the api works for requests and responses, but **nothing asynchronous happens**:

- the outbox is never drained, so no integration event reaches its subscribers: no emails
  (password reset, email-change confirmation), no notifications, no read-model updates
  and none of the event-driven cascades (cache invalidation and the refresh-family
  revocation backstop after a password change, initial lead credits for a new tenant,
  the order, stock and dispatch subscribers);
- River jobs do not run, including `identity.login_phantom_purge` (the phantom failed-login
  table then only grows; the api logs a WARN at boot when the outbox is stale and exposes
  `leadkart.outbox.oldest_unacked_age_seconds`).

Even with the worker, **no email is delivered**: it uses an in-memory recorder
(`email.NewRecorder`) and logs `password reset email sent` without the token, so a reset
flow cannot be completed locally.

Run it on the host (config `LoadWorker` needs the database, Redis and the JWT key, which it
also uses to key the phantom counter; it connects as the application role like the api):

```bash
go build -o tmp/worker.exe ./cmd/worker
set -a; . <(grep -E '^LEADKART_JWT__' docker/.env); set +a
export LEADKART_POSTGRES__DSN='postgres://leadkart_app:leadkart_app_dev_local_rotate_in_prod@localhost:5432/leadkart?sslmode=disable'
export LEADKART_REDIS__ADDR=localhost:6379
./tmp/worker.exe            # Ctrl-C to stop; admin probes on http://localhost:9091 (/ready)
```

Observed: it registers 32 subscribers and the River client, `GET :9091/ready` answers
`{"postgres":"ok","redis":"ok"}`, and a `POST /auth/request-password-reset` for
`user@acme.local` produced `password reset email sent ... to user@acme.local` in its log.
It also processed the earlier password-change events (`revoked families on security change`).
The worker connects as the application role on purpose: the owner role bypasses row-level
security. `go run ./cmd/worker` with the same environment works too.

## 8. Migrations, privileges, seeded data

- `migrate` runs `goose up` as the **owner** role (`leadkart`) and then applies River's own
  migrations and the application role's grants; the log ends
  `goose: successfully migrated database to version: 20261120000004` and
  `river migrations applied and application role granted`. River's tables (`river_job`,
  `river_migration`) and the identity tables of the latest rounds (for example
  `identity.login_phantom_failures`) are part of that run. Run it by hand with
  `docker compose --env-file docker/.env -f docker/compose.yml --profile full run --rm migrate up`.
- The api (and the worker) run as `leadkart_app`, a non-owner role, so row-level security
  is enforced as in production. At boot each checks that this role holds every privilege the
  application needs (`pg.VerifyApplicationPrivileges`): a database that is not migrated, or
  a role that lacks a grant, is a logged WARN in dev and a refusal to start in production.
- `bootstrap` (runs after `migrate`, idempotent) creates the platform tenant, the SuperAdmin
  person with the role and membership, from `LEADKART_SUPERADMIN__*` in `docker/.env`.
  The bootstrap CLI holds that password to the default policy and refuses a weak one.
  Nothing else is seeded: tenants, their roles and users come from section 5.

## 9. Stop, reset, teardown

```bash
task docker:down     # stop and remove the containers, KEEP the data (named volumes)
task docker:wipe     # also delete the Postgres and Redis volumes: a clean slate
```

After `docker:wipe` the next `task docker:up:full` migrates and bootstraps again and
section 5 can be re-run. Stop the worker with Ctrl-C. Ports 8080, 9090, 5432 and 6379 are
free again after `docker:down`.

## 10. Troubleshooting (failures hit while validating this runbook)

| Symptom | Cause | Fix |
|---|---|---|
| `migrate` exits 1: `goose up: error: found 2 missing migrations before current version ...` | an old `docker_leadkart_pg_data` volume migrated by an older branch whose version numbers do not match | `task docker:wipe`, then up again |
| api restarts forever: `config: placeholder secret detected - refusing to boot: Postgres.DSN contains a placeholder marker` | an older checkout whose dev role password contained the word "placeholder" | update, then `task docker:wipe` (the role password is set only when the volume is first created) |
| api cannot connect: `password authentication failed for user "leadkart_app"` | the volume was created before the role password changed | `task docker:wipe` |
| appended lines vanished from `docker/.env` / the JWT key changed on every `task docker:up*` | an older Taskfile regenerated the file on every run (its skip check broke on brace syntax) | current `Taskfile.yml`; re-append your lines once |
| a line in `docker/.env` has no effect on rate limits or the Argon2 gate | the compose file did not pass the variable to the container | current `docker/compose.yml` passes them all; after editing `.env` run `task docker:up:full` again |
| `docker exec leadkart-api sh` fails | the api image is distroless, no shell | use `docker logs leadkart-api` and `curl :9090/...` |
| every route answers `403 password_change_required` | the account is flagged | change the password (section 4) |
| `401 stale_token` right after a password change | by design: every session of that person ended | sign in again |
| `401 refresh_rejected` with `reason: session_expired` after the app sat unused | the idle timeout (`LEADKART_REFRESH__SLIDING_TTL`, default 2h; this runbook's `.env` block sets 24h) or the 14-day absolute lifetime ended the session | sign in again |
| login answers `403 tenant_suspended` (right password) | the person's company is suspended (`POST /tenants/{id}/suspend`); members cannot sign in or refresh until it is reactivated (`POST /tenants/{id}/activate`); a freshly registered company is `pending`, which signs in normally | reactivate the tenant as the SuperAdmin |
| login answers `429` | the per-route or the global (10/s, burst 60) limit | section 6 |
| login answers `503 service_busy` | the Argon2 gate: a wait ran out or a client's line of 8 was full | raise `MAX_PER_CLIENT` / `QUEUE_TIMEOUT` (section 6) |
| every user is throttled together | trusted proxies unset or not matching: all clients share the gateway address | section 2: check `trusted proxies in effect` in the api log and the WARN about an unusable header |
| `jq: command not found` | not installed on this machine | the commands here use `sed` only |
