# ADR 0057 — Email decoupling via outbox-driven subscribers

**Status:** Accepted
**Date:** 2026-05-23

## Context

Two command handlers were holding a synchronous `email.Gateway.Send` call inside their request path:

| Handler | Synchronous side-effect |
|---|---|
| `RequestPasswordResetHandler` | Persists pending reset + sends "reset your password" email to the recipient. |
| `RequestEmailChangeHandler`   | Persists pending change + sends "confirm your new email" link to the NEW address. |

The synchronous send couples the HTTP 200 to the SMTP/SES round-trip:

- A flaky provider (300ms+ latency, transient 5xx) inflates the request P99.
- Provider outage = request 500 = client retry = double-write of the pending reset row (idempotent at the aggregate level, but wasted I/O + a misleading user-facing error).
- The command handler picks up `email.Gateway` as a dependency for a reason that has nothing to do with the command's domain semantics — fan-out bloat in the composition root.

LeadKart's modular-monolith canon (ADR 0008, TDL EDA training) is unambiguous: cross-cutting effects ride the **outbox**, not the request path. The existing Identity subscribers (`InvalidateSecurityStampCache`, `RevokeFamiliesOnSecurityChange`, `ReuseDetectedSIEM`) already follow this pattern — emit an integration event, let an async subscriber act on it. Email delivery is structurally identical:

- Single-consumer action (one subscriber, one provider call).
- Recoverable on Watermill retry (transient SMTP / provider 5xx).
- Belongs to a Notifications-shaped concern that will eventually graduate to its own bounded context in Phase 4+ — moving it to a subscriber today makes that future cut a composition-root change, not a refactor.

### Why now (Wave 9.2d)?

The PR-B punt during the Wave-9 closeout. CLAUDE.md flagged that the welcome-email-on-tenant-register + on-create-user work was deliberately deferred (no SMTP provider integration shipped in v0.2). The user's direction: **defer SMTP, but keep the architecture ready**. The right shape for that is: ship the EDA refactor NOW, keep the v0.2 wire-up as `email.Recorder`-backed, and let v0.3 swap in a real provider as a one-line composition-root change.

### Choice space

| Option | Trade-off |
|---|---|
| A. Two integration events per flow: AUDIT (no plaintext) + ACTION (plaintext) | Cleanest. Two consumers, two payload shapes. Plaintext only flows to the one subscriber that needs it. |
| B. One event carrying plaintext for all consumers | Pollutes the audit event with security-sensitive plaintext; SIEM subscribers see plaintext they don't need. Violates least-privilege. |
| C. River job per send | Parallel infra. We already have the outbox + Watermill subscribers. Adds operational surface for no semantic benefit. |
| D. Domain event carrying plaintext, ZERO new integration event (handler reads aggregate events + calls gateway inline) | Synchronous coupling persists — defeats the point. |

We pick **A** — paired events, single-consumer action event. Same shape Stripe uses for OTP delivery (the issued-OTP event is internal-only with the plaintext; the user-action audit event has no plaintext).

### Plaintext-in-outbox security analysis

The action event payload carries the plaintext reset / confirmation token. The boundary:

- **Tables involved.** `identity.outbox` is RLS+FORCE + tenant-isolated; the platform-scoped Person events use `tenant_id = uuid.Nil` + run under TxScopePlatform.
- **At-rest window.** The OutboxForwarder polls at 1s idle / 50ms busy intervals (`forwarderPollInterval` / `forwarderRetryInterval` in cmd/worker). Plaintext lifetime on disk is ≤1s typical, bounded by token TTL (1h password reset, 1h email change).
- **Backup blast radius.** Postgres backups contain the row for the retention window. Mitigation: the token is single-use + hash-locked at the Person aggregate; even a leaked plaintext requires concurrent control of the email address to escalate. Same attack surface as a backed-up `Authorization: Bearer` log line.
- **Stripe / Auth0 canon.** Both ship short-lived OTP via async outbox-shaped queues for the same reasons we do. The acceptable-window argument is well-established.

The alternative — encrypting the plaintext at the outbox row + decrypting in the subscriber — adds KMS dependency + key rotation overhead for a ≤1s window. Defer to Phase 6+ if a compliance audit demands it.

## Decision

### 1. Two new V1 integration events (per flow)

```go
// internal/identity/integrationevents/person.go
type PersonPasswordResetEmailRequestedV1 struct {
    platformMarker
    PersonID       uuid.UUID
    Email          string
    PlaintextToken string
    ExpiresAtUTC   time.Time
    RecipientName  string
    OccurredAtUTC  time.Time
}

type PersonEmailChangeConfirmationRequestedV1 struct {
    platformMarker
    PersonID       uuid.UUID
    NewEmail       string
    OldEmail       string
    PlaintextToken string
    ExpiresAtUTC   time.Time
    RecipientName  string
    OccurredAtUTC  time.Time
}
```

Platform-scoped (Person is global). Topics: `identity.person_password_reset_email_requested.v1` and `identity.person_email_change_confirmation_requested.v1`. Both register through the existing `register()` catalogue + the arch test gates them.

### 2. Two new domain events (per flow)

`PasswordResetEmailRequestedEvent` + `EmailChangeConfirmationRequestedEvent` in `internal/identity/domain/person/events.go`. Mapped to the V1 events via the existing `integrationevents/mapping.go` switch.

### 3. Aggregate method signatures gain `plaintextToken`

```go
func (p *Person) RequestPasswordReset(plaintextToken string, tokenHash PasswordResetTokenHash, ttl time.Duration) error
func (p *Person) RequestEmailChange(newEmail email.Address, plaintextToken string, tokenHash EmailChangeTokenHash, ttl time.Duration) error
```

The aggregate emits BOTH events. The `plaintextToken == ""` path emits only the audit event (admin hotwire / future no-email pathway).

The plaintext NEVER hits the row state — it flows through the transient event buffer + the outbox payload exactly once.

### 4. Command handlers shed `email.Gateway`

```go
func NewRequestPasswordResetHandler(persons person.Repository) RequestPasswordResetHandler
func NewRequestEmailChangeHandler(persons person.Repository) RequestEmailChangeHandler
```

Three constructor args → one. The composition root in `cmd/api/main.go` drops the `emailGateway := email.NewRecorder(now)` + `noReplyAddress` lines; both handlers are now pure persistence orchestrators.

### 5. New subscriber — `internal/identity/ports/subscribers/email_sender.go`

```go
type EmailSender struct {
    gateway email.Gateway
    from    email.Address
    appURL  string
    log     *slog.Logger
}

func (h *EmailSender) HandlePasswordResetEmail(ctx, _ string, msg *message.Message) error
func (h *EmailSender) HandleEmailChangeConfirmation(ctx, _ string, msg *message.Message) error
```

Same shape as the existing `InvalidateSecurityStampCache` subscriber. Decodes the V1 event payload, builds the `email.Message`, dispatches via the gateway. Returns the gateway error so Watermill retries on transient failure (must-succeed semantics — same as `RevokeFamiliesOnSecurityChange`).

Two handler-name constants (`HandlerSendPasswordResetEmail` + `HandlerSendEmailChangeConfirmation`) drive idempotency-key scoping. Subscribers ride the shared `integrationevents.Topic` + filter by `event_type` header (the established pattern).

### 6. Wired in cmd/worker, NOT cmd/api

`subscribers.Register(...)` signature gains `emailSender *EmailSender`. `cmd/worker/main.go` constructs the sender with `email.Recorder` + `no-reply@leadkart.local` + `defaultEmailLinkBaseURL = "https://app.leadkart.example"`. `cmd/api/main.go` no longer holds an email gateway.

`subscribers.Register` accepts `nil` for `emailSender` to keep existing tests (which don't exercise the email path) terse — the SIEM + revoke + invalidate-cache subscribers wire unconditionally.

### 7. v0.2 → v0.3 provider-swap migration path

```go
// v0.2 (current) — cmd/worker/main.go
emailGateway := email.NewRecorder(time.Now)

// v0.3 — same line, different impl behind email.Gateway
emailGateway := emailses.New(cfg.AWS.SESRegion, ...)  // or msg91, postmark, etc.
```

No domain, app, or subscriber code changes. The Strategy pattern keyed on provider (per `internal/common/email/gateway.go` package docstring) is the seam.

## Consequences

### Positive

- **Request path decoupled from SMTP.** The HTTP 200 fires immediately after the aggregate persist; the email goes out async.
- **Provider failure no longer = command failure.** Watermill retries the subscriber; the user sees their original 200 + a delayed email.
- **Composition root simplification.** `cmd/api/main.go` no longer wires an email gateway. The dependency lives where it belongs — at the actual sender.
- **Plaintext-in-payload bounded.** Outbox is RLS+FORCE; window is ≤1s typical; matches Stripe / Auth0 canon.
- **EDA shape stays consistent with the existing subscribers.** No new infrastructure (no River dependency, no separate broker). Same `messaging.Router` + `messaging.AddSubscriber` plumbing.
- **Welcome-email-on-register is now a 5-line follow-up.** Tenant register / user create emit a new `*WelcomeEmailRequestedV1` event; the same subscriber pattern dispatches.

### Negative

- **Plaintext briefly at-rest.** ≤1s typical, ≤1h bound. Accepted per the security analysis above.
- **Domain method signatures changed.** `RequestPasswordReset` and `RequestEmailChange` gained a `plaintextToken` parameter. Six test call sites updated (mechanical refactor).
- **Two events per flow.** Slightly more catalogue surface; the V1 alias gate catches naming drift.

### Why NOT river jobs

River is for **scheduled or fire-and-forget jobs** (the existing `AuditLogPurgeJob` runs daily). Email-on-event is event-driven, not schedule-driven — Watermill subscribers are the right primitive. Adding river for cross-cutting events would mean two queue systems coexisting; the operational cost (two backoff systems, two retry policies, two dashboards) buys nothing semantic.

### Deferred follow-ups

- **SMTP / SES / Msg91 provider integration.** Composition-root change only (v0.3+).
- **HTML body templates.** v0.2 is plaintext-only per the existing `email.Message` shape. HTML alternative ships via `WithHTMLBody` option when templates are designed.
- **Tenant-branded sender + reply-to.** `WithTenantID` option already exists on `email.Message`; subscribers wire it when the per-tenant branding API materialises.
- **Inform-the-OLD-address email after confirmation.** `EmailChangeConfirmationRequestedEvent` already carries `OldEmail`; a second handler on the same topic could deliver this when product priorities allow.
- **SMS via the same shape.** Different gateway interface (`sms.Gateway`); identical event-driven shape.

## Sources

- ADR 0008 — Messaging via Watermill + outbox.
- ADR 0027 — Outbox table doubles as audit log (the existing infra this rides).
- ADR 0042 — Cache TTL strategy (the analogous "EDA subscriber pattern" reference).
- ThreeDotsLabs Watermill canon — "outbox pattern over distributed transactions".
- Brandur Leach — "Implementing Stripe-like Idempotency Keys in Postgres" (similar EDA shape for cross-cutting effects).
- Vladimir Khorikov §10 — Application services as the boundary between command + subscriber.
- Stripe / Auth0 OTP delivery — at-rest plaintext window canon for short-lived tokens.
- LeadKart `.NET .claude/rules/messaging.md` — "Cascading messages > IMessageBus injection" (the .NET parent's analogous pattern).
