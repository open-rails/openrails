package moneyutil_test

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

// MONEY-7, or#865.
//
// The register's old audit method was `grep -rln "wire pinning"`. It has no
// expected value: it prints the files that ARE pinned and says nothing about
// the ones that are not, so adding an unpinned money boundary changed the
// output from five lines to five lines. The list of pinned boundaries was
// hand-kept, which is fine — what was missing was anything that noticed the
// list going stale.
//
// This gives it an expected value. A "money boundary" is defined mechanically:
// a file that calls the internal→rail minor-unit converters, i.e. the exact
// point where an OpenRails integer amount becomes a number a provider sees.
// Every such file must be accounted for below — pinned, or explicitly deferred
// with a reason. A new one fails here until someone decides which.
//
// This does NOT check that a pinning test is any good; it checks that nobody
// can add a money boundary without the question being asked.

// converters are the money-boundary markers. moneyutil's own package is
// excluded (it IS the converter).
var converters = []string{
	"NativeToRailMinor",
	"NativeToRailMinorExact",
	"centsJSONAmount", // the NMI v5 JSON amount renderer
}

// pinnedBoundaries: file -> the wire-pinning test that covers it (known micros
// in ⇒ exact integer on the wire).
var pinnedBoundaries = map[string]string{
	"internal/integrations/nmi/payments.go":      "internal/integrations/nmi/payments_wire_test.go",
	"internal/integrations/nmi/v5.go":            "internal/integrations/nmi/payments_wire_test.go",
	"internal/integrations/nmi/subscriptions.go": "internal/integrations/nmi/recurring_plan_test.go",
	"internal/modules/webhooks/nmi.go":           "internal/modules/webhooks/nmi_test.go",
	"internal/modules/webhooks/ccbill.go":        "internal/modules/webhooks/ccbill_wire_pinning_test.go",
	"pkg/service/catalog_provider_stripe.go":     "internal/modules/subscriptions/stripe_wire_pinning_test.go",
	"pkg/service/catalog_provider_nmi.go":        "internal/integrations/nmi/recurring_plan_test.go",
}

// deferredBoundaries: file -> why it has no wire-pinning test yet. Recorded so
// the gate is honest from day one instead of being made green by suppression.
// Shrinking this map is the work; growing it needs a reason.
var deferredBoundaries = map[string]string{
	"internal/integrations/nmi/probe.go":                   "the $0.01 test-mode probe: a fixed literal amount, not a customer amount",
	"internal/modules/checkout/service.go":                 "upgrade proration + checkout sale — amounts reach the wire via nmi/payments.go, which IS pinned; the arithmetic above it is covered by unit tests, not a wire pin",
	"internal/modules/checkout/nmi_sale_intent.go":         "same: converts, then hands off to the pinned nmi client",
	"internal/modules/checkout/nmi_subscription_intent.go": "same",
	"internal/modules/checkout/custodian_sale.go":          "same",
	"internal/intents/topup_charge.go":                     "same: converts, then hands off through the charge seam",
	"internal/modules/money/arrears.go":                    "same",
	"internal/modules/subscriptions/plan_migration.go":     "or#815 plan migration: successor amount reaches NMI through the pinned client",
	"internal/modules/subscriptions/plan_migration_nmi.go": "same",
	"internal/http/handlers/admin_payments.go":             "admin-initiated refund amount; reaches the wire through the pinned nmi/stripe clients",
	"pkg/service/service_definition_catalog_admin.go":      "catalog definition admin; pushes through the pinned provider adapters",
}

// GAP-15 stays open and is deliberately NOT hidden in deferredBoundaries: the
// Solana Pay live formatter is a money boundary that this converter-based
// definition does not reach (Solana amounts are lamports, not a registered
// minor unit). Recorded in docs/invariants.md §10.

func TestEveryMoneyBoundaryIsPinnedOrDeferred(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", "..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("expected module root at %s: %v", root, err)
	}

	var found []string
	err = filepath.WalkDir(root, func(path string, d os.DirEntry, werr error) error {
		if werr != nil {
			return werr
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
			case rel == "internal/shared/moneyutil":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(rel, ".go") || strings.HasSuffix(rel, "_test.go") {
			return nil
		}
		fset := token.NewFileSet()
		f, perr := parser.ParseFile(fset, path, nil, 0)
		if perr != nil {
			return fmt.Errorf("parse %s: %w", rel, perr)
		}
		hit := false
		ast.Inspect(f, func(n ast.Node) bool {
			var name string
			switch node := n.(type) {
			case *ast.SelectorExpr:
				name = node.Sel.Name
			case *ast.Ident:
				name = node.Name
			default:
				return true
			}
			for _, c := range converters {
				if name == c {
					hit = true
					return false
				}
			}
			return true
		})
		if hit {
			found = append(found, rel)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	if len(found) < 10 {
		t.Fatalf("found only %d money boundaries: the walk is broken and this guard would pass vacuously", len(found))
	}

	var unaccounted []string
	for _, rel := range found {
		_, pinned := pinnedBoundaries[rel]
		_, deferred := deferredBoundaries[rel]
		if !pinned && !deferred {
			unaccounted = append(unaccounted, rel)
		}
	}
	sort.Strings(unaccounted)
	if len(unaccounted) > 0 {
		t.Errorf("these files convert an OpenRails amount into a provider minor unit but appear in neither "+
			"pinnedBoundaries nor deferredBoundaries (MONEY-7):\n  %s\n"+
			"Add a wire-pinning test (known micros in ⇒ exact integer on the wire) and register it, or record "+
			"why it is deferred. Do not delete this check to make it green.",
			strings.Join(unaccounted, "\n  "))
	}

	// The registries must not rot either: a file that no longer exists, or no
	// longer converts, is a claim of coverage that has silently stopped meaning
	// anything.
	present := map[string]bool{}
	for _, rel := range found {
		present[rel] = true
	}
	for _, registry := range []map[string]string{pinnedBoundaries, deferredBoundaries} {
		for rel := range registry {
			if !present[rel] {
				t.Errorf("MONEY-7 registry names %s, which is no longer a money boundary (moved, renamed, or it "+
					"stopped converting). Remove the entry so the list keeps meaning something.", rel)
			}
		}
	}

	// A named pinning test must actually exist.
	for rel, pin := range pinnedBoundaries {
		if _, serr := os.Stat(filepath.Join(root, pin)); serr != nil {
			t.Errorf("MONEY-7: %s is registered as pinned by %s, which does not exist: %v", rel, pin, serr)
		}
	}
}
