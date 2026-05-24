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
// # The 25-principle taxonomy
//
// Tests are organised by the DESIGN PRINCIPLE they enforce, not by the
// symptom they catch. A symptom-named test ("clock injection") is a
// proxy for the underlying principle ("pure domain — no hidden
// inputs"); when the principle is captured directly, the test surface
// stops bloating every time the symptom catalogue grows.
//
// May 2026 → June 2026 expansion: the original 14-principle / 98-test
// catalog (preserved in full) gained 11 new principles + ~110 tests
// covering the substrate areas the round-1 catalog didn't reach:
// sqlc/pgx, goose-discipline, cache, context, resource-cleanup,
// generics, error-handling, layout, naming, type-safety, ingress
// hardening (HTTP/audit/rate-limit), CQRS, lint, jobs, testing,
// docs, build determinism, channels, PR-time gates.
//
// Round-1 catalog (98 tests preserved by name; numbering stable):
//
//   P1  Pure Domain                — pure_domain_arch_test.go         (10)
//   P2  Explicit DI                — explicit_di_arch_test.go         ( 5)
//   P3  Aggregate Invariants       — aggregate_invariants_arch_test.go( 8)
//   P4  Event-Driven Communication — eda_arch_test.go                 ( 6)
//   P5  Persistence as Adapter     — persistence_arch_test.go         ( 9 *)
//   P6  Multi-Tenancy Enforcement  — multi_tenancy_arch_test.go       ( 6)
//   P7  Auth in Middleware         — auth_middleware_arch_test.go     ( 5)
//   P8  Concurrency Safety         — concurrency_arch_test.go         ( 4)
//   P9  Modern Go Idioms           — modern_go_arch_test.go           ( 7)
//   P10 URL / API Conformance      — url_api_arch_test.go             ( 8)
//   P11 DB Schema Hygiene          — db_schema_arch_test.go           (15)
//   P12 Observability Uniformity   — observability_arch_test.go       ( 8)
//   P13 Performance Discipline     — performance_arch_test.go         ( 5)
//   P14 Meta / Process             — meta_arch_test.go                ( 2)
//                                                                     ---
//                                                                      98
//
// Round-2 expansion (June 2026 — ~109 net new tests):
//
//   A   sqlc + pgx discipline      — sqlc_arch_test.go                ( 8)
//   B   goose-migration            — db_schema_arch_test.go (B1..B6)  ( 6)
//   C   Watermill messaging deep   — eda_arch_test.go (C1..C5)        ( 5)
//   D   JWT / crypto deep          — auth_middleware_arch_test.go     ( 7)
//   E   Cache discipline           — cache_arch_test.go               ( 7)
//   F   Context discipline         — context_arch_test.go             ( 6)
//   G   Resource cleanup           — resource_cleanup_arch_test.go    ( 5)
//   H   HTTP server hardening      — url_api_arch_test.go (H1..H4)    ( 4)
//   I   River background jobs      — jobs_arch_test.go                ( 4)
//   J   Testing discipline         — testing_arch_test.go             ( 5)
//   K   Generics                   — generics_arch_test.go            ( 3)
//   L   CGO + build determinism    — meta_arch_test.go (L1..L3)       ( 3)
//   M   Error handling deep        — error_handling_arch_test.go      ( 8)
//   N   Audit log discipline       — audit_arch_test.go               ( 4)
//   O   Rate limiting              — rate_limit_arch_test.go          ( 3)
//   P   Lint / static analysis     — lint_arch_test.go                ( 4)
//   Q   Numeric precision          — modern_go_arch_test.go (Q1..Q2)  ( 2)
//   R   Timezone discipline        — modern_go_arch_test.go (R1..R3)  ( 3)
//   S   CQRS discipline            — persistence_arch_test.go (S1..S3)( 3)
//   T   Folder + file conventions  — layout_arch_test.go              ( 7)
//   U   Documentation discipline   — meta_arch_test.go (U1..U3)       ( 3)
//   V   Naming conventions         — naming_arch_test.go              ( 4)
//   W   PR-time / CI gates         — meta_arch_test.go (W5)           ( 1)
//   X   Channel-shape              — modern_go_arch_test.go (X1)      ( 1)
//   Y   Type safety                — type_safety_arch_test.go         ( 4)
//                                                                     ---
//                                                                     ~110
//
// Grand total: ~207 tests across 25 principle categories.
//
// * P5 carries 3 preserved-from-the-original-19 layer-boundary tests
//   (PortsAdaptersDontDefineInterfaces, AppDoesntImportPorts,
//   DomainHasNoInfraImports).
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
// running cap is ≤ 25 skipped tests across the suite (round-2
// expansion raised the cap from 15 to accommodate the larger
// principle surface) — the suite must overwhelmingly enforce LIVE
// constraints, not document tech debt. Current state: 15 of 207
// tests skip; 5 of those skips came from the round-2 expansion, the
// other 10 are inherited from round-1.
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
