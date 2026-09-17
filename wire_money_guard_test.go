package openrails

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// moneyJSONName matches JSON names that carry native monetary units.
var moneyJSONName = regexp.MustCompile(`^units$|(^|_)(amount|amounts|price|limit|cap|threshold|floor|balance|revenue|fee|cost|tax|refunded|due|owed|spent|used|reserved|remaining|captured|authorized)(_|$)`)

// wireDTODirs hold types that the shared Client or HTTP handlers encode.
var wireDTODirs = []string{".", "pkg/api", "pkg/catalog", "internal/operator", "pkg/pricing", "internal/service", "internal/http/handlers", "internal/modules/copilot", "internal/modules/money", "internal/modules/metrics"}

// pendingNumericMoney lists the monetary integers still encoded as JSON
// numbers: every one is a type that never reaches the HTTP wire. The guard
// fails when a listed field becomes a decimal string or stops existing, so
// the list can only shrink.
var pendingNumericMoney = map[string]string{
	"internal/operator/fleet_analytics.go:FleetMerchantFunnel.ActiveRevenue active_revenue":                                                          notMoneyCount,
	"internal/operator/fleet_analytics.go:FleetMerchantFunnel.FirstRevenue first_revenue":                                                            notMoneyCount,
	"internal/modules/copilot/tools_draft.go:draftCatalogDiffArgs.UnitAmount unit_amount":                                                            notHTTPToolArgs,
	"internal/modules/copilot/tools_draft.go:draftPriceChangeArgs.NewAmount new_amount":                                                              notHTTPToolArgs,
	"internal/modules/money/enterprise.go:PendingCharge.Amount amount":                                                                               notHTTPPendingCharges,
	"internal/service/enterprise.go:PendingChargeDTO.Amount amount":                                                                                  notHTTPPendingCharges,
	"internal/modules/money/reconcile.go:OrphanedHold.Amount authorized_amount":                                                                      notHTTPReconcileReport,
	"internal/modules/money/provider_billing.go:normalizedProviderBillingRecord.AmountUSDMicros amount_usd_micros":                                   notHTTPProviderEvidence,
	"internal/modules/money/provider_billing.go:providerBillingSettlementManifest.QualifiedProviderCostUSDMicros qualified_provider_cost_usd_micros": notHTTPProviderEvidence,
	"internal/modules/money/service_usage.go:ResourceRevenueDailyRow.Amount amount":                                                                  notHTTPInternalRow,
	"internal/modules/money/service_usage.go:ServiceUsageRollupRow.TotalAmount total_amount":                                                         notHTTPInternalRow,
	"internal/modules/money/usage.go:UsageRollupRow.TotalAmount total_amount":                                                                        notHTTPInternalRow,
	"internal/service/spend.go:CreditAccountSnapshot.AvailableAmount available_amount":                                                               notHTTPInternalRow,
	"internal/service/spend.go:CreditAccountSnapshot.BalanceAmount balance_amount":                                                                   notHTTPInternalRow,
	"internal/service/spend.go:CreditAccountSnapshot.HeldAmount held_amount":                                                                         notHTTPInternalRow,
	"internal/service/spend.go:CreditAccountSnapshot.OutstandingOwedAmount outstanding_owed_amount":                                                  notHTTPInternalRow,
	"internal/service/host_events.go:func ListHostEvents.AmountFloor amount_floor":                                                                   notHTTPStoredPayload,
	"internal/service/host_events.go:func ListHostEvents.OverdueAmount overdue_amount":                                                               notHTTPStoredPayload,
	"internal/modules/money/invoice_collection_intent.go:InvoiceCollectionPayload.Amount amount":                                                     notHTTPIntentPayload,
}

const (
	notMoneyCount           = "not HTTP: merchant counts"
	notHTTPToolArgs         = "not HTTP: LLM tool-call arguments"
	notHTTPPendingCharges   = "not HTTP: no route serves pending charges"
	notHTTPReconcileReport  = "not HTTP: CLI/job reconcile report"
	notHTTPProviderEvidence = "not HTTP: provider billing evidence digest"
	notHTTPInternalRow      = "not HTTP: internal rows converted by internal/service"
	notHTTPStoredPayload    = "not HTTP: stored host_outbox payload decoded before the Client re-encodes it"
	// The invoice collection operation freezes its charge in a rail_intents
	// payload row under internal/modules/money, which this guard scans. It is
	// internal persisted intent data, never an HTTP body: an internal
	// exception, not permission to leave HTTP money numeric.
	notHTTPIntentPayload = "not HTTP: internal persisted rail_intents data; the pinned provider wire is asserted separately"
)

func TestEveryWireMoneyIntegerIsADecimalString(t *testing.T) {
	var violations, converted []string
	seen := map[string]bool{}
	for _, dir := range wireDTODirs {
		for field, exact := range moneyFields(t, dir) {
			seen[field] = true
			_, pending := pendingNumericMoney[field]
			switch {
			case !exact && !pending:
				violations = append(violations, field)
			case exact && pending:
				converted = append(converted, field)
			}
		}
	}
	var stale []string
	for field := range pendingNumericMoney {
		if _, ok := seen[field]; !ok {
			stale = append(stale, field)
		}
	}
	sort.Strings(violations)
	sort.Strings(converted)
	sort.Strings(stale)
	if len(violations) > 0 {
		t.Fatalf("monetary integers must use the json \",string\" option (docs/money-wire.md):\n%s", strings.Join(violations, "\n"))
	}
	if len(converted) > 0 {
		t.Fatalf("remove converted entries from pendingNumericMoney:\n%s", strings.Join(converted, "\n"))
	}
	if len(stale) > 0 {
		t.Fatalf("remove entries for deleted fields from pendingNumericMoney:\n%s", strings.Join(stale, "\n"))
	}
}

// moneyFields maps "dir/file.go:Type.Field json_name" to whether each
// int64/uint64 struct field with a monetary JSON name uses ",string". Entries
// for types deleted elsewhere simply stop matching.
func moneyFields(t *testing.T, dir string) map[string]bool {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]bool{}
	fset := token.NewFileSet()
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		path := filepath.Join(dir, name)
		file, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatal(err)
		}
		owner := ""
		ast.Inspect(file, func(node ast.Node) bool {
			switch n := node.(type) {
			case *ast.FuncDecl:
				owner = "func " + n.Name.Name
			case *ast.TypeSpec:
				owner = n.Name.Name
			case *ast.StructType:
				for _, field := range n.Fields.List {
					if field.Tag == nil || !isWideInteger(field.Type) {
						continue
					}
					tag, err := strconv.Unquote(field.Tag.Value)
					if err != nil {
						t.Fatal(err)
					}
					parts := strings.Split(reflect.StructTag(tag).Get("json"), ",")
					if parts[0] == "" || parts[0] == "-" || !moneyJSONName.MatchString(parts[0]) {
						continue
					}
					for _, ident := range field.Names {
						out[filepath.ToSlash(path)+":"+owner+"."+ident.Name+" "+parts[0]] = hasOption(parts[1:], "string")
					}
				}
			}
			return true
		})
	}
	return out
}

func isWideInteger(expr ast.Expr) bool {
	if star, ok := expr.(*ast.StarExpr); ok {
		expr = star.X
	}
	ident, ok := expr.(*ast.Ident)
	return ok && (ident.Name == "int64" || ident.Name == "uint64")
}

func hasOption(options []string, want string) bool {
	for _, option := range options {
		if option == want {
			return true
		}
	}
	return false
}
