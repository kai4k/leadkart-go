// Package subscribers holds Platform-side Watermill subscriber handlers.
//
// Per ADR 0001 modular-monolith canon + ADR 0008 outbox-Watermill
// canon: cross-module communication is via integration events on the
// bus, NEVER direct package imports of another module's domain / app /
// ports / adapters. The publisher's `integrationevents` package is the
// ONE exception (framework-neutral wire records the publisher
// considers public). This package imports
// `internal/identity/integrationevents` for the TenantRegisteredV1
// payload shape exactly because of that carve-out.
//
// Subscribers in this package:
//
//   - [TenantRegisteredIngestor] — handles
//     [identity.TenantRegisteredV1] by creating a zero-balance
//     LeadCredit row via [command.InitialiseLeadCreditsHandler].
//     Closes BRD §6.2 "Consumed: TenantRegistered → initialise lead
//     credits".
//
// Failure modes follow the Watermill canon documented on the CRM-side
// [crm/ports/subscribers] package: JSON decode failure + command
// failure → return error → router retries with exponential backoff,
// dead-letters on exhaustion; topic-mismatch on the shared
// `identity.events` topic short-circuits silently.
//
// IDEMPOTENCY: every subscriber here is idempotent. Either the
// router-level [messaging.IdempotentReceiver] (envelope-ID dedup) or a
// natural-key precheck inside the command (e.g.
// [leadcredit.Repository.GetByTenant] returning a non-ErrNotFound
// result) short-circuits replays to a no-op ACK.
package subscribers
