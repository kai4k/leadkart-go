// authz_arch_test.go — ADR 0068: every mutating route is authorised, not
// merely authenticated.
package architecture_test

import (
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// selfServiceMutationAllowList: mutating routes legitimately gated by
// authentication only — they act solely on the caller's own Person /
// session, or carry handler-inline authz mandated by an ADR.
var selfServiceMutationAllowList = map[string]string{
	"POST /api/v1/auth/change-password":                    "self: own credential (ADR 0053)",
	"POST /api/v1/auth/request-email-change":               "self: own email",
	"DELETE /api/v1/auth/sessions/{familyId}":              "self: own refresh family (owner-checked in command)",
	"DELETE /api/v1/auth/sessions":                         "self: own refresh families",
	"POST /api/v1/permission-requests":                     "ADR 0055 §8: any caller may submit",
	"POST /api/v1/permission-requests/{requestId}/approve": "ADR 0055 §8: handler-inline manager-or-platform",
	"POST /api/v1/permission-requests/{requestId}/deny":    "ADR 0055 §8: handler-inline manager-or-platform",
	"POST /api/v1/permission-requests/{requestId}/cancel":  "ADR 0055 §8: handler-inline requester-only",
}

// authzFactories are the gates that AUTHORISE. authn.RequirePlatform alone is
// deliberately absent: it proves platform-tenant membership, not a permission
// (is_platform is minted from the tenant slug), so a route gated by it alone is
// still unauthorised. RequirePlatformGate composes it with named permissions.
const authzFactories = `RequirePermission|RequireAnyPermission|RequirePlatformGate|RequireTenantPermission|RequirePermissionOrSelf`

var (
	authzCallRE    = regexp.MustCompile(`authn\.(` + authzFactories + `)\(`)
	authzComposeRE = regexp.MustCompile(`(?m)\b([a-zA-Z_]\w*)\s*:=\s*\w+\s*\(([^)]*)\)`)
)

func authzAliases(text string) map[string]bool { return aliasesOf(text, authzFactories) }

// aliasesOf discovers the identifiers a ports file binds to authn.<factory>(...)
// (factories is a regexp alternation), plus the identifiers composed from them
// (`x := chain(a, b)` where a is already an alias), by closure.
func aliasesOf(text, factories string) map[string]bool {
	bindingRE := regexp.MustCompile(`(?m)\b([a-zA-Z_]\w*)\s*:=\s*authn\.(` + factories + `)\b`)
	active := map[string]bool{}
	for _, m := range bindingRE.FindAllStringSubmatch(text, -1) {
		active[m[1]] = true
	}
	for round := 0; round < 4; round++ {
		added := false
		for _, m := range authzComposeRE.FindAllStringSubmatch(text, -1) {
			if active[m[1]] {
				continue
			}
			for name := range active {
				if regexp.MustCompile(`\b` + regexp.QuoteMeta(name) + `\b`).MatchString(m[2]) {
					active[m[1]], added = true, true
					break
				}
			}
		}
		if !added {
			break
		}
	}
	return active
}

func exprHasAuthz(expr string, aliases map[string]bool) bool {
	if authzCallRE.MatchString(expr) {
		return true
	}
	for name := range aliases {
		if regexp.MustCompile(`\b` + regexp.QuoteMeta(name) + `\b`).MatchString(expr) {
			return true
		}
	}
	return false
}

// TestArch_MutatingRoutesRequireAuthorization fails when any module
// registers a POST/PUT/PATCH/DELETE route whose handler expression carries
// only authentication (auth/RequireFreshStamp/RequireTenantContext) — the
// bug class that let any tenant member grant themselves roles (ADR 0068).
// Escape hatch: selfServiceMutationAllowList (reason required; stale
// entries fail).
//
// Scope: production — route registrations live in ports/http.go; test files register none.
//
// arch-test:no-negative-fixture (RED proof: wrap any identity mutating route in `auth(` → this test fails; recorded in the Task 4 commit message)
func TestArch_MutatingRoutesRequireAuthorization(t *testing.T) {
	t.Parallel()
	type violation struct {
		file string
		line int
		key  string
	}
	var violations []violation
	registered := map[string]bool{}
	for _, mod := range modulesUnderInternal(t) {
		portsDir := filepath.Join(internalDir(t), mod, "ports")
		walkGoFiles(t, portsDir, false, func(path string, src []byte) {
			slash := pathToSlash(path)
			if !strings.HasSuffix(slash, "/ports/http.go") && !strings.HasSuffix(slash, "_http.go") {
				return
			}
			text := string(src)
			aliases := authzAliases(text)
			for _, m := range muxHandleRE.FindAllStringSubmatchIndex(text, -1) {
				key := text[m[2]:m[3]]
				registered[key] = true
				method, _, _ := strings.Cut(key, " ")
				if method == "GET" || publicRoutesAllowList[key] {
					continue
				}
				if _, ok := selfServiceMutationAllowList[key]; ok {
					continue
				}
				if !exprHasAuthz(extractMuxHandlerExpr(text, m[1]), aliases) {
					violations = append(violations, violation{slash, 1 + strings.Count(text[:m[0]], "\n"), key})
				}
			}
		})
	}
	for key := range selfServiceMutationAllowList {
		if !registered[key] {
			t.Errorf("selfServiceMutationAllowList entry %q is not registered by any module — remove it", key)
		}
	}
	for _, v := range violations {
		t.Errorf("%s:%d %s — mutating route gated by authentication only; wrap in authn.RequirePermission/RequireAnyPermission/RequireTenantPermission/RequirePlatformGate, or (only if it acts solely on the caller's own identity) add it to selfServiceMutationAllowList with a reason", v.file, v.line, v.key)
	}
}
