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
// # The 14-principle taxonomy
//
// Tests are organised by the DESIGN PRINCIPLE they enforce, not by the
// symptom they catch. A symptom-named test ("clock injection") is a
// proxy for the underlying principle ("pure domain — no hidden
// inputs"); when the principle is captured directly, the test surface
// stops bloating every time the symptom catalogue grows.
//
//   P1  Pure Domain                — pure_domain_arch_test.go         (10 tests)
//   P2  Explicit DI                — explicit_di_arch_test.go         ( 5 tests)
//   P3  Aggregate Invariants       — aggregate_invariants_arch_test.go( 8 tests)
//   P4  Event-Driven Communication — eda_arch_test.go                 ( 6 tests)
//   P5  Persistence as Adapter     — persistence_arch_test.go         ( 9 tests *)
//   P6  Multi-Tenancy Enforcement  — multi_tenancy_arch_test.go       ( 6 tests)
//   P7  Auth in Middleware         — auth_middleware_arch_test.go     ( 5 tests)
//   P8  Concurrency Safety         — concurrency_arch_test.go         ( 4 tests)
//   P9  Modern Go Idioms           — modern_go_arch_test.go           ( 7 tests)
//   P10 URL / API Conformance      — url_api_arch_test.go             ( 8 tests)
//   P11 DB Schema Hygiene          — db_schema_arch_test.go           (15 tests)
//   P12 Observability Uniformity   — observability_arch_test.go       ( 8 tests)
//   P13 Performance Discipline     — performance_arch_test.go         ( 5 tests)
//   P14 Meta / Process             — meta_arch_test.go                ( 2 tests)
//                                                                     ----
//                                                                       98
//
// * P5 carries 3 preserved-from-the-original-19 layer-boundary tests
//   (PortsAdaptersDontDefineInterfaces, AppDoesntImportPorts,
//   DomainHasNoInfraImports). The brief specifies 6 net-new P5 tests;
//   keeping the preserved 3 maintains coverage continuity. Total 98 vs
//   target 95 — three-test overshoot in favour of zero coverage loss.
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
// # Skip discipline
//
// Tests that hit invasive violations (>50 LOC fix) may `t.Skip("known
// violation: <reason> — tracked in KNOWN_VIOLATIONS.md")`. The
// running cap is < 15 skipped tests across the suite — the suite must
// overwhelmingly enforce LIVE constraints, not document tech debt.
//
// # Cited canon
//
//   - Ford / Parsons / Kua — Building Evolutionary Architectures (2017)
//   - Neal Ford — Software Architecture: The Hard Parts (2021)
//   - Vernon — Implementing DDD (sealed events, aggregate factories)
//   - Khorikov — Unit Testing Principles (2020) ch. 11 (clock injection,
//     time-pure domain, the "humble object" pattern)
//   - Cheney — "Accept interfaces, return structs" (2016 blog)
//   - Mat Ryer — "How I write HTTP services" (2024)
//   - ThreeDotsLabs Wild Workouts (the canonical Go DDD reference)
//   - Brandur Leach — Crunchy Bridge architecture (outbox / sqlc canon)
//   - Stripe API platform — money-as-int64, spec-of-record, idempotency
//   - Auth0 / GitHub — enumeration safety + URL design canon
//   - NIST 800-63B §5.2.2 — account lockout policy
//   - RFC 9457 — Problem Details for HTTP APIs
//   - RFC 8693 — OAuth 2.0 Token Exchange (the `act` claim)
//   - OWASP API Top 10 (2023)
package architecture
