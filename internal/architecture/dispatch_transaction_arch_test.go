// dispatch_transaction_arch_test.go — fitness function for ADR 0072: the dispatch of a
// consignment note is ONE database transaction.
//
// THE TRAP — pg.Transactor.WithinTx and WithinTenantTx always BEGIN a new transaction; they
// never join one already in ctx (the repositories do). A step of the dispatch that wrapped
// itself in either would silently open a second transaction on a second connection: not
// atomic, and a deadlock on a small pool. The behavioural proofs
// (cmd/api/dispatch_transaction_e2e_test.go, notably the one-connection pool and the failure
// injected at each step) catch it at run time; this gate catches the shape at review time.
//
// The invariants, each one a string the files must (not) contain:
//   - the Inventory shipment step and the Orders dispatch step open no transaction of their own
//     (no WithinTx / WithinTenantTx / UnitOfWork in their code);
//   - the Dispatch handler opens exactly the one tenant transaction (WithinTenantTx);
//   - both composition-root bridges fail closed when ctx carries no transaction
//     (pg.TxFromContext checked in each step).
//
// arch-test:no-negative-fixture — the assertion target is the text of four production files;
// a fixture that opened a nested transaction in one of them would itself break the build of
// the module it sits in.
//
// arch-test:no-synctest — purely-static analysis test; no goroutines, no time-bound, no DB.
//
//nolint:dupword,revive // arch tests intentionally have catalog-style headers

package architecture_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// between returns the text of src from the first occurrence of from up to the next occurrence
// of to after it (or the end of src when to is empty or absent).
func between(src, from, to string) (string, bool) {
	i := strings.Index(src, from)
	if i < 0 {
		return "", false
	}
	rest := src[i:]
	if to == "" {
		return rest, true
	}
	if j := strings.Index(rest[len(from):], to); j >= 0 {
		return rest[:len(from)+j], true
	}
	return rest, true
}

// TestArch_DispatchStepsJoinTheDispatchTransaction gates the invariants listed in the file
// header (ADR 0072).
//
// arch-test:no-negative-fixture — the assertion target is the text of four production files;
// a fixture that opened a nested transaction in one of them would itself break the build of
// the module it sits in.
func TestArch_DispatchStepsJoinTheDispatchTransaction(t *testing.T) {
	t.Parallel()
	root := repoRoot(t)
	read := func(rel string) string {
		t.Helper()
		b, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
		if err != nil {
			t.Fatalf("read %s: %v", rel, err)
		}
		return strings.ReplaceAll(string(b), "\r\n", "\n")
	}
	opensTx := []string{"WithinTx(", "WithinTenantTx(", "WithinTxPgx(", "WithinTxPgxTenant(", "pg.UnitOfWork", "pg.TenantUnitOfWork"}

	// The Inventory shipment step: no transaction of its own.
	ship := read("internal/inventory/app/command/ship_order_stock.go")
	for _, bad := range opensTx {
		if strings.Contains(ship, bad) {
			t.Errorf("internal/inventory/app/command/ship_order_stock.go uses %s: the shipment is a step of the dispatch transaction and must use the one in ctx (ADR 0072)", bad)
		}
	}

	// The Orders dispatch step: no transaction of its own.
	orders := read("internal/orders/app/command/order_commands.go")
	step, ok := between(orders, "// ----- DispatchOrder (invoiced → dispatched)", "// ----- MarkDelivered")
	if !ok {
		t.Fatal("internal/orders/app/command/order_commands.go: the DispatchOrder section was not found")
	}
	for _, bad := range opensTx {
		if strings.Contains(step, bad) {
			t.Errorf("internal/orders/app/command/order_commands.go (DispatchOrder) uses %s: the order step runs inside the dispatch transaction and must use the one in ctx (ADR 0072)", bad)
		}
	}

	// The Dispatch handler: exactly one tenant transaction.
	dispatch := read("internal/dispatch/app/command/status_transitions.go")
	handler, ok := between(dispatch, "func (h MarkDispatchedHandler) Handle(", "// ----- MarkInTransit")
	if !ok {
		t.Fatal("internal/dispatch/app/command/status_transitions.go: MarkDispatchedHandler.Handle was not found")
	}
	if n := strings.Count(handler, "WithinTenantTx("); n != 1 {
		t.Errorf("MarkDispatchedHandler.Handle opens %d tenant transactions; want exactly 1 (ADR 0072)", n)
	}

	// Both bridges fail closed without a transaction in ctx.
	bridge := read("cmd/internal/bridges/dispatch_shipment.go")
	if n := strings.Count(bridge, "pg.TxFromContext(ctx)"); n < 2 {
		t.Errorf("cmd/internal/bridges/dispatch_shipment.go checks pg.TxFromContext %d time(s); want a check in BOTH steps so neither runs outside the dispatch transaction (ADR 0072)", n)
	}
}

// TestArch_DispatchPathRepositoriesJoinTheTransactionInCtx gates the root of the trap: a
// repository function on the dispatch path that opens a transaction (WithinTxPgx,
// WithinTxPgxTenant) must first join the one in ctx (pg.TxFromContext), or use
// Transactor.JoinTxPgxTenant. A READ that always began its own transaction (as the batch,
// order and movement GetByID once did) would, called from a dispatch step, take a second
// connection and read outside the dispatch transaction.
//
// What the gate cannot see: whether a step calls such a function at all, or a repository on
// the path that is not in the list below. The behavioural proof that the dispatch completes on a
// ONE-connection pool (cmd/api, proof 8) is the guard for those.
//
// arch-test:no-negative-fixture — the assertion target is the text of production files; a
// fixture repository with a non-joining read would itself be a trap on the dispatch path.
func TestArch_DispatchPathRepositoriesJoinTheTransactionInCtx(t *testing.T) {
	t.Parallel()
	root := repoRoot(t)
	files := []string{
		"internal/dispatch/adapters/consignment_note_repository_pg.go",
		"internal/orders/adapters/order_repository_pg.go",
		"internal/inventory/adapters/batch_repository_pg.go",
		"internal/inventory/adapters/stock_movement_repository_pg.go",
		"internal/inventory/adapters/policy_replica_repository_pg.go",
	}
	for _, rel := range files {
		src, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
		if err != nil {
			t.Fatalf("read %s: %v", rel, err)
		}
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, rel, src, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", rel, err)
		}
		for _, decl := range f.Decls {
			fd, ok := decl.(*ast.FuncDecl)
			if !ok || fd.Body == nil {
				continue
			}
			body := string(src[fset.Position(fd.Body.Pos()).Offset:fset.Position(fd.Body.End()).Offset])
			if strings.Contains(body, ".WithinTxPgx") && !strings.Contains(body, "TxFromContext") {
				t.Errorf("%s: %s opens a transaction without joining the one in ctx first (pg.TxFromContext or Transactor.JoinTxPgxTenant); on the dispatch path that is a second connection outside the dispatch transaction (ADR 0072)", rel, fd.Name.Name)
			}
		}
	}
}
