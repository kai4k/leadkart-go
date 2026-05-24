# Known violations — architectural fitness-function suite

This file tracks intentional `t.Skip(...)` decisions in the
`internal/architecture/` suite (Ford / Parsons / Kua canon — see
`doc.go`). Per the suite's process discipline: prefer fix-the-violation
over skip-the-test; reach for the skip path only when the violation is
invasive (>50 LOC) and a separate cleanup PR is the right scope.

## Current state — 2026-05-24

**Zero tests skipped. Zero known violations.**

The initial 19-test suite shipped on top of `feature/inventory-slice-1`
with every test in PASSING state. The minor in-line fixes applied
during the green-up sweep:

| Fix | Files | Why |
|---|---|---|
| `interface{}` → `any` (3 occurrences) | `internal/identity/ports/http.go`, `internal/identity/app/jwt/jwt.go` | Go 1.18+ alias; ADR 0034 idiom (caught by `TestArch_NoInterfaceEmpty`). |

## False-positive relaxations applied during construction

| Test | Relaxation | Rationale |
|---|---|---|
| `TestArch_NoCrossModuleImports` | Shared-kernel allow-list for `identity/domain/{tenant,membership,permission}`, `identity/ports/authn`, `identity/app/actclaim` | Vernon IDDD ch. 13 "Shared Kernel" pattern + ADR 0051 (Wave 9.1a/b). The identity bounded context owns the canonical typed-ID surface + cross-cutting authn middleware. Refusing these would force every module to redeclare UUID-typed IDs (anaemic-shared-kernel anti-pattern). |
| `TestArch_RepositoriesHaveUpdateByIDFn` | Append-only exceptions: `stockmovement`, `verificationcall`, `leadcredit` (optimistic-UpsertWithVersion) | All three are documented in their `repository.go` godoc as canonical design choices (ADR 0059 §"Optimistic concurrency" + Vernon IDDD ch. 10 on event-stream-ish aggregates). |
| `TestArch_PortsAdaptersDontDefineInterfaces` | Closed allow-list for 5 consumer-side interfaces that legitimately live with their primary impl | `ImpersonationAuditWriter` (PG + noop impls in same bundle), `PersonStampReader`, `SecurityStampInvalidator`, `Verifier`, `StampValidator` — middleware-local substitution points. Each entry has a one-line rationale in the test's godoc. |
| `TestArch_DomainHasNoInfraImports` | Allow-list of pure VO/utility leaves under `common/`: `errs, ids, slug, email, phone, pan, gst, postaladdress, druglicence, pagination, tenancy` | These are pure value-objects + typed-ID factories with no I/O — they're the project's pure-functional kernel. Adding a new leaf requires an ADR amendment. |
| `TestArch_NoMustInRequestPath` | Constructor-form regex `Must(New|Parse|Compile|Load|Init|Build|Open|Create|Configure)` | Avoids flagging legitimate domain methods named `Must<Verb>` (e.g. `person.MustChangePassword()` boolean flag-getter). Only panics-on-error constructors are anti-patterns per CLAUDE.md ctor-patterns. |
| `TestArch_NoBannedDeps` | Filters to **direct** `require` entries in `go.mod` (skips `// indirect`) | `sirupsen/logrus` rides in transitively via `testcontainers-go`; the ban applies to LeadKart code's direct dependency, not to the transitive surface. |

## How to add an entry to this file

1. Hit a red TestArch_*.
2. Diagnose the root cause. If trivial (< 50 LOC), fix the violation in the same PR.
3. If invasive, append a `t.Skip("known violation: <reason> — tracked in KNOWN_VIOLATIONS.md")` line + add a row here with the rationale + tracking PR link.
4. The next PR that closes the violation removes the `t.Skip` AND the row here.

See also: the parent process discipline in `doc.go` ("Process discipline").
