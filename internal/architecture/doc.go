// Package architecture is the LeadKart-Go architectural fitness-function
// suite — a CI-time gate that converts the project's ADR catalog into
// mechanically-enforced tests.
//
// Per Ford / Parsons / Kua "Building Evolutionary Architectures" (2017):
// fitness functions prevent architectural drift. Without them, ADRs rot into
// aspirational documentation. Cross-module + global-invariant tests live here;
// per-module tests live alongside their modules.
//
// Tests are organised by DESIGN PRINCIPLE, not by symptom.
//
// Round-1 catalog (98 tests; numbering stable):
//
//	P1  Pure Domain                — pure_domain_arch_test.go         (10)
//	P2  Explicit DI                — explicit_di_arch_test.go         ( 5)
//	P3  Aggregate Invariants       — aggregate_invariants_arch_test.go( 8)
//	P4  Event-Driven Communication — eda_arch_test.go                 ( 6)
//	P5  Persistence as Adapter     — persistence_arch_test.go         ( 9 *)
//	P6  Multi-Tenancy Enforcement  — multi_tenancy_arch_test.go       ( 6)
//	P7  Auth in Middleware         — auth_middleware_arch_test.go     ( 5)
//	P8  Concurrency Safety         — concurrency_arch_test.go         ( 4)
//	P9  Modern Go Idioms           — modern_go_arch_test.go           ( 7)
//	P10 URL / API Conformance      — url_api_arch_test.go             ( 8)
//	P11 DB Schema Hygiene          — db_schema_arch_test.go           (15)
//	P12 Observability Uniformity   — observability_arch_test.go       ( 8)
//	P13 Performance Discipline     — performance_arch_test.go         ( 5)
//	P14 Meta / Process             — meta_arch_test.go                ( 2)
//	                                                                  ---
//	                                                                   98
//
// Round-2 expansion (June 2026 — ~109 net new tests):
//
//	A   sqlc + pgx discipline      — sqlc_arch_test.go                ( 8)
//	B   goose-migration            — db_schema_arch_test.go (B1..B6)  ( 6)
//	C   Watermill messaging deep   — eda_arch_test.go (C1..C5)        ( 5)
//	D   JWT / crypto deep          — auth_middleware_arch_test.go     ( 7)
//	E   Cache discipline           — cache_arch_test.go               ( 7)
//	F   Context discipline         — context_arch_test.go             ( 6)
//	G   Resource cleanup           — resource_cleanup_arch_test.go    ( 5)
//	H   HTTP server hardening      — url_api_arch_test.go (H1..H4)    ( 4)
//	I   River background jobs      — jobs_arch_test.go                ( 4)
//	J   Testing discipline         — testing_arch_test.go             ( 5)
//	K   Generics                   — generics_arch_test.go            ( 3)
//	L   CGO + build determinism    — meta_arch_test.go (L1..L3)       ( 3)
//	M   Error handling deep        — error_handling_arch_test.go      ( 8)
//	N   Audit log discipline       — audit_arch_test.go               ( 4)
//	O   Rate limiting              — rate_limit_arch_test.go          ( 3)
//	P   Lint / static analysis     — lint_arch_test.go                ( 4)
//	Q   Numeric precision          — modern_go_arch_test.go (Q1..Q2)  ( 2)
//	R   Timezone discipline        — modern_go_arch_test.go (R1..R3)  ( 3)
//	S   CQRS discipline            — persistence_arch_test.go (S1..S3)( 3)
//	T   Folder + file conventions  — layout_arch_test.go              ( 7)
//	U   Documentation discipline   — meta_arch_test.go (U1..U3)       ( 3)
//	V   Naming conventions         — naming_arch_test.go              ( 4)
//	W   PR-time / CI gates         — meta_arch_test.go (W5)           ( 1)
//	X   Channel-shape              — modern_go_arch_test.go (X1)      ( 1)
//	Y   Type safety                — type_safety_arch_test.go         ( 4)
//	                                                                  ---
//	                                                                  ~110
//
// Grand total: ~207 tests across 25 principle categories.
// P5 carries 3 layer-boundary tests preserved from the original 19.
//
// # Process discipline
//
// New ADRs MUST land with either:
//
//  1. A `## Fitness function` section naming a TestArch_* test, OR
//  2. The marker `**Fitness function:** convention-only — not
//     mechanically expressible` with a 1-2 sentence rationale.
//
// Grandfathered ADRs carry `**Fitness function:** TBD — grandfathered`;
// at most 5 at a time.
//
// # Skip discipline
//
// Tests hitting invasive violations (>50 LOC fix) may
// `t.Skip("known violation: <reason> — tracked in KNOWN_VIOLATIONS.md")`.
// Cap: ≤ 25 skipped tests. Current: 15 of 207 skip.
//
// # Cited canon
//
//   - Ford / Parsons / Kua — Building Evolutionary Architectures (2017)
//   - Vernon — Implementing DDD; Khorikov — Unit Testing Principles ch. 11
//   - Cheney — "Accept interfaces, return structs"; Mat Ryer 2024
//   - ThreeDotsLabs Wild Workouts (canonical Go DDD reference)
//   - Brandur Leach — Crunchy Bridge (outbox / sqlc canon)
//   - Stripe / Auth0 / GitHub — URL design, idempotency, enumeration safety
//   - NIST 800-63B §5.2.2; RFC 9457; RFC 8693; OWASP API Top 10 (2023)
package architecture
