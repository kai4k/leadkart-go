// cache_arch_test.go — Principle E: Cache discipline.
//
// ADR 0015 (ristretto + redis + singleflight) + ADR 0042 (five TTL profiles).
// Guards: all cache via HybridCache, canonical key shape, tenant scope,
// jittered TTL profiles, singleflight stampede protection.

package architecture_test

import (
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestArch_AllCacheCallsViaHybridCache asserts direct ristretto/redis use is
// banned outside common/cache/ (ADR 0015).
// Scope: production — applies to non-test files; test-side discipline lives under Principle TD/TP.
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

// TestArch_CacheKeyFormatCanonical asserts cache.*(ctx, key, ...) string keys
// follow <scope>:<entity>:<id> (≥2 colons; Stripe/GitHub canon).
// Scope: production — applies to non-test files; test-side discipline lives under Principle TD/TP.
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

// TestArch_CacheTTLProfileExplicit asserts no cache.TTL{} literal is constructed
// outside common/cache/. Use a named profile fn (ADR 0042).
// Scope: production — applies to non-test files; test-side discipline lives under Principle TD/TP.
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

// TestArch_CacheKeysIncludeTenantScope asserts tenant-bearing cache keys include
// :tenant: or :t:. Missing scope risks serving Tenant A's data to Tenant B.
// Scope: production — applies to non-test files; test-side discipline lives under Principle TD/TP.
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

// TestArch_SingleflightWrapsExpensiveMisses asserts common/cache/ substrate
// references singleflight.Group. ADR 0015: prevents thundering-herd.
// Scope: production — applies to non-test files; test-side discipline lives under Principle TD/TP.
func TestArch_SingleflightWrapsExpensiveMisses(t *testing.T) {
	t.Parallel()

	dir := filepath.Join(internalDir(t), "common", "cache")
	found := false
	walkGoFiles(t, dir, false, func(_ string, src []byte) {
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

// TestArch_CacheKeyLengthBounded asserts literal cache keys are ≤ 256 chars.
// Scope: production — applies to non-test files; test-side discipline lives under Principle TD/TP.
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

// TestArch_CacheInvalidationPairedWithEvent asserts cache Delete/Invalidate
// calls are in ports/subscribers/, not in write handlers.
// Scope: production — applies to non-test files; test-side discipline lives under Principle TD/TP.
func TestArch_CacheInvalidationPairedWithEvent(t *testing.T) {
	t.Parallel()

	root := internalDir(t)
	invalRE := regexp.MustCompile(`\b(?:cache|facade|c)\.(?:Delete|Invalidate)\(`)
	var bad []string

	for _, mod := range modulesUnderInternal(t) {
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

// lineNumberAt returns the 1-indexed line number for byte offset off in src.
func lineNumberAt(src string, off int) int {
	if off > len(src) {
		off = len(src)
	}
	return strings.Count(src[:off], "\n") + 1
}

// hasOptOutNearby reports whether line or the preceding line contains directive.
func hasOptOutNearby(src string, line int, directive string) bool {
	return strings.Contains(readLine(src, line), directive) ||
		strings.Contains(readLine(src, line-1), directive)
}
