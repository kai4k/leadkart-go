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

## After the deploy

8. **The request log's `client_ip` shows public addresses**, not the BFF's or a Docker
   address. If every line shows one private address, items 3 and 7 are wrong and all
   users share one rate-limit bucket per credential route.
9. `GET /health` on the admin listener (`:9090`) and the worker's `/alive` (`:9091`)
   answer 200; `docker compose ps` shows api and worker healthy.
10. A test login and a test change-password succeed; the old access token then answers
    401 `stale_token` at once (stamp eviction, ADR 0068 D12).

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
| `LEADKART_ARGON2__RESERVED_AUTHENTICATED` | 4 | 0..MAX_CONCURRENT-1 |
| `LEADKART_IDENTITY__LOCKOUT_KEY` | none (required in production) | base64, at least 32 bytes |
| `GOMEMLIMIT` | 900MiB (compose) | Go runtime setting |
