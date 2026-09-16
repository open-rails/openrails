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
var moneyJSONName = regexp.MustCompile(`^units$|(^|_)(amount|amounts|price|limit|cap|threshold|floor|balance|revenue|fee|cost|refunded|due|owed|spent|used|reserved|remaining|captured|authorized)(_|$)`)

// wireDTODirs hold types that the shared Client or HTTP handlers encode.
var wireDTODirs = []string{".", "pkg/api", "pkg/catalog", "pkg/embedded/controlplane", "pkg/pricing", "pkg/service", "internal/http/handlers", "internal/modules/copilot", "internal/modules/money"}

// pendingNumericMoney lists monetary integers still encoded as JSON numbers.
// Every entry is a known #983 gap or a type that never reaches the HTTP wire;
// the guard fails when a listed field becomes a decimal string.
var pendingNumericMoney = map[string]string{
	"catalog.go:CatalogPrice.TrialUnitAmount trial_unit_amount":                                                                                      pendingCatalogPrice,
	"catalog.go:CatalogPrice.UnitAmount unit_amount":                                                                                                 pendingCatalogPrice,
	"catalog.go:CreatePriceRequest.TrialUnitAmount trial_unit_amount":                                                                                pendingCatalogPrice,
	"catalog.go:CreatePriceRequest.UnitAmount unit_amount":                                                                                           pendingCatalogPrice,
	"internal/modules/copilot/types.go:CreatePriceDraft.TrialUnitAmount trial_unit_amount":                                                           pendingCatalogPrice,
	"internal/modules/copilot/types.go:CreatePriceDraft.UnitAmount unit_amount":                                                                      pendingCatalogPrice,
	"internal/modules/copilot/types.go:PriceChangeDraft.CurrentAmount current_amount":                                                                pendingCatalogPrice,
	"internal/modules/copilot/types.go:PriceChangeDraft.NewAmount new_amount":                                                                        pendingCatalogPrice,
	"pkg/api/response.go:PriceObject.UnitAmount unit_amount":                                                                                         pendingCatalogPrice,
	"pkg/catalog/manifest.go:Price.UnitAmount unit_amount":                                                                                           pendingCatalogPrice,
	"pkg/catalog/manifest.go:PriceTrial.UnitAmount unit_amount":                                                                                      pendingCatalogPrice,
	"subscriptions.go:SubscriptionPayment.Amount amount":                                                                                             pendingSubscription,
	"subscriptions.go:SubscriptionPrice.Amount amount":                                                                                               pendingSubscription,
	"tier_change.go:TierChangePreviewResponse.AmountDueNow amount_due_now":                                                                           pendingSubscription,
	"tier_change.go:TierChangePreviewResponse.NextChargeAmount next_charge_amount":                                                                   pendingSubscription,
	"tier_change.go:TierChangeResponse.AmountDueNow amount_due_now":                                                                                  pendingSubscription,
	"tier_change.go:TierChangeResponse.NextChargeAmount next_charge_amount":                                                                          pendingSubscription,
	"internal/http/handlers/admin_payments.go:adminOffChannelPaymentRequest.Amount amount":                                                           pendingPayment,
	"internal/http/handlers/admin_payments.go:refundRequest.Amount amount":                                                                           pendingPayment,
	"internal/http/handlers/payment_api.go:userPaymentObject.Amount amount":                                                                          pendingPayment,
	"internal/http/handlers/payment_api.go:userPaymentObject.AmountRefunded amount_refunded":                                                         pendingPayment,
	"pkg/api/response.go:PaymentObject.Amount amount":                                                                                                pendingPayment,
	"pkg/api/response.go:PaymentObject.AmountRefunded amount_refunded":                                                                               pendingPayment,
	"pkg/pricing/price.go:FlatPrice.Amount amount":                                                                                                   pendingRateCard,
	"pkg/pricing/price.go:MatrixCell.MaximumAmount maximum_amount":                                                                                   pendingRateCard,
	"pkg/pricing/price.go:MatrixCell.UnitAmount unit_amount":                                                                                         pendingRateCard,
	"pkg/pricing/price.go:PackagePrice.Amount amount":                                                                                                pendingRateCard,
	"pkg/pricing/price.go:PerUnitPrice.MaximumAmount maximum_amount":                                                                                 pendingRateCard,
	"pkg/pricing/price.go:PerUnitPrice.UnitAmount unit_amount":                                                                                       pendingRateCard,
	"pkg/pricing/price.go:RateTier.FlatAmount flat_amount":                                                                                           pendingRateCard,
	"pkg/pricing/price.go:RateTier.UnitAmount unit_amount":                                                                                           pendingRateCard,
	"internal/http/handlers/admin_users.go:adminCreditBalanceResponse.Balance balance":                                                               pendingBalance,
	"internal/http/handlers/admin_users.go:adminCreditBalanceResponse.HeldBalance held_balance":                                                      pendingBalance,
	"internal/http/handlers/admin_users.go:adminCreditBalanceResponse.OutstandingOwedAmount outstanding_owed_amount":                                 pendingBalance,
	"internal/http/handlers/self_account.go:selfAccountSettingsRequest.AutoTopupAmount auto_topup_amount":                                            pendingBalance,
	"internal/http/handlers/self_account.go:selfAccountSettingsRequest.LowBalanceThreshold low_balance_threshold":                                    pendingBalance,
	"internal/http/handlers/self_account.go:selfAccountSettingsResponse.AutoTopupAmount auto_topup_amount":                                           pendingBalance,
	"internal/http/handlers/self_account.go:selfAccountSettingsResponse.LowBalanceThreshold low_balance_threshold":                                   pendingBalance,
	"internal/http/handlers/self_account.go:selfBalanceResponse.BalanceAmount balance_amount":                                                        pendingBalance,
	"internal/http/handlers/service_credits.go:adminGrantCreditsRequest.Amount amount":                                                               pendingBalance,
	"internal/http/handlers/service_credits.go:serviceTxnResponse.Amount amount":                                                                     pendingBalance,
	"internal/http/handlers/service_delinquency.go:serviceDelinquencyResponse.OverdueAmount overdue_amount":                                          pendingBalance,
	"internal/modules/money/credit_grants.go:CreditGrant.Amount amount":                                                                              pendingBalance,
	"internal/modules/money/credit_grants.go:CreditGrant.ExpiredAmount expired_amount":                                                               pendingBalance,
	"internal/modules/money/credit_grants.go:CreditGrant.RemainingAmount remaining_amount":                                                           pendingBalance,
	"internal/modules/money/credit_grants.go:CreditGrant.RevokedAmount revoked_amount":                                                               pendingBalance,
	"internal/modules/money/credit_grants.go:CreditGrant.SpentAmount spent_amount":                                                                   pendingBalance,
	"pkg/service/spend.go:UsageRow.TotalAmount total_amount":                                                                                         pendingBalance,
	"internal/http/handlers/solana_supported_tokens.go:TokenBalance.Units units":                                                                     pendingSolanaUnits,
	"internal/http/handlers/solana_supported_tokens.go:TokenQuote.Units units":                                                                       pendingSolanaUnits,
	"pkg/embedded/controlplane/fleet_analytics.go:FleetCurrencyRevenue.SettledAmount settled_amount_micros":                                          pendingFleet,
	"pkg/embedded/controlplane/fleet_analytics.go:FleetMRR.MonthlyAmount monthly_amount_micros":                                                      pendingFleet,
	"pkg/embedded/controlplane/fleet_timeseries.go:FleetWeeklyVolume.SettledAmount settled_amount_micros":                                            pendingFleet,
	"pkg/api/response.go:CreditGrantSpecObject.Amount amount":                                                                                        deletedByCatalogCut,
	"pkg/catalog/manifest.go:CreditGrant.Amount amount":                                                                                              deletedByCatalogCut,
	"pkg/catalog/manifest.go:UsageLimitWindow.Amount amount":                                                                                         deletedByCatalogCut,
	"pkg/service/catalog_recovery_metadata.go:credit.Amount amount":                                                                                  deletedByCatalogCut,
	"pkg/service/catalog_sidecars.go:CatalogUsageLimitWindowSpec.Amount amount":                                                                      deletedByCatalogCut,
	"pkg/service/service_definition_catalog.go:CreditGrantSpec.Amount amount":                                                                        deletedByCatalogCut,
	"pkg/embedded/controlplane/fleet_analytics.go:FleetMerchantFunnel.ActiveRevenue active_revenue":                                                  notMoneyCount,
	"pkg/embedded/controlplane/fleet_analytics.go:FleetMerchantFunnel.FirstRevenue first_revenue":                                                    notMoneyCount,
	"internal/modules/copilot/tools_draft.go:draftCatalogDiffArgs.UnitAmount unit_amount":                                                            notHTTPToolArgs,
	"internal/modules/copilot/tools_draft.go:draftPriceChangeArgs.NewAmount new_amount":                                                              notHTTPToolArgs,
	"internal/modules/money/enterprise.go:PendingCharge.Amount amount":                                                                               notHTTPPendingCharges,
	"pkg/service/enterprise.go:PendingChargeDTO.Amount amount":                                                                                       notHTTPPendingCharges,
	"internal/modules/money/reconcile.go:OrphanedHold.Amount authorized_amount":                                                                      notHTTPReconcileReport,
	"internal/modules/money/provider_billing.go:normalizedProviderBillingRecord.AmountUSDMicros amount_usd_micros":                                   notHTTPProviderEvidence,
	"internal/modules/money/provider_billing.go:providerBillingSettlementManifest.QualifiedProviderCostUSDMicros qualified_provider_cost_usd_micros": notHTTPProviderEvidence,
	"internal/modules/money/service_usage.go:ResourceRevenueDailyRow.Amount amount":                                                                  notHTTPInternalRow,
	"internal/modules/money/service_usage.go:ServiceUsageRollupRow.TotalAmount total_amount":                                                         notHTTPInternalRow,
	"internal/modules/money/usage.go:UsageRollupRow.TotalAmount total_amount":                                                                        notHTTPInternalRow,
	"pkg/service/spend.go:CreditAccountSnapshot.AvailableAmount available_amount":                                                                    notHTTPInternalRow,
	"pkg/service/spend.go:CreditAccountSnapshot.BalanceAmount balance_amount":                                                                        notHTTPInternalRow,
	"pkg/service/spend.go:CreditAccountSnapshot.HeldAmount held_amount":                                                                              notHTTPInternalRow,
	"pkg/service/spend.go:CreditAccountSnapshot.OutstandingOwedAmount outstanding_owed_amount":                                                       notHTTPInternalRow,
	"pkg/service/host_events.go:func ListHostEvents.AmountFloor amount_floor":                                                                        notHTTPStoredPayload,
	"pkg/service/host_events.go:func ListHostEvents.OverdueAmount overdue_amount":                                                                    notHTTPStoredPayload,
	"internal/modules/money/invoice_collection_intent.go:InvoiceCollectionPayload.Amount amount":                                                     notHTTPIntentPayload,
}

const (
	pendingCatalogPrice     = "pending #983: catalog price"
	pendingSubscription     = "pending #983: subscription and tier change"
	pendingPayment          = "pending #983: payment and refund"
	pendingRateCard         = "pending #983: rate card (also catalog_rate_cards JSONB)"
	pendingBalance          = "pending #983: balance, ledger, grant and delinquency"
	pendingSolanaUnits      = "pending #983: Solana token base units"
	pendingFleet            = "pending #983: fleet analytics encoded by the SaaS host"
	deletedByCatalogCut     = "deleted with catalog credit/usage-limit features (#1008 PR438)"
	notMoneyCount           = "not HTTP: merchant counts"
	notHTTPToolArgs         = "not HTTP: LLM tool-call arguments"
	notHTTPPendingCharges   = "not HTTP: no route serves pending charges"
	notHTTPReconcileReport  = "not HTTP: CLI/job reconcile report"
	notHTTPProviderEvidence = "not HTTP: provider billing evidence digest"
	notHTTPInternalRow      = "not HTTP: internal rows converted by pkg/service"
	notHTTPStoredPayload    = "not HTTP: stored host_outbox payload decoded before the Client re-encodes it"
	notHTTPIntentPayload    = "not HTTP: frozen rail_intents payload; the pinned provider wire is asserted separately"
)

func TestEveryWireMoneyIntegerIsADecimalString(t *testing.T) {
	var violations, converted []string
	for _, dir := range wireDTODirs {
		for field, exact := range moneyFields(t, dir) {
			_, pending := pendingNumericMoney[field]
			switch {
			case !exact && !pending:
				violations = append(violations, field)
			case exact && pending:
				converted = append(converted, field)
			}
		}
	}
	sort.Strings(violations)
	sort.Strings(converted)
	if len(violations) > 0 {
		t.Fatalf("monetary integers must use the json \",string\" option (docs/money-wire.md):\n%s", strings.Join(violations, "\n"))
	}
	if len(converted) > 0 {
		t.Fatalf("remove converted entries from pendingNumericMoney:\n%s", strings.Join(converted, "\n"))
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
