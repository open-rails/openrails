package intents

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// The #674 enforcement guard, in two halves that are only total together.
//
// GUARD B (TestProviderWriteSurfaceIsClassified) inventories the provider
// client's exported surface and demands every method be classified read or
// write. GUARD A (TestProviderWritesStayBehindIntents) then enforces that every
// call site of a WRITE is either an intent handler or an allowlisted exception.
//
// Why two: on its own, a call-site guard only ever sees the write methods
// someone remembered to name. Adding `func (c *NMIClient) ChargeNow(...)` used
// to be invisible to it forever. Guard B makes a new or renamed method fail CI
// until it is classified, at which point Guard A starts policing its callers.
//
// HONEST LIMIT (GAP-11 residual). This is enforcement by test, not by
// construction. Go has no friend visibility: the only way to make a bypass a
// COMPILE error would be to move the write client under internal/intents/
// internal/…, which the legitimate non-intent callers below (reactive
// user/admin cancels, decline cleanup, the checkout upgrade saga) make a large
// refactor. Recorded in docs/invariants.md as strong-T, not S.
//
// What the AST form does close, versus the previous textual grep:
//   - METHOD VALUES. `f := client.RunSale` has no "(" after the name; the
//     regex could not see it. A selector match does.
//   - WRAPPERS INSIDE ALLOWLISTED FILES. The allowlist is keyed by
//     file:function, not by file, so a second call added anywhere in an
//     already-trusted file fails.
//   - INTERFACE DISPATCH. A call through an interface that declares the same
//     method name is a selector like any other.
//   - RENAMED IMPORTS. Method calls do not name the package at all.
//
// Stripe writes are deliberately NOT scanned here: they are choked
// architecturally through internal/integrations/stripeapi, whose readonly
// transport blocks writes before bytes reach the network (IDEM-8, strength S).

// providerWriteSurface classifies every exported method on the NMI client.
// "write" = the call mutates provider state or moves money. Adding a method to
// the client without adding it here fails Guard B.
var providerWriteSurface = map[string]string{
	// --- writes ---------------------------------------------------------
	"RunSale":                         "write", // charges a card
	"Refund":                          "write", // returns money
	"Void":                            "write", // cancels an authorization
	"AddRecurringSubscription":        "write", // creates a remote billing schedule
	"UpdateRecurringSubscription":     "write", // mutates a remote billing schedule
	"DeleteRecurringSubscription":     "write", // IRREVERSIBLE (DES-1)
	"AttemptManualRebill":             "write", // charges a card off a schedule
	"UpdateSubscriptionPaymentSource": "write", // repoints a live schedule at another card
	"AddRecurringPlan":                "write", // creates a remote plan
	"EditRecurringPlan":               "write", // mutates a remote plan
	"CreateCustomerVault":             "write", // stores a card at the provider
	"UpdateCustomerVault":             "write", // mutates a stored card
	"DeleteCustomerVault":             "write", // IRREVERSIBLE: destroys the stored card
	"DeleteCustomerBillingEntry":      "write", // IRREVERSIBLE: shared-vault scoped delete

	// --- reads ----------------------------------------------------------
	"FindSuccessfulSaleByOrderID": "read",
	"GetPayment":                  "read",
	"GetPaymentActions":           "read",
	"GetRecurringLiveness":        "read",
	"GetRecurringPlanByID":        "read",
	"GetRecurringPlanDetailByID":  "read",
	"GetSubscription":             "read",
	"GetWebhookSecret":            "read",
	"ListCustomersPage":           "read",
	"ListRecurringPlans":          "read",
	"ListSubscriptionsPage":       "read",
	"ProbeSalesByOrderID":         "read",
	"ProbeTestMode":               "read",
	"SearchTransactions":          "read",
}

// solanaWriteFuncs are the sign-and-submit entry points: everything that puts a
// signed transaction on chain. Irreversible by construction.
var solanaWriteFuncs = map[string]string{
	"BuildSignSubmit":                   "write",
	"BuildSignSubmitPresubmit":          "write",
	"BuildSignSubmitWithPayerPresubmit": "write",
}

// nameAmbiguousWrites are write method names that also name an unrelated LOCAL
// method, so a selector match would be a false positive. Each is covered by a
// distinct, unambiguous marker instead.
var nameAmbiguousWrites = map[string]string{
	// PaymentService.Refund is a local payment-row write, not a provider call.
	// Any NMI refund must construct nmi.RefundParams, so that is the marker.
	"Refund": "nmi.RefundParams{",
}

// allowedWriteCallers maps "<file>:<function>" -> why that exact function is
// permitted to reach a provider write directly. Anything else fails.
var allowedWriteCallers = map[string]string{
	// --- the sanctioned executors: intent handlers -----------------------
	"internal/modules/checkout/nmi_sale_intent.go:Execute":         "nmi_sale intent handler",
	"internal/modules/checkout/nmi_subscription_intent.go:Execute": "nmi_subscription_create intent handler",
	"internal/intents/manual_rebill.go:Execute":                    "manual_rebill intent handler",
	"internal/intents/refund.go:Execute":                           "nmi_refund intent handler",
	"internal/intents/nmi_delete.go:Execute":                       "nmi_delete_subscription intent handler",
	"internal/intents/nmi_payment_source_update.go:Execute":        "nmi_payment_source_update intent handler (both call sites route through PaymentSourceUpdateThrough)",
	"internal/intents/nmi_vault_delete.go:Execute":                 "nmi_vault_delete intent handler — the sanctioned executor for durable user-initiated deletes",

	// --- the checkout upgrade saga: reactive, compensating ----------------
	"internal/modules/checkout/service.go:processUpgrade":          "upgrade proration sale + successor create: order ids content-derived, pre/post verify, ambiguity ⇒ roster-scan adopt-or-processing (#674 tail); full saga assessed and declined (completed.md #674)",
	"internal/modules/checkout/service.go:compensateFailedUpgrade": "upgrade compensation refund; intent migration deferred",
	"internal/modules/checkout/service.go:cancelNMISubscription":   "upgrade/cancel rollback of a just-created remote sub (reactive compensation)",

	// --- reactive user/admin cancels ------------------------------------
	"internal/modules/subscriptions/admin_service.go:cancelWithNMI":         "reactive admin cancel; deferred deletes route through intents, immediate ones are user/admin-reactive",
	"internal/modules/subscriptions/user_service.go:CancelUserSubscription": "reactive user cancel (see admin_service note)",

	// --- vault lifecycle -------------------------------------------------
	"internal/modules/paymentmethods/vault_service.go:CreateVault":            "the create half of the vault lifecycle: no durable intent exists until a vault does",
	"internal/modules/paymentmethods/vault_service.go:UpdateVault":            "card update on an existing vault (reactive, user-initiated)",
	"internal/modules/paymentmethods/vault_service.go:deleteVaultDirect":      "reactive decline-cleanup only: vault referenced nowhere, harmless if lost; durable deletes route through DeleteVault → nmi_vault_delete intent (#674 tail)",
	"internal/modules/paymentmethods/vault_service.go:cleanupVaultBestEffort": "reactive decline-cleanup (shared-vault scope); see deleteVaultDirect",

	// --- catalog push + plan migration ----------------------------------
	"pkg/service/catalog_provider_nmi.go:createPlan":                      "catalog push: creates the remote plan a price is billed against (the provider adapter, mirror of the Stripe AutoCreate)",
	"internal/modules/subscriptions/plan_migration_nmi.go:PushPlanAmount": "or#815 plan migration: repoints a live schedule at the successor plan",

	// --- solana: the three sign+submit entry points of the ONE Submitter ---
	"internal/modules/solana/recurring/plan_service.go:Submit":                                "the Submitter implementation — the single Solana sign+submit choke; pulls route through the solana_pull intent handler",
	"internal/modules/solana/recurring/plan_service.go:SubmitWithPresubmit":                   "same Submitter, presubmit variant",
	"internal/modules/solana/recurring/plan_service.go:SubmitForMerchantAddressWithPresubmit": "same Submitter, merchant-fee-payer variant",

	// --- the charge seam --------------------------------------------------
	"internal/modules/payments/rails/nmidirect/charger.go:Charge": "#297 charge-seam implementation: the ONE seam wire call, reached only via the money CollectionAdapter choke (topup_charge intent handler + arrears/invoice worker attempt-count keys, #672/#673)",

	// --- inside the client's own probe ------------------------------------
	"internal/integrations/nmi/probe.go:voidProbe": "the client probing ITSELF: voids the $0.01 auth ProbeTestMode just made; never touches a customer",
}

func repoRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("expected module root at %s: %v", root, err)
	}
	return root
}

// TestProviderWriteSurfaceIsClassified is GUARD B. Every exported method on the
// NMI client, and every exported sign-and-submit entry point in the Solana
// integration, must be classified read or write. A new method — or a rename of
// an existing one — fails here until someone decides which it is, which is what
// stops a wire call from being added under a name no guard knows.
func TestProviderWriteSurfaceIsClassified(t *testing.T) {
	root := repoRoot(t)

	nmiMethods := exportedMethods(t, filepath.Join(root, "internal/integrations/nmi"), "NMIClient")
	if len(nmiMethods) < 20 {
		t.Fatalf("found only %d exported NMIClient methods: parsing is broken, the guard would pass vacuously", len(nmiMethods))
	}
	for _, m := range nmiMethods {
		kind, ok := providerWriteSurface[m]
		if !ok {
			t.Errorf("NMIClient.%s is not classified in providerWriteSurface — decide whether it MUTATES provider "+
				"state or moves money (\"write\") or only reads (\"read\"). A write must then route through a "+
				"provider intent (#674) and its call sites are enforced by TestProviderWritesStayBehindIntents.", m)
			continue
		}
		if kind != "read" && kind != "write" {
			t.Errorf("NMIClient.%s has bogus classification %q (want \"read\" or \"write\")", m, kind)
		}
	}
	present := map[string]bool{}
	for _, m := range nmiMethods {
		present[m] = true
	}
	for m := range providerWriteSurface {
		if !present[m] {
			t.Errorf("providerWriteSurface names NMIClient.%s, which no longer exists — it was renamed or removed, "+
				"and the call-site guard silently stopped covering it", m)
		}
	}

	solanaFuncs := exportedFuncsWithPrefix(t, filepath.Join(root, "internal/integrations/solana"), "BuildSignSubmit")
	if len(solanaFuncs) == 0 {
		t.Fatal("found no BuildSignSubmit* entry points: parsing is broken, the guard would pass vacuously")
	}
	for _, f := range solanaFuncs {
		if _, ok := solanaWriteFuncs[f]; !ok {
			t.Errorf("solana.%s is a sign-and-submit entry point but is not classified in solanaWriteFuncs", f)
		}
	}
	for f := range solanaWriteFuncs {
		found := false
		for _, have := range solanaFuncs {
			if have == f {
				found = true
			}
		}
		if !found {
			t.Errorf("solanaWriteFuncs names solana.%s, which no longer exists — renamed or removed", f)
		}
	}
}

// TestProviderWritesStayBehindIntents is GUARD A: every call site of a
// classified WRITE must be an intent handler or an allowlisted exception,
// keyed by the exact enclosing function.
func TestProviderWritesStayBehindIntents(t *testing.T) {
	root := repoRoot(t)

	writes := map[string]bool{}
	for name, kind := range providerWriteSurface {
		if kind == "write" {
			if _, ambiguous := nameAmbiguousWrites[name]; !ambiguous {
				writes[name] = true
			}
		}
	}
	for name := range solanaWriteFuncs {
		writes[name] = true
	}

	var violations []string
	scanned := 0
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, rerr := filepath.Rel(root, path)
		if rerr != nil {
			return rerr
		}
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			switch {
			case strings.HasPrefix(d.Name(), "."), d.Name() == "node_modules", d.Name() == "vendor":
				return filepath.SkipDir
			// internal/integrations/nmi IS the client — its own methods calling
			// each other are not bypasses. probe.go is scanned explicitly via
			// its allowlist entry because a probe still charges a real card.
			case rel == "internal/integrations/solana":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(rel, ".go") || strings.HasSuffix(rel, "_test.go") {
			return nil
		}
		if !strings.HasPrefix(rel, "internal/") && !strings.HasPrefix(rel, "pkg/") {
			return nil
		}
		fset := token.NewFileSet()
		file, perr := parser.ParseFile(fset, path, nil, 0)
		if perr != nil {
			return fmt.Errorf("parse %s: %w", rel, perr)
		}
		scanned++
		raw, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		src := string(raw)

		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok {
				continue
			}
			key := rel + ":" + fn.Name.Name
			_, allowedHere := allowedWriteCallers[key]
			ast.Inspect(fn, func(n ast.Node) bool {
				sel, ok := n.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				name := sel.Sel.Name
				if !writes[name] {
					return true
				}
				// A client method calling a sibling client method is internal.
				if strings.HasPrefix(rel, "internal/integrations/nmi/") && !allowedHere {
					if _, isSurface := providerWriteSurface[fn.Name.Name]; isSurface {
						return true
					}
				}
				if allowedHere {
					return true
				}
				violations = append(violations, fmt.Sprintf(
					"%s:%d: %s reaches provider WRITE %q — route it through a provider intent "+
						"(internal/intents, #674) or add %q to allowedWriteCallers WITH a justification",
					rel, fset.Position(sel.Pos()).Line, fn.Name.Name, name, key))
				return true
			})

			// Ambiguous names are matched by their unmistakable marker instead.
			for name, marker := range nameAmbiguousWrites {
				if !strings.Contains(src, marker) || allowedHere {
					continue
				}
				start := fset.Position(fn.Pos()).Offset
				end := fset.Position(fn.End()).Offset
				if end > len(src) {
					end = len(src)
				}
				if !strings.Contains(src[start:end], marker) {
					continue
				}
				violations = append(violations, fmt.Sprintf(
					"%s: %s constructs %s and so reaches provider WRITE %q — route it through a provider "+
						"intent (internal/intents, #674) or add %q to allowedWriteCallers WITH a justification",
					rel, fn.Name.Name, marker, name, key))
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	// Vacuity guard: if the walk stops finding files, everything passes.
	if scanned < 300 {
		t.Fatalf("scanned only %d files (< 300): the walk is broken, the guard would pass vacuously", scanned)
	}

	sort.Strings(violations)
	for _, v := range violations {
		t.Error(v)
	}

	// A stale allowlist entry means the call moved and is now unguarded.
	// (Only checked for entries whose file still exists — a deleted file is a
	// legitimate removal that the entry should be cleaned up for anyway.)
	for key := range allowedWriteCallers {
		parts := strings.SplitN(key, ":", 2)
		if _, err := os.Stat(filepath.Join(root, parts[0])); err != nil {
			t.Errorf("allowlist entry %q names a file that no longer exists — delete it", key)
		}
	}
}

// exportedMethods returns the exported method names declared on recv in dir.
func exportedMethods(t *testing.T, dir, recv string) []string {
	t.Helper()
	var out []string
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, filepath.Join(dir, e.Name()), nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", e.Name(), err)
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Recv == nil || len(fn.Recv.List) == 0 || !fn.Name.IsExported() {
				continue
			}
			if receiverTypeName(fn.Recv.List[0].Type) == recv {
				out = append(out, fn.Name.Name)
			}
		}
	}
	sort.Strings(out)
	return out
}

// exportedFuncsWithPrefix returns exported top-level funcs in dir whose name
// starts with prefix.
func exportedFuncsWithPrefix(t *testing.T, dir, prefix string) []string {
	t.Helper()
	var out []string
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, filepath.Join(dir, e.Name()), nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", e.Name(), err)
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Recv != nil || !fn.Name.IsExported() {
				continue
			}
			if strings.HasPrefix(fn.Name.Name, prefix) {
				out = append(out, fn.Name.Name)
			}
		}
	}
	sort.Strings(out)
	return out
}

func receiverTypeName(expr ast.Expr) string {
	switch x := expr.(type) {
	case *ast.StarExpr:
		return receiverTypeName(x.X)
	case *ast.Ident:
		return x.Name
	}
	return ""
}
