# Production deployment checklist

One page. Read it before every production deploy and keep it next to
`docker/compose.prod.yml` and `docker/.env.example`. It is a checklist, not a
runbook: Task 14's runbook will link here. Every item names the setting, why it
matters and how to check it.

## Before the deploy

1. **Deploy order: `migrate`, then the worker, then the api.** `compose.prod.yml`
   makes both `api` and `worker` wait for `migrate` (`depends_on: migrate:
   service_completed_successfully`); when deploying by hand, never start a new api or
   worker against an old schema, and start the worker before the api so the first rows
   the new api writes to the outbox (stamp evictions, family revocations, the tenant
   suspension cascade) find a consumer. All three images (`API_IMAGE`, `WORKER_IMAGE`,
   `MIGRATE_IMAGE`) must carry the SAME release tag: `.github/workflows/release.yml`
   builds, signs and publishes the three together, and
   `TestArch_ProdComposeImagesAreReleased` fails when the compose file pulls an image
   the release does not build.

   *May the OLD api keep running while `migrate` runs?* For migrations 20261120000004,
   `...05` and `...06` yes: they only add a table and columns with defaults
   (`persons.password_reset_failed_attempts integer NOT NULL DEFAULT 0`,
   `refresh_tokens.grace_replays smallint NOT NULL DEFAULT 0`,
   `refresh_tokens.consumed_client_binding text NULL`), which the old statements never
   name, so old inserts and updates keep working (read from the SQL; not run against an
   old binary). NOT for this release as a whole: `20261119000001_identity_role_tiers`
   rewrites role levels and permission sets of existing tenants under the old api, and
   `20261120000002` repairs self-managed memberships and adds a CHECK. Treat the old api as unsafe once
   `migrate` has started: stop it first (a short outage) or accept that it serves stale
   role data until it is replaced. Each of 05 and 06 sets `lock_timeout = 5s`: if a busy
   table makes `migrate` fail with a lock timeout, nothing was applied from that file,
   re-run it in a quieter minute.
2. **`LEADKART_ENV=production`** on api and worker. The api then refuses to start on
   a missing trusted-proxy decision, a missing lockout key, a weak or placeholder
   secret, or a database role that is an owner or a superuser. The privilege check
   itself (`pg.VerifyApplicationPrivileges`) runs for the api AND the worker in EVERY
   environment, not only production: a role that LACKS a privilege stops both at boot
   everywhere; only "the role is an owner / superuser / BYPASSRLS" is an error in
   production and a logged warning elsewhere. See "The application role" below.
3. **`LEADKART_HTTP__TRUSTED_PROXIES`** = the BFF's fixed address, or a dedicated
   subnet only the BFF lives in, or `none` when the api is directly exposed. **Never
   `172.16.0.0/12`**: that range trusts every Docker network on the host and anything
   that reaches the published port, and a client inside a trusted range chooses its
   own rate-limit bucket by sending `X-Forwarded-For`.
4. **`LEADKART_IDENTITY__LOCKOUT_KEY`** = `openssl rand -base64 32`, different from
   the JWT key. Required in production. Rotating it restarts the phantom failed-login
   counters and opens a short 423-versus-401 window, so rotate deliberately.
5. **The worker is deployed and healthy.** It is part of `compose.prod.yml`
   (`worker`, image `WORKER_IMAGE` = `ghcr.io/<org>/leadkart-go-worker:<tag>`, health
   check `/worker -healthcheck`). Without it: no outbox row is delivered (stamp-cache
   and family-revocation backstops, cascades, audit, email, **the tenant-suspension
   revocation of live sessions**) and the phantom failed-login table is never purged.
   Its admin listener (`/alive`, probed by the health check) is
   **`LEADKART_LISTEN__WORKER_ADMIN`** (default `:9091`; the compose file and the
   image set it): it is distinct from the api's `LEADKART_LISTEN__ADMIN` (`:9090`),
   must not be published outside the host, and the probe reads the same variable, so
   changing one without the other makes the container unhealthy.
6. **`GOMEMLIMIT`** (900MiB for the 1 GiB api container; set in `compose.prod.yml`).
   Raise `LEADKART_ARGON2__MAX_CONCURRENT` only together with the container limit and
   `GOMEMLIMIT` (16 slots hold about 312 MiB of live Argon2 memory, and the collector
   needs headroom).
7. **On the BFF** (leadkart-web): `ADDRESS_HEADER` and `XFF_DEPTH` must equal, and
   never exceed, the number of proxies in front of the BFF. A larger depth lets a
   client choose the address the api sees. Also on the BFF: `ORIGIN` and
   `PUBLIC_SITE_URL` must both be set to the public `https://` origin (no path, no
   trailing slash). With `ORIGIN` unset the BFF falls back to the request's `Host`
   header; with `PUBLIC_SITE_URL` unset link previews carry no absolute image.

8. **Rotating the JWT signing key signs out some people: do it off-peak.** The
    refresh grace window (item below) derives its successor token from the signing key;
    after a rotation a token that was consumed less than the grace window ago and is
    presented again (a lost response, a second tab) no longer matches, is treated as
    reuse, and its session is revoked: that person signs in again. Everything else
    carries over (`LEADKART_JWT__PREVIOUS_KEYS` keeps old access tokens valid).
9. **`LEADKART_REFRESH__GRACE_WINDOW`** (default 30s, at most 120s, 0 = strict
    rotation). A just-rotated refresh token may be presented again for this long, and
    only by the SAME client that rotated it (the client address the rate limiters use:
    it needs item 3 and item 7 right; with an untrusted or missing `X-Forwarded-For`
    every user shares one client key and the binding proves nothing), at most 3 times.
    A client whose address changes inside the window between a lost response and its
    retry (a phone switching networks) is treated as reuse and signed out: that is the
    price of the binding.

## The application role

`leadkart_app` is the runtime role of the api and the worker (non-owner, row-level
security applies to it). Migration `20260606000001_application_role_grants` GRANTs it
its privileges, but it is a **no-op when the role does not exist yet**: goose then
records it as applied and **will not run it again**, and `migrate up` re-applies only
River's own grants. So on a managed Postgres, or any database that was migrated before
the role was created, the api and the worker refuse to boot with
`database role "leadkart_app" lacks ...`.

*Prevent it.* As the database owner (a superuser or the provisioning role), before the
FIRST `migrate up`:

```sql
CREATE ROLE leadkart_app WITH LOGIN NOSUPERUSER NOINHERIT NOCREATEROLE NOCREATEDB PASSWORD '<from the secrets store>';
```

and run **every** migration as the same owner role (`POSTGRES_OWNER`): default privileges
bind to the role that ran the migration, so tables created later by a different owner
would not be granted to `leadkart_app`.

*Repair it* (the role was created late, or two owners migrated). As the owner role that
ran the migrations, apply the Up block of the grants migration by hand, and of each
later module grants migration (a module added after it carries its own, today
`20261123000004_companysettings_grants`), then run `migrate up` once more for
River's grants. Each block is idempotent:

```bash
for f in migrations/20260606000001_application_role_grants.sql migrations/20261123000004_companysettings_grants.sql; do
  sed -n '/^-- +goose Up/,/^-- +goose Down/p' "$f" | psql -v ON_ERROR_STOP=1 "$OWNER_DATABASE_URL"
done
```

(the goose markers are SQL comments, so `psql` reads the block as is; the migrations
directory is in the repository at the release tag, the `migrate` image has no shell or
`psql`). Re-run `docker compose ... up -d api worker`: the boot check passes.

## After the deploy

10. **The request log's `client_ip` shows public addresses**, not the BFF's or a Docker
   address. If every line shows one private address, items 3 and 7 are wrong and all
   users share one rate-limit bucket per credential route.
11. `GET /health` on the admin listener (`:9090`) and the worker's `/alive` (`:9091`)
   answer 200; `docker compose ps` shows api and worker healthy.
12. A test login and a test change-password succeed; the old access token then answers
    401 `stale_token` at once (stamp eviction, ADR 0068 D12). If the counter
    `leadkart.identity.stamp_evict_failures` rises, the cache is unreachable and old
    tokens live up to 30 s.
13. **Verify a suspended company cannot sign in.** With a throwaway tenant and member:
    suspend the tenant (`POST /api/v1/tenants/{id}/suspend`); the member's login with the
    right password answers 403 `tenant_suspended` (a wrong password: 401
    `invalid_credentials`), a refresh of an older session answers 401 `refresh_rejected`
    with reason `tenant_suspended`; reactivate (`POST /api/v1/tenants/{id}/activate`) and
    the login works again. Access tokens already issued live up to 600 s.
    (Only an active company's members sign in, owner ruling 2026-11-21. A company is never
    deleted: it is pending, active or suspended, ADR 0071.)
    Registering a tenant creates it ACTIVE, so its first admin signs in at once.
14. **Verify a suspended person cannot sign in.** With a throwaway account: global-suspend
    it (`POST /api/v1/platform/persons/{id}/global-suspend`), then log in with the right
    password (expect 401 `invalid_credentials`, the same answer as a wrong password),
    refresh an older session (401), and lift the suspension (the login works again).

## Residual risks accepted

These are known, plain-language limits of this release. None is a bug to fix before the deploy; each is a conscious acceptance (several are pending the owner, ADR 0069).

- **A misconfigured proxy puts everyone in one rate-limit bucket.** Per-client limits, the failed-login lockout and the refresh grace binding all key on the client address the api derives from `X-Forwarded-For`. If `LEADKART_HTTP__TRUSTED_PROXIES` or the BFF's `XFF_DEPTH` is wrong, every user shares one key: the limits throttle everybody together and the grace binding proves nothing (items 3, 7, 10).
- **Grace-window takeover from behind the victim's NAT.** The refresh grace replay (30 s, at most 3 times) is bound to the client ADDRESS, so someone who holds a just-rotated token AND sits behind the same public address as its owner (one office, one carrier NAT) can still replay it inside the window.
- **Sessions last up to 14 days** from login (the absolute lifetime), refreshed or not; the idle timeout (24 h, the owner's ruling of 2026-11-21) ends a session unused for that long sooner.
- **On deploy, sessions older than 14 days end and flagged accounts must change their password.** The absolute lifetime counts from each family's creation, so every family created more than 14 days ago is refused at its next refresh (`session_expired`), and every person still flagged `must_change_password` is forced to change it at the next sign-in (owner decision (a) in ADR 0069).
- **On deploy, every `pending` company becomes `active`.** Only an active company's members sign in (the owner's ruling of 2026-11-21), and registration created every company `pending` until now, so migration `20261121000002_identity_pending_tenants_active` activates them (one audit row each; `activated_at` is the deploy time). Without it every company registered before the deploy would be locked out. A company registered after the deploy is created active. A `suspended` company is left as it is: its members cannot sign in until an operator reactivates it.
- **Session values of this release** (owner rulings of 2026-11-21): idle timeout 24 h (`LEADKART_REFRESH__SLIDING_TTL`, was 2 h), absolute lifetime 14 days (`LEADKART_REFRESH__ABSOLUTE_TTL`), default password policy 8 characters with upper, lower, digit and symbol (was 12; tenants that stored their own policy keep it).
- **A security stamp can be 30 s stale.** An old access token dies at once when Redis is reachable (the api evicts the cached stamp after the commit); with the cache unreachable it lives up to 30 s.
- **Rotating the JWT signing key signs some people out** (item 8: the grace successor derives from it), and access tokens issued before a **tenant suspension live up to 600 s**.
- **Someone who holds a CONSUMED refresh token can force a sign-out.** Presenting a consumed token revokes the whole family (reuse detection): a stolen old token can end the owner's session. That is the price of detecting theft.
- **Residual timing differences between login branches** (a right password is distinguishable by design; one branch writes an extra row) are below database jitter next to the Argon2 time (ADR 0068 D12).
- **Impersonation sessions live in one process's memory.** A restart of the api ends every session; with more than one api instance a session is only known to the instance that created it (ADR 0045).

## Alerts to configure

| Signal | Meaning | Alert when |
|---|---|---|
| `leadkart.http.trusted_proxy_header_unusable` (counter) | a trusted proxy sent no usable `X-Forwarded-For`: its users share a bucket | rate > 0 for 5 minutes |
| `leadkart.outbox.oldest_unacked_age_seconds` (gauge) | age of the oldest outbox row the worker has not acknowledged | above 300 for 5 minutes (a dead or wedged worker; the api also logs a WARN at boot) |
| `leadkart.identity.login_phantom_failures.rows` (gauge) | rows in the phantom failed-login table | still growing for 30 minutes, or above 100,000 (the purge job is not running) |
| River job failures (`river_job` with state `discarded`, or the worker's job error log) | background jobs failing, `identity.login_phantom_purge` included | any discarded job |
| `service_busy` 503 rate on the password routes | the Argon2 gate is saturated, or a client's wait ran out or its line of 8 was full | sustained above 1% of login requests |

## Configuration reference (password work)

| Variable | Default | Range |
|---|---|---|
| `LEADKART_ARGON2__MAX_CONCURRENT` | 16 | 1..64 |
| `LEADKART_ARGON2__QUEUE_TIMEOUT` | 2s | 100ms..30s |
| `LEADKART_ARGON2__MAX_PER_CLIENT` | 2 (slots one client holds; up to 8 more of its operations wait, a fixed bound in code) | 1..MAX_CONCURRENT |

**Customers behind one NAT address (an office, a carrier-grade NAT).** The rate limits and the password-work gate count per client ADDRESS (IPv6 per /64). One address runs at most 2 password operations at once and queues 8 more for up to the 2 s queue timeout; with Argon2id at about 30 to 50 ms per verify that is roughly 40 to 60 logins per second for the whole address before a 503, which is ample for an office of a few hundred people signing in over a minute but not for several hundred signing in within seconds (a shift start). The per-route limits are also per address (login 30 per minute, burst 10; refresh 120 per minute, burst 30; reset-password 10 per minute): a NAT-heavy customer needs `LEADKART_RATE_LIMIT__LOGIN__PER_MINUTE` / `BURST` raised, and the per-client ceiling `LEADKART_ARGON2__MAX_PER_CLIENT` is the other knob. These numbers are defaults pending the owner (ADR 0069 items 34 and 50).
| `LEADKART_ARGON2__RESERVED_AUTHENTICATED` | 4 | 0..MAX_CONCURRENT-1 |
| `LEADKART_IDENTITY__LOCKOUT_KEY` | none (required in production) | base64, at least 32 bytes |
| `LEADKART_REFRESH__GRACE_WINDOW` | 30s | 0 (strict rotation) .. 120s, below `SLIDING_TTL` |
| `LEADKART_REFRESH__ABSOLUTE_TTL` | 336h (14 days; a session ends this long after its login) | above `SLIDING_TTL` |
| `LEADKART_REFRESH__SLIDING_TTL` | 24h: the IDLE timeout (a session unused for this long ends, 401 `session_expired`; a rotation renews it, never past `ABSOLUTE_TTL`). The owner's ruling of 2026-11-21 (it was 2h) | above `GRACE_WINDOW`, below `ABSOLUTE_TTL` |
| `GOMEMLIMIT` | 900MiB (compose) | Go runtime setting |
