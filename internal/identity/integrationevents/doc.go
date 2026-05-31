// Package integrationevents holds the framework-neutral wire-contract
// records for cross-module communication originating from Identity.
// Follows NServiceBus "Unobtrusive Mode" canon.
//
// CI-blocking rules (arch_test.go):
//
//  1. Zero framework imports — stdlib + uuid + sibling domain packages only.
//  2. Every record satisfies [TenantScoped] OR [Platform]; compile-time
//     assertions per file, arch test iterates the catalogue.
//  3. Every [Event.Topic] matches `^identity\.[a-z][a-z0-9_]*\.v\d+$`
//     per messaging.md "Event versioning".
//
// Breaking changes mint a new record (e.g. V2); the V1 record stays until
// every in-flight outbox row drains (Three Dots Labs "Message Versioning").
package integrationevents
