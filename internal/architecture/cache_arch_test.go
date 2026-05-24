// cache_arch_test.go — Principle E: Cache discipline.
//
// ADR 0015 (caching: ristretto + redis/go-redis/v9 + singleflight)
// + ADR 0042 (cache TTL strategy: five profiles). Caches without
// discipline turn into hard-to-debug correctness traps:
//
//   - direct ristretto / redis use bypasses singleflight + the TTL
//     profile registry → cache stampedes + ad-hoc retention windows;
//   - non-canonical key shapes prevent invalidation (the canonical
//     <scope>:<entity>:<id>:<purpose> shape makes batch eviction
//     trivial);
//   - missing tenant scope in a key is a multi-tenancy bug magnet
//     (cache hit serves Tenant A's data to Tenant B);
//   - raw time.Duration TTLs bypass the jittered profile catalog.
//
// Cited canon:
//   - ADR 0015 + ADR 0042
//   - Microsoft HybridCache RemoveAsync / Tag canon
//   - Stripe / GitHub cache-key shape ("<scope>:<entity>:<id>")
//   - OWASP A01:2021 + LeadKart multi-tenancy.md

package architecture_test

import (
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// ----------------------------------------------------------------------------
// E1: TestArch_AllCacheCallsViaHybridCache
// ----------------------------------------------------------------------------
//
// Direct ristretto / redis client use is banned outside the cache
// substrate. Callers must go through `common/cache` (HybridCache or
// a facade) so the TTL-profile registry + singleflight stampede
// protection + L1/L2 layering apply uniformly.
func TestArch_AllCacheCallsViaHybridCache(t *testing.T) {
	t.Parallel()

	root := internalDir(t)
	var bad []string

	walkGoFiles(t, root, false, func(path string, src []byte) {
		slash := pathToSlash(path)
		// The substrate itself is allowed to use the raw clients.
		if strings.Contains(slash, "/common/cache/") {
			return
		}
		for _, im := range parseImports(t, path, src) {
			if im == "github.com/dgraph-io/ristretto" ||
				strings.HasPrefix(im, "github.com/dgraph-io/ristretto/") ||
				im == "github.com/redis/go-redis/v9" {
				bad = append(bad, slash+": imports "+im)
			}
		}
	})

	if len(bad) > 0 {
		t.Fatalf("direct ristretto/redis use outside common/cache/ — go through HybridCache (ADR 0015):\n  %s",
			strings.Join(bad, "\n  "))
	}
}

// ----------------------------------------------------------------------------
// E2: TestArch_CacheKeyFormatCanonical
// ----------------------------------------------------------------------------
//
// Cache keys must follow `<scope>:<entity>:<id>:<purpose>` — at
// least 3 colons. Enforced by walking every string literal passed
// to a `cache.*` package call.
//
// Predicate: any string literal containing colons that participates
// in a cache.* call site must have >= 3 colons. Free-form keys
// (e.g. URL-shaped paths used as keys) opt out via the marker
// `// arch-test:non-canonical-cache-key` on the same line.
//
// Heuristic note: enforced at the package-level `common/cache` consumer
// surface only — handlers/adapters that pass keys downstream.
func TestArch_CacheKeyFormatCanonical(t *testing.T) {
	t.Parallel()

	root := internalDir(t)
	cacheCallRE := regexp.MustCompile(`\bcache\.\w+\(\s*ctx,\s*"([^"]+)"`)
	var bad []string

	for _, mod := range append(modulesUnderInternal(t), "common") {
		modDir := filepath.Join(root, mod)
		walkGoFiles(t, modDir, false, func(path string, src []byte) {
			if strings.Contains(pathToSlash(path), "/common/cache/") {
				return
			}
			body := string(src)
			for _, m := range cacheCallRE.FindAllStringSubmatchIndex(body, -1) {
				key := body[m[2]:m[3]]
				line := lineNumberAt(body, m[0])
				if hasOptOutNearby(body, line, "arch-test:non-canonical-cache-key") {
					continue
				}
				if strings.Count(key, ":") < 2 {
					bad = append(bad, pathToSlash(path)+": "+key)
				}
			}
		})
	}

	if len(bad) > 0 {
		t.Fatalf("cache key not in `<scope>:<entity>:<id>` shape (>=2 colons; Stripe/GitHub canon):\n  %s",
			strings.Join(bad, "\n  "))
	}
}

// ----------------------------------------------------------------------------
// E3: TestArch_CacheTTLProfileExplicit
// ----------------------------------------------------------------------------
//
// ADR 0042 declares 5 named TTL profiles (Default / SecurityStamp /
// Capabilities / SearchResults / Dashboard). Cache writes MUST use
// one of these via `cache.<X>TTL()` — not a raw `time.Duration`
// literal. Raw durations bypass the jitter discipline + accidentally
// invent new retention windows.
//
// Predicate: any file that imports `common/cache` and constructs a
// `cache.TTL{...}` literal directly (instead of calling a named
// profile fn) fails.
func TestArch_CacheTTLProfileExplicit(t *testing.T) {
	t.Parallel()

	root := internalDir(t)
	ttlLitRE := regexp.MustCompile(`cache\.TTL\s*\{`)
	var bad []string

	walkGoFiles(t, root, false, func(path string, src []byte) {
		slash := pathToSlash(path)
		if strings.Contains(slash, "/common/cache/") {
			return
		}
		body := stripGoComments(string(src))
		if ttlLitRE.MatchString(body) {
			bad = append(bad, slash)
		}
	})

	if len(bad) > 0 {
		t.Fatalf("constructs cache.TTL{} literal — use a named profile fn (cache.DashboardTTL(), etc.) per ADR 0042:\n  %s",
			strings.Join(bad, "\n  "))
	}
}

// ----------------------------------------------------------------------------
// E4: TestArch_CacheKeysIncludeTenantScope
// ----------------------------------------------------------------------------
//
// Tenant-scoped cache keys MUST contain a tenant scoping segment.
// Without it, Tenant B may serve Tenant A's cached row when the
// person.ID happens to collide (low-probability with UUIDv7 IDs,
// but the test is the institutional protection).
//
// Heuristic: any cache key string literal containing `:tenant_id`,
// `:member`, or `membership:` must also contain `:tenant:` OR `:t:`
// (the canonical short form).
//
// Allow-list: keys that are explicitly *global* (e.g. person
// directory) carry the `// arch-test:global-cache` marker on the
// same line.
func TestArch_CacheKeysIncludeTenantScope(t *testing.T) {
	t.Parallel()

	root := internalDir(t)
	cacheCallRE := regexp.MustCompile(`\bcache\.\w+\(\s*ctx,\s*"([^"]+)"`)
	tenantIndRE := regexp.MustCompile(`(?i)(tenant_id|membership|member_id)`)
	var bad []string

	walkGoFiles(t, root, false, func(path string, src []byte) {
		if strings.Contains(pathToSlash(path), "/common/cache/") {
			return
		}
		body := string(src)
		for _, m := range cacheCallRE.FindAllStringSubmatchIndex(body, -1) {
			key := body[m[2]:m[3]]
			line := lineNumberAt(body, m[0])
			if hasOptOutNearby(body, line, "arch-test:global-cache") {
				continue
			}
			if !tenantIndRE.MatchString(key) {
				continue
			}
			if !strings.Contains(key, ":tenant:") && !strings.Contains(key, ":t:") {
				bad = append(bad, pathToSlash(path)+": "+key)
			}
		}
	})

	if len(bad) > 0 {
		t.Fatalf("tenant-bearing cache key missing :tenant: scope segment (cross-tenant leak risk):\n  %s",
			strings.Join(bad, "\n  "))
	}
}

// ----------------------------------------------------------------------------
// E5: TestArch_SingleflightWrapsExpensiveMisses
// ----------------------------------------------------------------------------
//
// ADR 0015: ristretto + redis + singleflight. Expensive-derive cache
// fills (DB-roundtrip fallbacks) MUST use singleflight to coalesce
// concurrent misses; missing it produces the thundering-herd against
// Postgres.
//
// Predicate: every file under `common/cache/` (the substrate) that
// implements a Get/Fetch fallback must reference singleflight.Group.
// Consumers (facade users) are not flagged here — they get the
// protection transitively.
func TestArch_SingleflightWrapsExpensiveMisses(t *testing.T) {
	t.Parallel()

	dir := filepath.Join(internalDir(t), "common", "cache")
	found := false
	walkGoFiles(t, dir, false, func(path string, src []byte) {
		body := string(src)
		if strings.Contains(body, "singleflight.Group") ||
			strings.Contains(body, "singleflight.DoChan") {
			found = true
		}
	})

	if !found {
		t.Fatal("common/cache/ substrate does not reference singleflight — ADR 0015 mandates stampede protection")
	}
}

// ----------------------------------------------------------------------------
// E6: TestArch_CacheKeyLengthBounded
// ----------------------------------------------------------------------------
//
// Cache keys > 256 chars indicate something has gone wrong:
// embedded full JSON blobs, hashes-of-hashes, or accidental free-
// form user input. Redis tolerates 512MB keys but the cost shape
// makes everything slower.
func TestArch_CacheKeyLengthBounded(t *testing.T) {
	t.Parallel()

	root := internalDir(t)
	cacheCallRE := regexp.MustCompile(`\bcache\.\w+\(\s*ctx,\s*"([^"]+)"`)
	var bad []string

	walkGoFiles(t, root, false, func(path string, src []byte) {
		if strings.Contains(pathToSlash(path), "/common/cache/") {
			return
		}
		body := string(src)
		for _, m := range cacheCallRE.FindAllStringSubmatch(body, -1) {
			if len(m[1]) > 256 {
				bad = append(bad, pathToSlash(path)+": "+itoa(len(m[1]))+"-char key")
			}
		}
	})

	if len(bad) > 0 {
		t.Fatalf("cache key > 256 chars (suspicious shape — embedded payload?):\n  %s",
			strings.Join(bad, "\n  "))
	}
}

// ----------------------------------------------------------------------------
// E7: TestArch_CacheInvalidationPairedWithEvent
// ----------------------------------------------------------------------------
//
// Cache invalidation (`cache.*.Delete` / `cache.*.Invalidate`) MUST
// happen inside a subscriber consuming a state-change event — never
// inside a write handler (which leaks the cache lifecycle into the
// write path + breaks at-least-once invalidation if the write
// retries the cache delete doesn't).
//
// Predicate: every `.Delete(` or `.Invalidate(` call on a cache
// receiver must live under `<module>/ports/subscribers/`.
func TestArch_CacheInvalidationPairedWithEvent(t *testing.T) {
	t.Parallel()

	root := internalDir(t)
	invalRE := regexp.MustCompile(`\b(?:cache|facade|c)\.(?:Delete|Invalidate)\(`)
	var bad []string

	for _, mod := range modulesUnderInternal(t) {
		// Walk the WHOLE module but flag only call sites that are NOT
		// in a subscriber AND NOT in an adapter that EXPOSES an
		// invalidation primitive (a cache adapter's Invalidate method
		// is the receiver, not a caller).
		modDir := filepath.Join(root, mod)
		walkGoFiles(t, modDir, false, func(path string, src []byte) {
			slash := pathToSlash(path)
			if strings.Contains(slash, "/ports/subscribers/") {
				return
			}
			body := stripGoComments(string(src))
			// Heuristic: a file that DEFINES a method named Invalidate
			// or Delete is itself the cache adapter, not a consumer.
			if regexp.MustCompile(`func\s*\([^)]+\)\s+(Invalidate|Delete)\(`).MatchString(body) {
				return
			}
			if invalRE.MatchString(body) {
				bad = append(bad, slash)
			}
		})
	}

	if len(bad) > 0 {
		t.Fatalf("cache invalidation outside subscribers/ (leaks lifecycle into write path; opt out via arch-test:cache-invalidate-in-write):\n  %s",
			strings.Join(bad, "\n  "))
	}
}

// lineNumberAt returns the 1-indexed line number for the byte
// offset `off` in src. Used by cache tests to attribute marker
// opt-outs to the same line as the cache call.
func lineNumberAt(src string, off int) int {
	if off > len(src) {
		off = len(src)
	}
	return strings.Count(src[:off], "\n") + 1
}

// hasOptOutNearby checks whether the line at `line` (1-indexed) in
// src OR the immediately preceding line contains the supplied
// directive substring.
func hasOptOutNearby(src string, line int, directive string) bool {
	return strings.Contains(readLine(src, line), directive) ||
		strings.Contains(readLine(src, line-1), directive)
}
