// orders_arch_test.go — Orders module fitness functions per ADR 0063 §"Fitness function".
//
// ADR 0063 declares two mechanical assertions specific to the Orders
// module that ride alongside the generic principle-tests (Layer 1).
// Both are catalog-named (`TestArch_Orders*`) so [TestMeta_EveryAcceptedADRHasFitnessFunctionRef]
// resolves the ADR's references.
//
// Tests in this file:
//   - TestArch_OrdersAggregatesDoNotImportPgx       (ADR 0063 fitness #1)
//   - TestArch_OrdersInvoiceNumberHasGaplessAllocation (ADR 0063 fitness #2)
//
// The second test is a FORWARD GATE: the file it inspects
// (`invoice_number_repository_pg.go`) lands in the B.4 adapter slice;
// until then the test passes vacuously. The gate prevents a future
// committer from accidentally using `nextval()` on a Postgres SEQUENCE
// (which would burn numbers on tx rollback → gaps → GSTR-1 audit fail).
//
//nolint:dupword,revive // arch tests intentionally have catalog-style headers

package architecture_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ----------------------------------------------------------------------------
// TestArch_OrdersAggregatesDoNotImportPgx
// ----------------------------------------------------------------------------
//
// `internal/orders/domain/**` (every aggregate package) MUST NOT import:
//   - github.com/jackc/pgx/v5
//   - github.com/jackc/pgx/v5/pgxpool
//   - github.com/jackc/pgx/v5/pgtype
//   - internal/orders/adapters/db (the sqlc-generated package)
//
// Per ADR 0047 layer-boundary discipline + ADR 0063 §1: the Orders
// domain layer is pure Go + identity-domain imports; the driver
// vocabulary belongs in adapters/.
//
// Complements the generic [TestArch_NoDBTypesInDomainSignatures] —
// that one catches pgx-typed signatures across all modules; this one
// catches Orders-specific imports including the sqlc-generated
// adapters/db carve-out.
//
// Scope: production — domain layer imports are a production-only
// concern. Test files under orders/domain are allowed to import
// fakes from sibling <aggregate>test/ packages per TD/TP test-pyramid
// canon (ADR 0062).
//
// arch-test:no-negative-fixture — the assertion target is a
// production-code import statement (Orders domain → pgx). Creating a
// fixture file that imports pgx in `internal/orders/domain/...` would
// itself violate the rule; the test would then catch its own fixture
// and the fixture would need to be ignored, defeating the purpose.
// The rule's negative case is the very thing the test prevents.
func TestArch_OrdersAggregatesDoNotImportPgx(t *testing.T) {
	t.Parallel()

	banned := []string{
		"github.com/jackc/pgx",
		"github.com/leadkart/leadkart-go/internal/orders/adapters",
	}

	ordersDomainDir := filepath.Join(internalDir(t), "orders", "domain")
	if _, err := os.Stat(ordersDomainDir); os.IsNotExist(err) {
		t.Skip("internal/orders/domain not yet present")
	}

	type violation struct {
		file string
		imp  string
	}
	var violations []violation

	walkGoFiles(t, ordersDomainDir, false, func(path string, src []byte) {
		for _, imp := range parseImports(t, path, src) {
			for _, b := range banned {
				if strings.HasPrefix(imp, b) {
					violations = append(violations, violation{file: path, imp: imp})
				}
			}
		}
	})

	if len(violations) > 0 {
		t.Logf("ORDERS-DOMAIN-IMPORT VIOLATIONS — %d", len(violations))
		t.Logf("Per ADR 0047 + ADR 0063 §1: orders/domain MUST stay pure Go +")
		t.Logf("identity-domain imports only. Driver vocabulary + sqlc-generated")
		t.Logf("code belong in orders/adapters.")
		for _, v := range violations {
			t.Errorf("%s — forbidden import %q", v.file, v.imp)
		}
	}
}

// ----------------------------------------------------------------------------
// TestArch_OrdersInvoiceNumberHasGaplessAllocation
// ----------------------------------------------------------------------------
//
// `internal/orders/adapters/invoice_number_repository_pg.go` (the pgx-
// backed [invoicenumber.Allocator] impl) MUST allocate via row-UPDATE-
// RETURNING inside a UoW closure. It MUST NOT use Postgres `nextval()`
// on a SEQUENCE — those are non-transactional and burn numbers on
// rollback, breaking the GSTR-1 gapless invoice-number rule.
//
// Per ADR 0063 §3 + BRD §A-014.
//
// FORWARD GATE: file doesn't exist on this branch yet (lands in B.4
// adapter slice). The test passes vacuously today; once the file
// lands, the rule kicks in automatically — no follow-up wiring needed.
// This is the canonical "future-proofing arch test" shape used
// elsewhere in this catalogue (cf. [TestMeta_EveryFitnessFunctionHasNegativeFixture]).
//
// arch-test:no-negative-fixture — the assertion target is the SQL
// shape of a SINGLE specific production file (invoice_number_repository_pg.go).
// A negative fixture would mean creating a sibling file with the
// forbidden `nextval(` shape — but the path-anchored matcher only
// looks at the one canonical file, so a sibling fixture wouldn't
// trigger the rule. The forward-gate shape (file absent today,
// kicks in when it lands) is the canonical "single-file path-anchored
// production rule" pattern.
func TestArch_OrdersInvoiceNumberHasGaplessAllocation(t *testing.T) {
	t.Parallel()

	path := filepath.Join(internalDir(t), "orders", "adapters", "invoice_number_repository_pg.go")
	raw, err := os.ReadFile(path) //nolint:gosec // arch-test fixture path
	if os.IsNotExist(err) {
		// Forward gate — adapter not yet present; test passes vacuously.
		// Once the file lands, the rules kick in.
		return
	}
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	body := string(raw)

	// Negative rule — no nextval() / no SEQUENCE allocation.
	bannedShapes := []string{
		"nextval(",
		"CREATE SEQUENCE",
		"CREATE TEMP SEQUENCE",
	}
	for _, b := range bannedShapes {
		if strings.Contains(body, b) {
			t.Errorf("%s contains forbidden %q — gapless allocation MUST use row-UPDATE-RETURNING per ADR 0063 §3, NOT a SEQUENCE (non-transactional → gaps on rollback → GSTR-1 audit fail)", filepath.Base(path), b)
		}
	}

	// Positive rule — the row-UPDATE-RETURNING shape must be present.
	requiredShapes := []string{
		"UPDATE",
		"RETURNING",
	}
	for _, r := range requiredShapes {
		if !strings.Contains(body, r) {
			t.Errorf("%s missing required SQL shape %q — gapless allocation per ADR 0063 §3 uses `UPDATE invoice_number_sequences SET last_used = last_used + 1 RETURNING last_used` inside a UoW closure", filepath.Base(path), r)
		}
	}
}
