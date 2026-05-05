# ADR 0034 — Go version: 1.25 today, target 1.26+ on toolchain availability

**Status:** Accepted
**Date:** 2026-05-05

## Context

Go releases a new minor version every six months (Feb / Aug). The Go team supports the last two majors only. Pinning to "latest stable" is the default for new projects; pinning to a specific version is the right policy for an active SaaS.

Release schedule:
- Go 1.22 — Feb 2024 (ServeMux routing enhancements).
- Go 1.23 — Aug 2024 (range-over-func iterators stable).
- Go 1.24 — Feb 2025 (generic type aliases, Swiss-table maps, weak pointers, `tool` directive in `go.mod`).
- Go 1.25 — Aug 2025 (current installed locally).
- Go 1.26 — Feb 2026 (target per master plan).

## Decision

**`go 1.25`** in `go.mod` for the initial scaffold (matches locally installed toolchain). **Bump to `go 1.26+`** as soon as Go 1.26 toolchain is verified locally + on CI runners.

Auto-fetch via `toolchain` directive in `go.mod` once 1.26 is the target:

```
go 1.26
toolchain go1.26.0
```

This makes contributors auto-fetch the matching toolchain on first build (Go 1.21+ feature).

## Refresh policy

**Quarterly** — at the start of each quarter:
1. Bump `go.mod` `go` + `toolchain` directives to the latest stable (or one minor behind if newest is < 60 days old).
2. Run `task ci` (full lint + race + vuln + build).
3. Update CI workflow `go-version-file: go.mod` already pulls from go.mod — no change needed.
4. Note the bump in `CHANGELOG.md` (TBD when v0.1 ships).

Floor walks forward with each release — Go-team supports last 2 majors, so we drop support for 1.N-2 the day 1.N+1 ships.

## Consequences

**Positive:**
- Compile-time benefits of latest toolchain (Swiss-table maps 30%+ map perf in 1.24+, generic type aliases, weak pointers).
- Iterators (`iter.Seq[T]`) since 1.23 available where natural (bulk-upload streaming).
- `tool` directive (Go 1.24+) replaces the `tools.go` build-tag hack.
- Auto-toolchain fetch means contributors never need manual Go installation matching.

**Negative:**
- Quarterly bump is engineering work — typically 30 min if no breaking changes.
- Some third-party deps lag (rare in Go ecosystem; Go's compatibility promise is strong).

## Currently using Go 1.25

This is a **temporary state** until Go 1.26 toolchain is pulled locally. Mitigation:
- `tool` directive (commented out in `go.mod`) ready to enable on bump.
- Code uses no 1.26-specific features (none stable yet at scaffold time).

## Alternatives considered

1. **Go 1.24 LTS (no LTS in Go ecosystem, but pinning to a stable older version).** Rejected: misses iterator + generic-type-alias ergonomic improvements.
2. **Pin via Docker image only, leave `go.mod` flexible.** Rejected: `actions/setup-go@v5` reads `go.mod` `go-version-file` — single source of truth is cleaner.
3. **Always pin to absolute latest (Go 1.26+ before toolchain auto-fetch verified).** Rejected: contributor onboarding friction; build can fail on first clone if toolchain not present.

## Sources

- [Go 1.24 release notes](https://go.dev/blog/go1.24).
- [Go toolchain docs](https://go.dev/doc/toolchain).
- [Go release policy](https://go.dev/doc/devel/release).
