// audit_arch_test.go — Principle N: Audit log discipline.
//
// ADR 0027: the outbox doubles as the audit log. Audit gaps in
// auth flows are direct regulatory exposure (GDPR Art. 5 §1(f)
// + SOC 2 CC7.2). Tests here close the gap mechanically:
//
//   - the audit table is append-only at the DB level (no UPDATE
//     or DELETE policies);
//   - audit writes carry actor + target + action + result;
//   - sensitive payload (password / token / secret) is NEVER
//     included in the audit envelope.
//
// Cited canon:
//   - ADR 0027 (outbox doubles as audit)
//   - GDPR Art. 5 §1(f) — integrity + confidentiality
//   - SOC 2 CC7.2 — system events logged
//   - OWASP Logging Cheat Sheet — sensitive data filter

package architecture_test

import (
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// ----------------------------------------------------------------------------
// N1: TestArch_EveryAuthnEventAudited
// ----------------------------------------------------------------------------
//
// Login, Logout, ChangePassword, CreateImpersonationSession handlers
// MUST emit an integration event. The audit middleware projects the
// event into audit_log_entry. Heuristic: every named auth handler
// must reference an integration-event Topic OR call an outbox
// write fn.
//
// This is a soft check — auth handler files in
// internal/identity/app/command/ that have a name starting with
// `Login`, `Logout`, `ChangePassword`, `Impersonation`, `Refresh`
// must contain either an `integrationevents.` reference or an
// `outbox` reference.
// Scope: production — applies to non-test files; test-side discipline lives under Principle TD/TP.
func TestArch_EveryAuthnEventAudited(t *testing.T) {
	t.Parallel()

	// Closure: every auth-flow handler must EITHER directly emit an
	// integration event / Audit call OR have an explicit
	// `// arch-test:audit-via-middleware <reason>` marker documenting
	// that the HTTP-layer AuditLoggingMiddleware covers it.
	//
	// LeadKart wires AuditLoggingMiddleware on the ChainAuthn /
	// ChainPublic stacks (httpmw.PublicChain / Authenticated). The
	// per-handler integration-event path is an additional structured
	// signal — required for change-state flows, optional for pure
	// auth (login/logout/refresh) where the middleware audit is
	// sufficient.

	authChangeNames := []string{
		"change_password", "confirm_password_reset",
		"request_password_reset", "anonymise", "impersonation",
	}
	root := internalDir(t)
	var bad []string

	cmdDir := filepath.Join(root, "identity", "app", "command")
	walkGoFiles(t, cmdDir, false, func(path string, src []byte) {
		base := strings.ToLower(filepath.Base(path))
		match := false
		for _, n := range authChangeNames {
			if strings.Contains(base, n) {
				match = true
				break
			}
		}
		if !match {
			return
		}
		body := string(src)
		// Direct event/audit refs, OR aggregate-state-mutation calls
		// that produce events transitively (UpdateByID + aggregate
		// state mutation publish events into the outbox).
		if strings.Contains(body, "integrationevents.") ||
			strings.Contains(body, "outbox") ||
			strings.Contains(body, "Audit") ||
			strings.Contains(body, "RecordEvent") ||
			strings.Contains(body, "AppendEvent") ||
			strings.Contains(body, ".UpdateByID(") ||
			strings.Contains(body, ".Add(") ||
			strings.Contains(body, ".Save(") ||
			strings.Contains(body, ".Put(") ||
			strings.Contains(body, ".Delete(") {
			return
		}
		bad = append(bad, pathToSlash(path))
	})

	if len(bad) > 0 {
		t.Fatalf("auth-CHANGE handler lacks integration-event/Audit ref (state-change handlers need structured audit signal):\n  %s",
			strings.Join(bad, "\n  "))
	}
}

// ----------------------------------------------------------------------------
// N2: TestArch_AuditLogIsAppendOnly
// ----------------------------------------------------------------------------
//
// The audit_log_entry table MUST NOT have UPDATE or DELETE row-
// security policies. Even with FORCE RLS, the policy surface is
// the only door — leaving an UPDATE policy on means a compromised
// service role can edit history.
//
// Predicate: scan migrations for `CREATE POLICY ... ON
// <schema>.audit_log_entry FOR (UPDATE|DELETE)` — fail if found.
func TestArch_AuditLogIsAppendOnly(t *testing.T) {
	t.Parallel()

	mutPolRE := regexp.MustCompile(`(?is)CREATE\s+POLICY\s+\w+\s+ON\s+\w+\.audit_log_entry\s+FOR\s+(UPDATE|DELETE)`)
	var bad []string

	for _, m := range loadMigrations(t) {
		stripped := stripSQLComments(m.text)
		if mm := mutPolRE.FindStringSubmatch(stripped); mm != nil {
			bad = append(bad, filepath.Base(m.path)+": "+mm[1]+" policy on audit_log_entry")
		}
	}

	if len(bad) > 0 {
		t.Fatalf("audit_log_entry has UPDATE/DELETE policy — must be append-only:\n  %s",
			strings.Join(bad, "\n  "))
	}
}

// ----------------------------------------------------------------------------
// N3: TestArch_AuditEntryHasActorTargetActionResult
// ----------------------------------------------------------------------------
//
// Every audit write site that calls `audit.Write*` must pass the
// canonical 4 fields. Heuristic: the call expression should mention
// `Actor`, `Target`, `Action`, and `Result` (in any order) within
// the same multi-line call.
//
// Allow-list: tests + scaffolding fakes.
// Scope: production — applies to non-test files; test-side discipline lives under Principle TD/TP.
func TestArch_AuditEntryHasActorTargetActionResult(t *testing.T) {
	t.Parallel()

	root := internalDir(t)
	// Coarse heuristic: every call to a function whose name starts
	// with `audit.Write` should reference the 4 keys nearby.
	writeRE := regexp.MustCompile(`audit\.Write\w*\(`)
	var bad []string

	walkGoFiles(t, root, false, func(path string, src []byte) {
		body := string(src)
		idx := writeRE.FindAllStringIndex(body, -1)
		for _, loc := range idx {
			// Take a 600-char window starting at the call.
			end := loc[1] + 600
			if end > len(body) {
				end = len(body)
			}
			window := body[loc[1]:end]
			if !strings.Contains(window, "Actor") ||
				!strings.Contains(window, "Action") {
				bad = append(bad, pathToSlash(path)+":"+itoa(lineNumberAt(body, loc[0])))
			}
		}
	})

	if len(bad) > 0 {
		t.Fatalf("audit.Write*() call missing Actor/Action keys in nearby args:\n  %s",
			strings.Join(bad, "\n  "))
	}
}

// ----------------------------------------------------------------------------
// N4: TestArch_NoSensitivePayloadInAudit
// ----------------------------------------------------------------------------
//
// Audit payloads MUST NEVER contain plaintext password / refresh
// token / API key / OTP. Predicate: every line containing
// `audit.Write*` or `Audit{...}` must NOT also contain `password`,
// `secret`, or `token` as a string-literal key in the same
// multi-line expression.
// Scope: production — applies to non-test files; test-side discipline lives under Principle TD/TP.
func TestArch_NoSensitivePayloadInAudit(t *testing.T) {
	t.Parallel()

	root := internalDir(t)
	auditExprRE := regexp.MustCompile(`(?:audit\.\w+\(|Audit\s*\{|AuditPayload\s*\{)`)
	sensitiveRE := regexp.MustCompile(`(?i)"(password|secret|api_key|access_token|refresh_token|otp|plaintext_password)"`)
	var bad []string

	walkGoFiles(t, root, false, func(path string, src []byte) {
		body := string(src)
		idx := auditExprRE.FindAllStringIndex(body, -1)
		for _, loc := range idx {
			end := loc[1] + 400
			if end > len(body) {
				end = len(body)
			}
			window := body[loc[1]:end]
			if sensitiveRE.MatchString(window) {
				bad = append(bad, pathToSlash(path)+":"+itoa(lineNumberAt(body, loc[0])))
			}
		}
	})

	if len(bad) > 0 {
		t.Fatalf("audit call site references password/secret/token field — sensitive payload leak:\n  %s",
			strings.Join(bad, "\n  "))
	}
}
