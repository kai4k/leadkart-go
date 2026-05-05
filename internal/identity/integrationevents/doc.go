// Package integrationevents holds the framework-neutral wire-contract
// records for cross-module communication originating from Identity.
// Mirrors the .NET `LeadKart.Modules.Identity.IntegrationEvents`
// project + the "Unobtrusive Mode" canon
// (`docs.particular.net/nservicebus/messaging/unobtrusive-mode`).
//
// HARD RULES (CI-blocking architecture tests in arch_test.go):
//
//  1. ZERO framework imports. No Watermill, pgx, sqlc-generated, http,
//     redis, ristretto, or any other infrastructure dependency. Only
//     stdlib + uuid + the sibling domain packages are allowed (and
//     domain only because the V1 records embed primitive forms of the
//     same fields).
//  2. Every record satisfies EITHER [TenantScoped] OR [Platform] —
//     compile-time assertions per file enforce this; arch test
//     iterates [AllEvents] to catch missing assertions.
//  3. Every record's [Event.Topic] matches the regex
//     `^identity\.[a-z][a-z0-9_]*\.v\d+$` per `messaging.md`
//     "Event versioning".
//
// Versioning: breaking changes mint a NEW record (e.g. `…V2`) per
// `messaging.md`. The V1 record stays in tree until every in-flight
// outbox row drains; same-handler-handles-both-versions per Wolverine
// canon (Three Dots Labs Watermill docs "Message Versioning"
// alternative).
//
// Citations: NServiceBus "Unobtrusive Mode"; CloudEvents `type`;
// EventBridge `DetailType`; Stripe webhook `type`; Three Dots Labs
// `messaging.md` "Composition, not inheritance".
package integrationevents
