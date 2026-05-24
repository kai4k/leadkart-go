// Package architecture is the LeadKart-Go architectural fitness-function
// suite — a CI-time gate that converts the project's ADR catalog into
// mechanically-enforced tests.
//
// # Why a separate package
//
// Per Neal Ford + Rebecca Parsons + Patrick Kua, "Building Evolutionary
// Architectures" (O'Reilly 2017; 2nd ed. 2023): the "fitness function"
// is the mechanism that prevents architectural drift over time — every
// architectural characteristic (modularity, layer purity, security
// boundary, performance budget) gets a test that fails when the
// characteristic is violated. Without fitness functions, ADRs rot into
// aspirational documentation that the codebase silently contradicts.
//
// This package holds the fitness functions that span MULTIPLE modules
// (e.g. "no cross-module imports between identity + inventory") or
// that gate global invariants (e.g. "every accepted ADR has a fitness
// function reference"). Per-module arch tests still live alongside
// their modules (e.g. internal/identity/integrationevents/arch_test.go,
// internal/identity/ports/route_registration_test.go) so the per-module
// authors can iterate on local discipline without merge friction here.
//
// # ADR catalog enforced
//
// EDA discipline (5 tests):
//   - ADR 0001 (modular monolith) → TestArch_NoCrossModuleImports
//   - ADR 0008 (Watermill messaging) → TestArch_SubscribersInPortsSubscribers
//   - ADR 0001/0006 (module-owned schemas) → TestArch_NoCrossSchemaJoins
//   - ADR 0027 (outbox-doubles-as-audit) → TestArch_OutboxTableSchema
//   - ADR 0002 (DDD sealed events) → TestArch_DomainEventsSealed
//
// TDL Clean Architecture (5 tests):
//   - ADR 0002/0047 (domain purity) → TestArch_DomainHasNoInfraImports
//   - ADR 0004 (TDL aggregate factory + UnmarshalFromDB) → TestArch_AggregatesHaveFactoryAndUnmarshal
//   - ADR 0004 (UpdateByID closure pattern) → TestArch_RepositoriesHaveUpdateByIDFn
//   - ADR 0047 (Cheney "accept interfaces") → TestArch_PortsAdaptersDontDefineInterfaces
//   - ADR 0047 (one-way dependency flow) → TestArch_AppDoesntImportPorts
//
// Modern Go (5 tests):
//   - ADR 0034 (Go 1.26+) → TestArch_NoInterfaceEmpty
//   - ADR 0013 (log/slog stdlib) → TestArch_NoLogPackage
//   - CLAUDE.md "Banned" list → TestArch_NoBannedDeps
//   - CLAUDE.md ctor-patterns → TestArch_NoMustInRequestPath
//   - Stripe money canon → TestArch_NoFloat64ForMoney
//
// Clock-injection invariants (3 tests, protects commit a33e9a0):
//   - TestArch_NoClockPackageReference
//   - TestArch_NoTimeNowInDomain
//   - TestArch_HandlersInjectNow
//
// Meta (1 test):
//   - TestMeta_EveryAcceptedADRHasFitnessFunctionRef
//
// # Process discipline
//
// New ADRs MUST land with either:
//
//  1. A `## Fitness function` section naming a TestArch_* test, OR
//  2. The marker `**Fitness function:** convention-only — not
//     mechanically expressible` with a 1-2 sentence rationale.
//
// The meta-test enforces this. Grandfathered ADRs may carry the
// `**Fitness function:** TBD — grandfathered` marker but at most 5
// such markers may exist at any time, forcing gradual gap closure.
//
// # Cited canon
//
//   - Ford / Parsons / Kua — Building Evolutionary Architectures (2017)
//   - Neal Ford — Software Architecture: The Hard Parts (2021)
//   - Vernon — Implementing DDD (sealed events, aggregate factories)
//   - Khorikov — Unit Testing Principles (2020) ch. 11 (clock injection)
//   - Cheney — "Accept interfaces, return structs" (2016 blog)
//   - Mat Ryer — "How I write HTTP services" (2024)
//   - ThreeDotsLabs Wild Workouts (the canonical Go DDD reference)
//   - Brandur Leach — Crunchy Bridge architecture (outbox / sqlc canon)
//   - Stripe API platform — money-as-int64, spec-of-record
//   - Auth0 / GitHub — enumeration safety + URL design canon
//   - NIST 800-63B §5.2.2 — account lockout policy
package architecture
