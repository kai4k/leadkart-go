// audit_arch_test.go — Principle N: Audit log discipline.
//
// ADR 0027: the outbox doubles as the audit log. GDPR Art. 5 §1(f) + SOC 2 CC7.2.
// Gates: audit table append-only; writes carry Actor+Action; no sensitive payload.

package architecture_test

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
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
		"request_password_reset", "impersonation",
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

// auditDeleteRE finds a statement that removes the audit trail: DELETE FROM,
// TRUNCATE or DROP TABLE naming common.audit_log_entry (alone or in a list of tables), or
// DROP SCHEMA common, which takes it with it. It runs over [normaliseSQLSource] text,
// where quotes, string concatenation and SQL comments are gone.
//
// What a text gate cannot see, by design: a statement built with fmt.Sprintf or from a
// named constant, a table renamed before it is dropped, and dynamic SQL inside a function.
// Those are held back by the database, not by this scan: the application role has no
// DELETE, TRUNCATE or DROP on the table (pg.TablePrivilegeExceptions, the boot check).
var auditDeleteRE = regexp.MustCompile(`(?is)(` +
	`(DELETE\s+FROM|TRUNCATE(\s+TABLE)?|DROP\s+TABLE(\s+IF\s+EXISTS)?)\s+(ONLY\s+)?([\w.]+\s*,\s*)*(common\s*\.\s*)?audit_log_entry\b` +
	`|DROP\s+SCHEMA(\s+IF\s+EXISTS)?\s+([\w.]+\s*,\s*)*common\b)`)

// sqlCommentRE matches an SQL comment: a block comment, or a line comment that starts a
// line or follows whitespace (so a Go "n--" is left alone).
var sqlCommentRE = regexp.MustCompile(`(?s)/\*.*?\*/|(?m)(^|\s)--[^\n]*`)

// concatRE matches the join of two adjacent string literals in Go
// ("DELETE FROM " + "common.audit_log_entry", either quote kind, across lines).
var concatRE = regexp.MustCompile(`[\x22\x60]\s*\+\s*[\x22\x60]`)

// normaliseSQLSource undoes what hides a statement from a text scan: string
// concatenation and the quotes around identifiers ("common"."audit_log_entry",
// \"audit_log_entry\" inside a Go string).
func normaliseSQLSource(src string) string {
	src = concatRE.ReplaceAllString(src, "")
	src = strings.NewReplacer(`\"`, "", `"`, "", "`", "").Replace(src)
	return sqlCommentRE.ReplaceAllString(src, " ")
}

// upBlock is the part of a goose migration that runs on the way up. The Down block
// of an earlier migration may legitimately name a statement the Up block removed.
func upBlock(src string) string {
	up, _, _ := strings.Cut(src, "-- +goose Down")
	return up
}

// auditTrailDeleters returns the files under root (the repository root) that
// remove the audit trail: production Go under internal/ and cmd/, every .sql file
// there, and the Up block of every migration, which is the owner role's way in (the
// application role holds no DELETE on it, the owner role can).
func auditTrailDeleters(t *testing.T, root string) []string {
	t.Helper()
	var bad []string
	check := func(path string, src []byte) {
		if auditDeleteRE.MatchString(normaliseSQLSource(string(src))) {
			bad = append(bad, pathToSlash(path))
		}
	}
	for _, dir := range []string{filepath.Join(root, "internal"), filepath.Join(root, "cmd")} {
		walkGoFiles(t, dir, false, check)
		_ = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || filepath.Ext(path) != ".sql" {
				return nil
			}
			if src, rerr := os.ReadFile(path); rerr == nil {
				check(path, src)
			}
			return nil
		})
	}
	_ = filepath.WalkDir(filepath.Join(root, "migrations"), func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || filepath.Ext(path) != ".sql" {
			return nil
		}
		if src, rerr := os.ReadFile(path); rerr == nil {
			check(path, []byte(upBlock(string(src))))
		}
		return nil
	})
	return bad
}

// TestArch_NothingDeletesTheAuditTrail pins the owner rule "we never delete any data"
// (ADR 0071) for the audit trail: no production Go or SQL source, and no migration's
// Up block, deletes from, truncates or drops common.audit_log_entry, quoted or
// built by concatenation or not, so no scheduled job can either. The database side
// is the revoked DELETE grant (pg.TablePrivilegeExceptions) and the boot check.
// Scope: production, plus the Up block of migrations.
//
// arch-test:no-negative-fixture (the ten plants are in TestAuditDeleteGate_CatchesItsPlants, which runs this same scan over a scratch tree)
func TestArch_NothingDeletesTheAuditTrail(t *testing.T) {
	t.Parallel()

	if bad := auditTrailDeleters(t, repoRoot(t)); len(bad) > 0 {
		t.Fatalf("source deletes from the audit trail (it is kept forever, ADR 0071):\n  %s", strings.Join(bad, "\n  "))
	}
}

// TestAuditDeleteGate_CatchesItsPlants plants ten ways of deleting the audit
// trail in a scratch tree and requires the gate to name each; a REVOKE (what the
// migration that removed the grant says) and a Down block must stay quiet.
func TestAuditDeleteGate_CatchesItsPlants(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	write := func(rel, body string) {
		t.Helper()
		path := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	const pkg = "package x\n\nvar q = "
	plants := map[string]string{
		"internal/m/plain.go":                 pkg + "`DELETE FROM common.audit_log_entry WHERE true`\n",
		"internal/m/purge.sql":                "DELETE FROM common.audit_log_entry;\n",
		"migrations/20990101000001_purge.sql": "-- +goose Up\nDELETE FROM common.audit_log_entry WHERE created_at < now();\n-- +goose Down\nSELECT 1;\n",
		"cmd/x/quoted.go":                     pkg + "`DELETE FROM \"common\".\"audit_log_entry\"`\n",
		"internal/m/concat.go":                pkg + "\"DELETE FROM \" +\n\t\"common.\" + \"audit_log_entry\"\n",
		// Review G2c N6: the forms the first five missed.
		"internal/m/multitruncate.go":               pkg + "`TRUNCATE common.dead_letter_replay, common.audit_log_entry`\n",
		"internal/m/multidrop.go":                   pkg + "`DROP TABLE IF EXISTS common.outbox, common.audit_log_entry CASCADE`\n",
		"internal/m/dropschema.go":                  pkg + "`DROP SCHEMA common CASCADE`\n",
		"internal/m/blockcomment.go":                pkg + "`DELETE /* tidy up */ FROM common.audit_log_entry`\n",
		"migrations/20990101000004_linecomment.sql": "-- +goose Up\nDELETE -- old rows\n  FROM common.audit_log_entry;\n-- +goose Down\nSELECT 1;\n",
	}
	for rel, body := range plants {
		write(rel, body)
	}
	write("migrations/20990101000002_revoke.sql", "-- +goose Up\nREVOKE DELETE ON common.audit_log_entry FROM leadkart_app;\n-- +goose Down\nGRANT DELETE ON common.audit_log_entry TO leadkart_app;\n")
	write("migrations/20990101000003_old.sql", "-- +goose Up\nSELECT 1;\n-- +goose Down\nDELETE FROM common.audit_log_entry;\n")
	write("internal/m/ok.go", pkg+"`SELECT * FROM common.audit_log_entry`\n")
	write("internal/m/ok_comment.go", pkg+"`SELECT 1 -- DELETE FROM common.audit_log_entry`\n")
	write("internal/m/ok_schema.go", pkg+"`DROP SCHEMA tmp_common`\n")

	got := auditTrailDeleters(t, root)
	for rel := range plants {
		if !slices.ContainsFunc(got, func(g string) bool { return strings.HasSuffix(g, rel) }) {
			t.Errorf("the gate missed the plant %s", rel)
		}
	}
	if len(got) != len(plants) {
		t.Errorf("the gate named %d files, want exactly the %d plants: %v", len(got), len(plants), got)
	}
}
