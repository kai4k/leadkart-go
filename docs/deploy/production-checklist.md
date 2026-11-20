# Production deployment checklist

One page. Read it before every production deploy and keep it next to
`docker/compose.prod.yml` and `docker/.env.example`. It is a checklist, not a
runbook: Task 14's runbook will link here. Every item names the setting, why it
matters and how to check it.

## Before the deploy

1. **Run `migrate` before the api and the worker.** `compose.prod.yml` orders it
   (`depends_on: migrate: service_completed_successfully`); when deploying by hand,
   never start a new api against an old schema.
2. **`LEADKART_ENV=production`** on api and worker. The api then refuses to start on
   a missing trusted-proxy decision, a missing lockout key, a weak or placeholder
   secret, or a database role that is an owner or lacks a privilege.
3. **`LEADKART_HTTP__TRUSTED_PROXIES`** = the BFF's fixed address, or a dedicated
   subnet only the BFF lives in, or `none` when the api is directly exposed. **Never
   `172.16.0.0/12`**: that range trusts every Docker network on the host and anything
   that reaches the published port, and a client inside a trusted range chooses its
   own rate-limit bucket by sending `X-Forwarded-For`.
4. **`LEADKART_IDENTITY__LOCKOUT_KEY`** = `openssl rand -base64 32`, different from
   the JWT key. Required in production. Rotating it restarts the phantom failed-login
   counters and opens a short 423-versus-401 window, so rotate deliberately.
5. **The worker is deployed and healthy.** It is part of `compose.prod.yml`
   (`worker`, health check `/worker -healthcheck`). Without it: no outbox row is
   delivered (stamp-cache and family-revocation backstops, cascades, audit, email) and
   the phantom failed-login table is never purged.
6. **`GOMEMLIMIT`** (900MiB for the 1 GiB api container; set in `compose.prod.yml`).
   Raise `LEADKART_ARGON2__MAX_CONCURRENT` only together with the container limit and
   `GOMEMLIMIT` (16 slots hold about 312 MiB of live Argon2 memory, and the collector
   needs headroom).
7. **On the BFF** (leadkart-web): `ADDRESS_HEADER` and `XFF_DEPTH` must equal, and
   never exceed, the number of proxies in front of the BFF. A larger depth lets a
   client choose the address the api sees.

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
ran the migrations, apply the Up block of the grants migration by hand, then run
`migrate up` once more for River's grants. The block is idempotent:

```bash
sed -n '/^-- +goose Up/,/^-- +goose Down/p' migrations/20260606000001_application_role_grants.sql   | psql -v ON_ERROR_STOP=1 "$OWNER_DATABASE_URL"
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
14. **Verify a suspended person cannot sign in.** With a throwaway account: global-suspend
    it (`POST /api/v1/platform/persons/{id}/global-suspend`), then log in with the right
    password (expect 401 `invalid_credentials`, the same answer as a wrong password),
    refresh an older session (401), and lift the suspension (the login works again).

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
| `LEADKART_REFRESH__SLIDING_TTL` | 2h: the IDLE timeout (a session not refreshed for this long ends, 401 `session_expired`; a rotation renews it, never past `ABSOLUTE_TTL`). Default pending the owner's confirmation (ADR 0069) | above `GRACE_WINDOW`, below `ABSOLUTE_TTL` |
| `GOMEMLIMIT` | 900MiB (compose) | Go runtime setting |
