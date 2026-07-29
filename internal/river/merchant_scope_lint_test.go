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

// FC-16 (or#862): every River worker that reaches a merchant-owned table must
// open a merchant-scoped connection or transaction.
//
// This lint exists because the failure it prevents is SILENT. A worker that
// touches a policied table on the bare River job context does not error — under
// the production openrails_app role its reads return zero rows and its writes
// are denied 42501. That is how the provider-intent executor plane (or#862),
// the credit-lot expiry sweep (or#868 B1) and four more workers (or#877) all
// shipped, passed their tests, and processed nothing.
//
// The rule: a worker's Work method must open a merchant scope itself, or reach
// one through a helper in THIS package — or DECLARE below where the scope is
// opened. Cross-package delegation is not followed by the AST, deliberately:
// "the module service handles it" is exactly the belief that produced or#862,
// so it has to be written down and checkable by a human, not assumed.
//
// KNOWN LIMIT: this is a structural check, not a dataflow one. A worker that
// opens a scope for SOME of its work and reads a policied table outside it
// still passes — or#877 B4/B5/B6 are exactly that shape. The check catches the
// whole-worker miss (or#862, or#868 B1); the per-statement miss is what
// db.AssertMerchantScope is for, at the call site that matters.
//
// It is a plain unit test (no build tag) so it runs on every `go test ./...`.
var workersWithoutMerchantScope = map[string]string{
	// --- verified: the scope is opened one call out of this package ---
	"AlertEvalWorker":   "alerting.Service.Evaluate opens RunInMerchantConn per merchant (evaluator.go)",
	"SolanaCrankWorker": "solanasubs.SolanaSubscriptionRepo.ListDue fans out per merchant under RunInMerchantConn",
	"SolanaGasAlertWorker": "solanasubs.SolanaSubscriptionRepo.ListActiveMerchantWallets fans out per merchant under " +
		"RunInMerchantConn",
	"SolanaReconcileWorker": "solanasubs.SolanaSubscriptionRepo.ListActiveWithSignature fans out per merchant under " +
		"RunInMerchantConn",
	"PlanMigrationRedriveWorker": "subscriptions.PlanMigrationService.RedriveBlocked walks 0022's work queue under " +
		"RunInMerchantScope (or#861)",

	// --- verified: touches no merchant-owned table ---
	"CreditReconcileWorker":   "MoneyService.Reconcile is a stub in the Redis hold model — it reads nothing",
	"WorkerHealthCheckWorker": "openrails.worker_health only, RLS-exempt by design (operator telemetry, not merchant data)",

	// --- KNOWN INERT: filed, not hidden. Delete each entry as it is fixed. ---
	"ResumeSubscriptionWorker": "or#877 B7: cannot see its own subscription on the bare job context and drops a " +
		"user-requested resume",
	"CatalogReconciliationPullWorker": "INERT (this audit): catalog.ProductService/PriceService GetAll run s.db.Gen(ctx) " +
		"on the bare job context, so the alert-only pull reconciler compares an EMPTY local catalog against the " +
		"provider and reports nothing. Same fix shape as or#862; file with or#877",
	"FindingsDigestWorker": "INERT (this audit): alerting.Service.DigestFindings reads the watermark and the pending " +
		"findings with s.store on a merchant.WithID context only — a value, not a pinned connection — so the #787 " +
		"digest selects merchants and then digests nothing. File with or#877",
}

func TestEveryRiverWorkerOpensAMerchantScope(t *testing.T) {
	fset := token.NewFileSet()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}

	scopers := []string{"RunInMerchantConn", "RunInMerchantScope", "MerchantTx", "WithMerchantConn"}
	var offenders []string
	seen := map[string]bool{}

	// Intra-package helper bodies, so a Work method that delegates to a helper in
	// this package still counts as scoped. Cross-package delegation is NOT
	// followed: a worker that hands the job to a module service must say so in
	// workersWithoutMerchantScope, naming where the scope is opened.
	pkgFuncs := map[string]*ast.FuncDecl{}
	for _, e := range entries {
		n := e.Name()
		if e.IsDir() || !strings.HasSuffix(n, ".go") || strings.HasSuffix(n, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(".", n), nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", n, err)
		}
		for _, d := range f.Decls {
			if fd, ok := d.(*ast.FuncDecl); ok && fd.Body != nil {
				pkgFuncs[fd.Name.Name] = fd
			}
		}
	}

	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, filepath.Join(".", name), nil, parser.ParseComments)
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
			seen[worker] = true
			if _, exempt := workersWithoutMerchantScope[worker]; exempt {
				continue
			}
			if !opensScope(fn.Body, pkgFuncs, scopers, 4, map[string]bool{}) {
				offenders = append(offenders, worker+" ("+name+")")
			}
		}
	}

	if len(offenders) > 0 {
		sort.Strings(offenders)
		t.Fatalf(`FC-16: these River workers reach merchant-owned data without opening a merchant scope:

  %s

There is no privileged pool. On the bare River job context a policied read
returns ZERO ROWS AND NO ERROR and a policied write is denied 42501 — the worker
appears healthy and does nothing (or#862, or#868, or#877).

Fix it one of two ways:
  * enumerate the merchants with work through a SECURITY DEFINER work queue
    (migrations 0021/0022 — ids only) and run each merchant's pass inside
    db.RunInMerchantScope; or
  * if the worker genuinely touches only merchants / probe_verdicts /
    worker_health / destructive_action_switch, add it to
    workersWithoutMerchantScope WITH the reason.`, strings.Join(offenders, "\n  "))
	}

	// A stale exemption is its own bug: it hides a rule that no longer applies.
	for worker := range workersWithoutMerchantScope {
		if !seen[worker] {
			t.Errorf("FC-16: %q is exempted but no longer has a Work method here; delete the exemption", worker)
		}
	}
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

func receiverTypeName(expr ast.Expr) string {
	switch t := expr.(type) {
	case *ast.Ident:
		return t.Name
	case *ast.StarExpr:
		return receiverTypeName(t.X)
	}
	return ""
}
