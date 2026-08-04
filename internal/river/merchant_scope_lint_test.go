package riverjobs

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// FC-16 (or#862, extended or#877): every River worker that reaches a
// merchant-owned table must open a merchant-scoped connection or transaction —
// and must not read one on the bare job context on the way there.
//
// This lint exists because the failure it prevents is SILENT. A worker that
// touches a policied table on the bare River job context does not error — under
// the production openrails_app role its reads return zero rows and its writes
// are denied 42501. That is how the provider-intent executor plane (or#862),
// the credit-lot expiry sweep (or#868 B1) and six more workers (or#877) all
// shipped, passed their tests, and processed nothing.
//
// Two rules, checked separately:
//
//	R1 (whole-worker): a worker's Work method must open a merchant scope itself,
//	   or reach one through a helper in THIS package — or DECLARE below where the
//	   scope is opened. Cross-package delegation is not followed by the AST,
//	   deliberately: "the module service handles it" is exactly the belief that
//	   produced or#862, so it has to be written down and checkable by a human.
//
//	R2 (unscoped read, or#877): on the UNSCOPED part of a Work path — the code
//	   that runs before/outside any RunInMerchantConn/RunInMerchantScope/
//	   MerchantTx closure — the only sanctioned sqlc accessor is GenDirectory()
//	   (the four policy-free tables plus the SECURITY DEFINER work queues).
//	   A `.Gen(ctx)` / `.Qx(ctx)` out there is the or#877 B4/B6 shape: a worker
//	   that DOES scope some of its work, and reads its work list on the bare
//	   context first, so the scoped part never has anything to do.
//
// KNOWN LIMIT: R2 sees sqlc accessors, not module services. A worker that hands
// the bare job context to a cross-package repo (`subs.NewRepo(w.DB).ListDue(ctx…)`
// — or#877 B5's exact shape) still passes both rules. db.AssertMerchantScope at
// the call site that matters is the guard for that one, and RunInMerchantScope
// makes it automatic for any pass converted to the work-queue shape.
//
// It is a plain unit test (no build tag) so it runs on every `go test ./...`.
var workersWithoutMerchantScope = map[string]string{
	// --- verified: the scope is opened one call out of this package ---
	"AlertEvalWorker":   "alerting.Service.EvaluateMerchant opens RunInMerchantConn per merchant (evaluator.go)",
	"SolanaCrankWorker": "solanasubs.SolanaSubscriptionRepo.ListDue fans out per merchant under RunInMerchantConn",
	"SolanaGasAlertWorker": "solanasubs.SolanaSubscriptionRepo.ListActiveMerchantWallets fans out per merchant under " +
		"RunInMerchantConn",
	"SolanaReconcileWorker": "solanasubs.SolanaSubscriptionRepo.ListActiveWithSignature fans out per merchant under " +
		"RunInMerchantConn",
	"PlanMigrationRedriveWorker": "subscriptions.PlanMigrationService.RedriveBlocked walks 0022's work queue under " +
		"RunInMerchantScope (or#861)",

	// --- verified: touches no merchant-owned table ---
	"CreditReconcileWorker":   "MoneyService.Reconcile is a stub in the Redis hold model — it reads nothing",
}

// scopers are the calls that pin app.merchant_id for the work inside them.
var scopers = []string{"RunInMerchantConn", "RunInMerchantScope", "MerchantTx", "WithMerchantConn"}

// unscopedAccessors are the sqlc handles that resolve to the BASE pool when no
// connection is pinned. GenDirectory is deliberately absent: it is the one
// accessor whose name says "policy-free tables and definers only".
var unscopedAccessors = []string{"Gen", "Qx"}

func TestEveryRiverWorkerOpensAMerchantScope(t *testing.T) {
	report := scanWorkersForMerchantScope(t, ".")

	if len(report.unscopedWorkers) > 0 {
		sort.Strings(report.unscopedWorkers)
		t.Errorf(`FC-16 R1: these River workers reach merchant-owned data without opening a merchant scope:

  %s

There is no privileged pool. On the bare River job context a policied read
returns ZERO ROWS AND NO ERROR and a policied write is denied 42501 — the worker
appears healthy and does nothing (or#862, or#868, or#877).

Fix it one of two ways:
  * enumerate the merchants with work through a SECURITY DEFINER work queue
    (migrations 0021/0022/0023 — ids only) and run each merchant's pass inside
    db.RunInMerchantScope; or
  * if the worker genuinely touches only merchants / probe_verdicts /
    worker_health / destructive_action_switch, add it to
    workersWithoutMerchantScope WITH the reason.`, strings.Join(report.unscopedWorkers, "\n  "))
	}

	if len(report.unscopedReads) > 0 {
		sort.Strings(report.unscopedReads)
		t.Errorf(`FC-16 R2: these River workers query on the BARE job context before/outside any merchant scope:

  %s

This is the or#877 shape: the worker does open a scope, but reads the work list
that feeds it on the base pool first — where a policied table matches
merchant_id = NULL, so the scoped part never has anything to do and the pass
reports success.

Outside a merchant scope the only sanctioned accessor is DB.GenDirectory():
the four policy-free tables (merchants, probe_verdicts, worker_health,
destructive_action_switch) and the SECURITY DEFINER work queues, which RAISE
instead of silently answering nothing.`, strings.Join(report.unscopedReads, "\n  "))
	}

	// A stale exemption is its own bug: it hides a rule that no longer applies.
	for worker := range workersWithoutMerchantScope {
		if !report.seen[worker] {
			t.Errorf("FC-16: %q is exempted but no longer has a Work method here; delete the exemption", worker)
		}
	}
}

// TestMerchantScopeLintCatchesANewInertWorker is the lint's own proof. A guard
// against a silent failure is worthless if it silently stops guarding, so both
// rules are exercised against a synthetic package containing exactly the two
// shapes this issue is about.
func TestMerchantScopeLintCatchesANewInertWorker(t *testing.T) {
	dir := t.TempDir()
	src := `package riverjobs

import "context"

// Never scopes anything: the whole-worker miss (or#862 / or#868 B1).
func (w BrandNewInertWorker) Work(ctx context.Context, job *river.Job[A]) error {
	rows, err := w.DB.Gen(ctx).ListSomethingPolicied(ctx)
	_ = rows
	return err
}

// Scopes the WORK but reads its work list on the bare context (or#877 B4/B6).
func (w BrandNewHalfScopedWorker) Work(ctx context.Context, job *river.Job[A]) error {
	targets, err := w.DB.Qx(ctx).Query(ctx, "SELECT id FROM openrails.psps")
	if err != nil {
		return err
	}
	for _, t := range targets {
		if err := w.DB.RunInMerchantScope(ctx, t.MerchantID, "op", func(mctx context.Context) error {
			_, err := w.DB.Gen(mctx).DoTheWork(mctx)
			return err
		}); err != nil {
			return err
		}
	}
	return nil
}

// The correct shape: directory/definer read outside, everything else inside.
func (w BrandNewCorrectWorker) Work(ctx context.Context, job *river.Job[A]) error {
	ids, err := w.DB.GenDirectory().ListActiveMerchantIDs(ctx)
	if err != nil {
		return err
	}
	for _, id := range ids {
		if err := w.DB.RunInMerchantScope(ctx, id, "op", func(mctx context.Context) error {
			_, err := w.DB.Gen(mctx).DoTheWork(mctx)
			return err
		}); err != nil {
			return err
		}
	}
	return nil
}
`
	if err := os.WriteFile(filepath.Join(dir, "jobs_synthetic.go"), []byte(src), 0o600); err != nil {
		t.Fatalf("write synthetic worker: %v", err)
	}

	report := scanWorkersForMerchantScope(t, dir)
	if !contains(report.unscopedWorkers, "BrandNewInertWorker") {
		t.Errorf("R1 did not catch a worker that never opens a merchant scope: %v", report.unscopedWorkers)
	}
	if !contains(report.unscopedReads, "BrandNewHalfScopedWorker") {
		t.Errorf("R2 did not catch a worker reading its work list on the bare job context: %v", report.unscopedReads)
	}
	if contains(report.unscopedWorkers, "BrandNewCorrectWorker") || contains(report.unscopedReads, "BrandNewCorrectWorker") {
		t.Errorf("the sanctioned directory-then-scope shape must pass: %v / %v", report.unscopedWorkers, report.unscopedReads)
	}
}

func contains(entries []string, worker string) bool {
	for _, e := range entries {
		if strings.HasPrefix(e, worker+" (") || e == worker {
			return true
		}
	}
	return false
}

type merchantScopeReport struct {
	seen            map[string]bool
	unscopedWorkers []string
	unscopedReads   []string
}

// scanWorkersForMerchantScope applies R1 and R2 to every `Work` method in dir.
func scanWorkersForMerchantScope(t *testing.T, dir string) merchantScopeReport {
	t.Helper()
	fset := token.NewFileSet()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}

	report := merchantScopeReport{seen: map[string]bool{}}

	// Intra-package helper bodies, so a Work method that delegates to a helper in
	// this package still counts as scoped. Cross-package delegation is NOT
	// followed: a worker that hands the job to a module service must say so in
	// workersWithoutMerchantScope, naming where the scope is opened.
	pkgFuncs := map[string]*ast.FuncDecl{}
	var files []string
	for _, e := range entries {
		n := e.Name()
		if e.IsDir() || !strings.HasSuffix(n, ".go") || strings.HasSuffix(n, "_test.go") {
			continue
		}
		files = append(files, n)
		f, err := parser.ParseFile(fset, filepath.Join(dir, n), nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", n, err)
		}
		for _, d := range f.Decls {
			if fd, ok := d.(*ast.FuncDecl); ok && fd.Body != nil {
				pkgFuncs[fd.Name.Name] = fd
			}
		}
	}

	for _, name := range files {
		file, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, parser.ParseComments)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Name.Name != "Work" || fn.Recv == nil || len(fn.Recv.List) == 0 || fn.Body == nil {
				continue
			}
			worker := receiverTypeName(fn.Recv.List[0].Type)
			if !strings.HasSuffix(worker, "Worker") {
				continue
			}
			report.seen[worker] = true
			if _, exempt := workersWithoutMerchantScope[worker]; exempt {
				continue
			}
			if !opensScope(fn.Body, pkgFuncs, scopers, 4, map[string]bool{}) {
				report.unscopedWorkers = append(report.unscopedWorkers, worker+" ("+name+")")
				// R1 already fails it; R2 would just repeat the finding.
				continue
			}
			if readsOnBareContext(fn.Body, pkgFuncs, 4, map[string]bool{}) {
				report.unscopedReads = append(report.unscopedReads, worker+" ("+name+")")
			}
		}
	}
	return report
}

// opensScope reports whether body calls a merchant-scoping helper, following
// calls to functions declared in this package up to depth levels.
func opensScope(body *ast.BlockStmt, pkgFuncs map[string]*ast.FuncDecl, scopers []string, depth int, visiting map[string]bool) bool {
	if body == nil || depth <= 0 {
		return false
	}
	found := false
	var callees []string
	ast.Inspect(body, func(n ast.Node) bool {
		switch v := n.(type) {
		case *ast.SelectorExpr:
			for _, s := range scopers {
				if v.Sel.Name == s {
					found = true
				}
			}
			callees = append(callees, v.Sel.Name)
		case *ast.Ident:
			callees = append(callees, v.Name)
		}
		return true
	})
	if found {
		return true
	}
	for _, c := range callees {
		fd, ok := pkgFuncs[c]
		if !ok || visiting[c] {
			continue
		}
		visiting[c] = true
		if opensScope(fd.Body, pkgFuncs, scopers, depth-1, visiting) {
			return true
		}
	}
	return false
}

// readsOnBareContext reports whether body reaches Gen()/Qx() OUTSIDE any
// scoping closure. Function literals passed to a scoper are skipped whole (what
// runs inside them is pinned by definition), and intra-package helpers are
// followed only when they are called from the unscoped region — a helper called
// from inside a scope inherits it.
func readsOnBareContext(body *ast.BlockStmt, pkgFuncs map[string]*ast.FuncDecl, depth int, visiting map[string]bool) bool {
	if body == nil || depth <= 0 {
		return false
	}
	found := false
	var unscopedCallees []string
	var walk func(n ast.Node) bool
	walk = func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		for _, s := range scopers {
			if sel.Sel.Name == s {
				// Everything inside the scoping call is pinned; do not descend
				// into its closure arguments. The receiver expression itself is
				// still worth walking (it is evaluated unscoped).
				ast.Inspect(sel.X, walk)
				return false
			}
		}
		for _, a := range unscopedAccessors {
			if sel.Sel.Name == a {
				found = true
			}
		}
		unscopedCallees = append(unscopedCallees, sel.Sel.Name)
		if id, ok := call.Fun.(*ast.Ident); ok {
			unscopedCallees = append(unscopedCallees, id.Name)
		}
		return true
	}
	ast.Inspect(body, func(n ast.Node) bool {
		if call, ok := n.(*ast.CallExpr); ok {
			if id, ok := call.Fun.(*ast.Ident); ok {
				unscopedCallees = append(unscopedCallees, id.Name)
			}
		}
		return walk(n)
	})
	if found {
		return true
	}
	for _, c := range unscopedCallees {
		fd, ok := pkgFuncs[c]
		if !ok || visiting[c] {
			continue
		}
		visiting[c] = true
		if readsOnBareContext(fd.Body, pkgFuncs, depth-1, visiting) {
			return true
		}
	}
	return false
}

func receiverTypeName(expr ast.Expr) string {
	switch t := expr.(type) {
	case *ast.Ident:
		return t.Name
	case *ast.StarExpr:
		return receiverTypeName(t.X)
	}
	return ""
}
