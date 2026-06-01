---
name: verify-green
description: Verification + commit discipline for this repo. Use before claiming work is done/green, before any git commit, and after multi-file edits. Prevents the commit-before-verify failure mode.
---

# verify-green discipline

NEVER claim green or commit without reading the actual exit code of a fresh run.

## Order (non-negotiable)
1. Make edits.
2. Run gates to a MARKED log; READ the markers directly — never infer from a prior run or IDE diagnostics (those lag/stale):
   - `gofmt -l <changed>` must be empty.
   - `go build ./... ; echo BUILD=$?` and `go build -tags=integration ./... ; echo BUILDINT=$?` → both 0.
   - `go clean -testcache; go test -run "^Test(Arch|Meta)_" ./internal/architecture/... ; echo ARCH=$?` → 0.
   - For behavior changes: full integration `go test -shuffle=on -tags=integration -timeout=15m -p 1 ./... ; echo EXIT=$?` → 0 (Docker testcontainers).
3. ONLY after reading every marker = pass → commit. The verify run and the commit are SEPARATE steps, never the same tool batch.

## Rules
- Stage explicit files (`git add <paths>`), not `git add -A` — a half-applied edit batch must not auto-commit.
- The commit message must state the ACTUAL verified numbers (EXIT=0, ok=N, fail=0). Never write "green" you didn't read.
- Background long runs to a log file; read `grep EXIT= / FAIL` from the log, not the full dump (saves tokens).
- A `Stop` hook in settings.json enforces build+arch green on turn end — but don't rely on it; gate yourself.
- Branches live in the main repo (`d:/Development/leadkart-go`), not worktrees. main is PR-protected; commit/push only when asked.

## When a gate is RED
Fix prod / fix the rewrite — never weaken the test, never add an allowlist/annotation to silence it. A failing arch gate that greps a comment substring (e.g. `TxScopeTenant`, `arch-test:*`, em-dash reason) means a marker was dropped — restore the marker, don't edit the gate.
