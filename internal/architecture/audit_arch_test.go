// audit_arch_test.go — Principle N: Audit log discipline.
//
// ADR 0027: the outbox doubles as the audit log. GDPR Art. 5 §1(f) + SOC 2 CC7.2.
// Gates: audit table append-only; writes carry Actor+Action; no sensitive payload.

package architecture_test

import (
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestArch_EveryAuthnEventAudited asserts auth-change handler files reference
// an integration event, outbox write, or state-mutation call.
// Scope: production — applies to non-test files; test-side discipline lives under Principle TD/TP.
func TestArch_EveryAuthnEventAudited(t *testing.T) {
	t.Parallel()

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

// TestArch_AuditLogIsAppendOnly asserts audit_log_entry has no UPDATE or DELETE
// RLS policies. An UPDATE policy lets a compromised service role edit history.
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

// TestArch_AuditEntryHasActorTargetActionResult asserts audit.Write* calls
// include Actor and Action within a 600-char window.
// Scope: production — applies to non-test files; test-side discipline lives under Principle TD/TP.
func TestArch_AuditEntryHasActorTargetActionResult(t *testing.T) {
	t.Parallel()

	root := internalDir(t)
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

// TestArch_NoSensitivePayloadInAudit asserts audit call sites don't include
// password/secret/token literal keys in a 400-char window.
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
