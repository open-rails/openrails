package openrails

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
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

// wireRoots are scanned for every non-test Go file, recursively: the root
// package and everything a handler, worker or the shared Client can encode.
// Generated SQLC output is the storage layer and is skipped.
var (
	wireRoots    = []string{".", "pkg", "internal", "embed", "config", "permissions"}
	skippedTrees = map[string]bool{"internal/db/gen": true}
)

// pendingNumericMoney lists the monetary values still encoded as JSON
// numbers or held in an untyped field: every one is a type that never reaches
// the HTTP wire. The guard fails when a listed field becomes a decimal string
// or stops existing, so the list can only shrink.
var pendingNumericMoney = map[string]string{
	"internal/http/handlers/service_commerce.go:func ServiceCreateCheckoutSession.NewPriceID new_price_id":                                           "not money: rejected legacy price reference; any presence fails before executing checkout",
	"internal/intents/nmi_provider_cutover.go:nmiCutoverPayload.Amount amount":                                                                       notHTTPIntentPayload,
	"internal/modules/subscriptions/stripe_invoice_collection.go:func GetCollectedInvoice.AmountCaptured amount_captured":                            "Stripe-owned inbound integer amount; the retained qualified receipt encodes charged_amount as a decimal string",
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
	"internal/intents/collection_payload.go:InvoiceCollectionPayload.Amount amount":                                                                  notHTTPIntentPayload,
	"embed/river.go:InvoiceSweepArgs.CollectionThresholdAmount collection_threshold_amount":                                                          notHTTPJobArgs,
	"internal/db/models/billing_policy.go:BillingPolicy.AccrualRateCapPerHour accrual_rate_cap_per_hour":                                             notHTTPStorageRow,
	"internal/db/models/billing_policy.go:BillingPolicy.CollectionThresholdAmount collection_threshold_amount":                                       notHTTPStorageRow,
	"internal/db/models/billing_policy.go:BillingPolicy.DelinquencyAmountFloor delinquency_amount_floor":                                             notHTTPStorageRow,
	"internal/db/models/billing_policy.go:BillingPolicy.OutstandingCapAmount outstanding_cap_amount":                                                 notHTTPStorageRow,
	"internal/db/models/budget_window_policy.go:BudgetWindowPolicy.Limit limit":                                                                      notHTTPStorageRow,
	"internal/db/models/checkout_session.go:CheckoutSession.Amount amount":                                                                           notHTTPStorageRow,
	"internal/db/models/invoice.go:Invoice.AmountDue amount_due":                                                                                     notHTTPStorageRow,
	"internal/db/models/invoice.go:Invoice.AmountPaid amount_paid":                                                                                   notHTTPStorageRow,
	"internal/db/models/invoice.go:Invoice.ClosingBalance closing_balance":                                                                           notHTTPStorageRow,
	"internal/db/models/invoice.go:Invoice.OwedAccrued owed_accrued":                                                                                 notHTTPStorageRow,
	"internal/db/models/invoice.go:Invoice.OwedPaid owed_paid":                                                                                       notHTTPStorageRow,
	"internal/db/models/invoice.go:Invoice.SubtotalAmount subtotal_amount":                                                                           notHTTPStorageRow,
	"internal/db/models/invoice.go:Invoice.Tax tax":                                                                                                  notHTTPStorageRow,
	"internal/db/models/invoice.go:Invoice.TotalAmount total_amount":                                                                                 notHTTPStorageRow,
	"internal/db/models/invoice.go:InvoiceLineItem.Amount amount":                                                                                    notHTTPStorageRow,
	"internal/db/models/invoice.go:InvoicePaymentAttempt.Amount amount":                                                                              notHTTPStorageRow,
	"internal/db/models/merchant_configuration.go:MerchantConfiguration.ArrearsDelinquencyFloor arrears_delinquency_floor":                           notHTTPStorageRow,
	"internal/db/models/merchant_configuration.go:MerchantConfiguration.InvoiceCollectionThreshold collection_threshold":                             notHTTPStorageRow,
	"internal/db/models/merchant_configuration.go:MerchantConfiguration.InvoiceMonthlyFloor monthly_floor":                                           notHTTPStorageRow,
	"internal/db/models/money.go:MoneyAccount.CreditLimitAmount credit_limit_amount":                                                                 notHTTPStorageRow,
	"internal/db/models/money.go:MoneyBalance.Balance balance":                                                                                       notHTTPStorageRow,
	"internal/db/models/money.go:MoneyBalance.HeldBalance held_balance":                                                                              notHTTPStorageRow,
	"internal/db/models/money.go:MoneyTransaction.Amount amount":                                                                                     notHTTPStorageRow,
	"internal/db/models/money.go:MoneyTransaction.Authorized authorized_amount":                                                                      notHTTPStorageRow,
	"internal/db/models/money.go:MoneyTransaction.BalanceAfter balance_after":                                                                        notHTTPStorageRow,
	"internal/db/models/money.go:MoneyTransaction.Captured captured_amount":                                                                          notHTTPStorageRow,
	"internal/db/models/payment.go:Payment.Amount amount":                                                                                            notHTTPStorageRow,
	"internal/db/models/payment.go:Payment.ListAmount list_amount":                                                                                   notHTTPStorageRow,
	"internal/db/models/product_catalog.go:Price.Amount amount":                                                                                      notHTTPStorageRow,
	"internal/db/models/product_catalog.go:Price.TrialUnitAmount trial_unit_amount":                                                                  notHTTPStorageRow,
	"internal/db/models/usage_event.go:UsageEvent.Amount amount":                                                                                     notHTTPStorageRow,
	"internal/http/handlers/admin_catalog.go:paginatedResponse.Limit limit":                                                                          notMoneyPageSize,
	"internal/http/handlers/admin_findings.go:findingsListResponse.Limit limit":                                                                      notMoneyPageSize,
	"internal/integrations/nmi/v5.go:v5PaymentRequest.Amount amount":                                                                                 notHTTPProviderWire,
	"internal/integrations/nmi/v5.go:v5PlanCreateRequest.PlanAmount plan_amount":                                                                     notHTTPProviderWire,
	"internal/modules/abuse/wasted_spend.go:WindowUsage.Limit limit":                                                                                 notHTTPInternalRow,
	"internal/modules/abuse/wasted_spend.go:WindowUsage.Used used":                                                                                   notHTTPInternalRow,
	"internal/modules/admission/spendgate/policy.go:Window.Limit limit":                                                                              notHTTPInternalRow,
	"internal/modules/catalog/stripe_catalog.go:StripePrice.UnitAmount unit_amount":                                                                  notHTTPProviderWire,
	"internal/modules/checkout/custodian_sale.go:CustodianSalePayload.AmountMicros amount_micros":                                                    notHTTPIntentPayload,
	"internal/modules/checkout/stripe_tier_change_intent.go:StripeTierChangePayload.RecurringAmount recurring_amount":                                notHTTPIntentPayload,
	"internal/modules/checkout/stripe_tier_change_intent.go:StripeTierChangePayload.AmountDueNow amount_due_now":                                     notHTTPIntentPayload,
	"internal/modules/delinquency/service.go:Snapshot.OverdueAmount overdue_amount":                                                                  notHTTPInternalRow,
	"internal/modules/metrics/query.go:Query.Limit limit":                                                                                            notMoneyPageSize,
	"internal/modules/metrics/schema.go:SchemaLimits.MaxLimit max_limit":                                                                             notMoneyPageSize,
	"internal/modules/money/credit_grants.go:CreditGrantPage.Limit limit":                                                                            notMoneyPageSize,
	"internal/modules/money/invoice_profile.go:CustomerInvoiceProfile.Tax tax":                                                                       notMoneyTaxFacts,
	"internal/modules/ratelimit/limiter.go:WindowInfo.Limit limit":                                                                                   notMoneyCount,
	"internal/modules/ratelimit/limiter.go:WindowInfo.Remaining remaining":                                                                           notMoneyCount,
	"internal/modules/subscriptions/stripe_engine_payment.go:func engineReceipt.Amount amount":                                                       notHTTPProviderWire,
	"internal/modules/subscriptions/stripe_engine_payment.go:func engineReceipt.AmountRefunded amount_refunded":                                      notHTTPProviderWire,
	"internal/modules/subscriptions/stripe_engine_payment.go:func engineReceipt.AmountCaptured amount_captured":                                      notHTTPProviderWire,
	"internal/modules/subscriptions/stripe_engine_payment.go:stripeEngineIntent.Amount amount":                                                       notHTTPProviderWire,
	"internal/modules/subscriptions/stripe_engine_payment.go:stripeEngineIntent.AmountReceived amount_received":                                      notHTTPProviderWire,
	"internal/modules/subscriptions/stripe_liveness_source.go:stripeLivenessSubscriptionEnvelope.AmountDue amount_due":                               notHTTPProviderWire,
	"internal/modules/subscriptions/stripe_liveness_source.go:stripeLivenessSubscriptionEnvelope.AmountPaid amount_paid":                             notHTTPProviderWire,
	"internal/modules/subscriptions/stripe_refunds.go:RefundResult.Amount amount":                                                                    notHTTPProviderWire,
	"internal/modules/webhooks/stripe.go:stripeCharge.Amount amount":                                                                                 notHTTPProviderWire,
	"internal/modules/webhooks/stripe.go:stripeCharge.AmountRefunded amount_refunded":                                                                notHTTPProviderWire,
	"internal/modules/webhooks/stripe.go:stripeCheckoutSession.AmountTotal amount_total":                                                             notHTTPProviderWire,
	"internal/modules/webhooks/stripe.go:stripeDispute.Amount amount":                                                                                notHTTPProviderWire,
	"internal/modules/webhooks/stripe.go:stripeInvoice.AmountDue amount_due":                                                                         notHTTPProviderWire,
	"internal/modules/webhooks/stripe.go:stripeInvoice.AmountPaid amount_paid":                                                                       notHTTPProviderWire,
	"internal/modules/webhooks/stripe.go:stripeRefund.Amount amount":                                                                                 notHTTPProviderWire,
	"internal/modules/webhooks/webhook_handler.go:func Normalize.Amount amount":                                                                      notHTTPProviderWire,
	"internal/modules/webhooks/webhook_handler.go:func Normalize.AmountPaid amount_paid":                                                             notHTTPProviderWire,
	"internal/modules/webhooks/webhook_handler.go:func Normalize.AmountTotal amount_total":                                                           notHTTPProviderWire,
	"internal/reconcile/converge/converge_passes.go:ownershipPurchaseRow.Amount amount":                                                              notHTTPQueryRow,
	"internal/reconcile/destructive_run_converge.go:ConvergeRollbackResult.EntitlementsCaptured entitlements_captured":                               notMoneyCount,
	"internal/reconcile/reconcile.go:RemoteSubscription.AmountCents amount_cents":                                                                    notHTTPInternalRow,
	"internal/reconcile/reconcile.go:RemoteTransaction.AmountCents amount_cents":                                                                     notHTTPInternalRow,
	"internal/reconcile/stripe.go:func normalizeStripeCharge.Amount amount":                                                                          notHTTPProviderWire,
	"internal/reconcile/stripe.go:func normalizeStripeDispute.Amount amount":                                                                         notHTTPProviderWire,
	"internal/reconcile/stripe.go:func normalizeStripeRefund.Amount amount":                                                                          notHTTPProviderWire,
	"internal/reconcile/stripe.go:stripeSubscriptionJSON.UnitAmount unit_amount":                                                                     notHTTPProviderWire,
	"internal/river/jobs_credit_money_in.go:InvoiceArgs.CollectionThresholdAmount collection_threshold_amount":                                       notHTTPJobArgs,
	"internal/service/catalog_sidecars.go:CatalogRateCardSpec.Price price":                                                                           notHTTPInternalRow,
	"internal/service/service_definition_catalog_admin.go:CatalogPage.Limit limit":                                                                   notMoneyPageSize,

	"invoices.go:InvoiceDTO.Tax tax": notMoneyTaxFacts,

	"internal/modules/subscriptions/stripe_tier_change.go:func parseStripeScheduleState.Price price": notHTTPProviderWire,

	"invoices.go:InvoiceProfileDTO.Tax tax": notMoneyTaxFacts,

	"pkg/query/query.go:QueryOptions.Limit limit": notMoneyPageSize,

	"remote_catalog.go:CatalogPage.Limit limit": notMoneyPageSize,

	"subscriptions.go:Page.Limit limit": notMoneyPageSize,
}

const (
	notMoneyCount           = "not money: a count"
	notMoneyPageSize        = "not money: a page size"
	notMoneyTaxFacts        = "not money: tax facts (ids and schemes; profiles never calculate tax)"
	notHTTPStorageRow       = "not HTTP: storage row; every route projects it onto a wire DTO"
	notHTTPProviderWire     = "not HTTP: a provider's own wire format (Stripe/NMI JSON) decoded from or sent to the provider"
	notHTTPJobArgs          = "not HTTP: River job arguments"
	notHTTPQueryRow         = "not HTTP: the SQL query's jsonb shape, re-encoded onto the wire type beside it"
	notHTTPStoredMetadata   = "not HTTP: jsonb metadata or rail state stored on the row, never served"
	notHTTPLogContext       = "not HTTP: billing-error or webhook log context"
	notMoneyPurgeInventory  = "not money: the purge inventory's list of uncaptured secret names"
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

// pendingDynamicMoney lists the map[string]any entries with a monetary key
// whose value is not a string: each is stored JSONB, a provider's own wire, a
// log context or a page size — never served by a route. Same shrink-only rule.
var pendingDynamicMoney = map[string]string{
	"internal/http/handlers/admin_credit_grants.go:func ListAdminCreditTransactions \"limit\"":           notMoneyPageSize,
	"internal/http/handlers/admin_payments.go:func GetAdminUserPayments \"limit\"":                       notMoneyPageSize,
	"internal/http/handlers/admin_payments.go:func adminRefundMetadata \"admin_refund_amount\"":          notHTTPStoredMetadata,
	"internal/http/handlers/self_usage_invoices.go:func GetMyInvoices \"limit\"":                         notMoneyPageSize,
	"internal/http/request/request.go:func SuccessJSONPaginated \"limit\"":                               notMoneyPageSize,
	"internal/integrations/nmi/payments.go:func Refund \"amount\"":                                       notHTTPProviderWire,
	"internal/merchants/delete.go:func TakePurgeInventory \"not_captured\"":                              notMoneyPurgeInventory,
	"internal/modules/checkout/solana_settlement.go:func creditSolanaPurchase \"solana_token_amount\"":   notHTTPStoredMetadata,
	"internal/modules/checkout/session_service.go:func setSolanaQuoteState \"token_price_usd\"":          notHTTPStoredMetadata,
	"internal/modules/delinquency/service.go:func apply \"amount_floor\"":                                notHTTPStoredPayload,
	"internal/modules/delinquency/service.go:func apply \"overdue_amount\"":                              notHTTPStoredPayload,
	"internal/solanafake/solanafake.go:func Land \"fee\"":                                                notHTTPProviderWire,
	"internal/modules/solana/recurring/enroll_service.go:func ConfirmEnrollment \"solana_token_amount\"": notHTTPStoredMetadata,
	"internal/modules/webhooks/ccbill.go:func handleChargeback \"chargeback_amount_cents\"":              notHTTPStoredMetadata,
	"internal/modules/webhooks/ccbill.go:func handleRefund \"refund_amount_cents\"":                      notHTTPStoredMetadata,
	"internal/modules/webhooks/ccbill.go:func handleRenewalSuccessInternal \"amount_cents\"":             notHTTPStoredMetadata,
	"internal/modules/webhooks/ccbill.go:func handleUpgradeSuccess \"billed_amount_cents\"":              notHTTPStoredMetadata,
	"internal/modules/webhooks/ccbill.go:func handleUpgradeSuccess \"expected_amount_cents\"":            notHTTPStoredMetadata,
	"internal/modules/webhooks/ccbill.go:func validateCCBillBilledAmount \"billed_amount_cents\"":        notHTTPLogContext,
	"internal/modules/webhooks/ccbill.go:func validateCCBillBilledAmount \"expected_amount_cents\"":      notHTTPLogContext,
	"internal/modules/webhooks/nmi.go:func reconcileNMIChargebackEntry \"amount_cents\"":                 notHTTPLogContext,
	"internal/modules/webhooks/nmi.go:func reconcileNMIChargebackEntry \"matched_amount_cents\"":         notHTTPLogContext,
	"internal/river/jobs_solana_crank.go:func finalizePull \"solana_token_amount\"":                      notHTTPStoredMetadata,
}

// pinnedMarshalers lists every custom JSON marshaler in the wire tree with
// the test that pins its money encoding; a marshaler the struct-tag scan
// cannot see must be pinned or it is a violation.
var pinnedMarshalers = map[string]string{
	"merchant_configuration.go:MerchantConfigurationApplyParams": "TestMerchantConfigurationExplicitEmptyListsSurviveTransport",
	"amount_map.go:AmountMap":                                    "TestCanonicalWireFixtures (merchant_settings.json) — decimal strings",
	"internal/modules/metrics/service.go:MoneyCell":              "TestResultWireEncoding — decimal string",
	"pkg/catalog/application.go:Field":                           "TestApplicationFormatsPreserveIntent — exact int64 money as decimal strings",
}

func TestEveryWireMoneyIntegerIsADecimalString(t *testing.T) {
	scan := scanWireTree(t)
	var violations, converted []string
	for field, exact := range scan.fields {
		_, pending := pendingNumericMoney[field]
		switch {
		case !exact && !pending:
			violations = append(violations, field)
		case exact && pending:
			converted = append(converted, field)
		}
	}
	var stale []string
	for field := range pendingNumericMoney {
		if _, ok := scan.fields[field]; !ok {
			stale = append(stale, field)
		}
	}
	sort.Strings(violations)
	sort.Strings(converted)
	sort.Strings(stale)
	if len(violations) > 0 {
		t.Fatalf("monetary values must be int64/uint64 with the json \",string\" option (docs/money-wire.md); a float, a plain int or an untyped field is never money:\n%s", strings.Join(violations, "\n"))
	}
	if len(converted) > 0 {
		t.Fatalf("remove converted entries from pendingNumericMoney:\n%s", strings.Join(converted, "\n"))
	}
	if len(stale) > 0 {
		t.Fatalf("remove entries for deleted fields from pendingNumericMoney:\n%s", strings.Join(stale, "\n"))
	}
	var dynamic, staleDynamic []string
	seenDynamic := map[string]bool{}
	for _, entry := range scan.dynamic {
		seenDynamic[entry] = true
		if _, ok := pendingDynamicMoney[entry]; !ok {
			dynamic = append(dynamic, entry)
		}
	}
	for entry := range pendingDynamicMoney {
		if !seenDynamic[entry] {
			staleDynamic = append(staleDynamic, entry)
		}
	}
	sort.Strings(dynamic)
	sort.Strings(staleDynamic)
	if len(dynamic) > 0 {
		t.Fatalf("a map[string]any entry carries a monetary key whose value is not spelled as a string; use a typed DTO or format the amount (strconv.FormatInt):\n%s", strings.Join(dynamic, "\n"))
	}
	if len(staleDynamic) > 0 {
		t.Fatalf("remove pendingDynamicMoney entries for deleted sites:\n%s", strings.Join(staleDynamic, "\n"))
	}
	var unpinned, stalePins []string
	for m := range scan.marshalers {
		if _, ok := pinnedMarshalers[m]; !ok {
			unpinned = append(unpinned, m)
		}
	}
	for m := range pinnedMarshalers {
		if !scan.marshalers[m] {
			stalePins = append(stalePins, m)
		}
	}
	sort.Strings(unpinned)
	sort.Strings(stalePins)
	if len(unpinned) > 0 {
		t.Fatalf("custom MarshalJSON bypasses the struct-tag scan; pin its money encoding in pinnedMarshalers with the test that proves it:\n%s", strings.Join(unpinned, "\n"))
	}
	if len(stalePins) > 0 {
		t.Fatalf("remove pinnedMarshalers entries for deleted marshalers:\n%s", strings.Join(stalePins, "\n"))
	}
}

type wireScan struct {
	// fields maps "dir/file.go:Type.Field json_name" to whether the money
	// field is an int64/uint64 with ",string".
	fields map[string]bool
	// dynamic lists map[string]any literal entries with a monetary key and a
	// value that is not syntactically a string.
	dynamic []string
	// marshalers lists "dir/file.go:Type" for every MarshalJSON method.
	marshalers map[string]bool
}

func scanWireTree(t *testing.T) wireScan {
	t.Helper()
	scan := wireScan{fields: map[string]bool{}, marshalers: map[string]bool{}}
	fset := token.NewFileSet()
	for _, root := range wireRoots {
		err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			rel := filepath.ToSlash(path)
			if entry.IsDir() {
				if path != root && (root == "." || skippedTrees[rel] || strings.HasPrefix(entry.Name(), ".")) {
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(rel, ".go") || strings.HasSuffix(rel, "_test.go") {
				return nil
			}
			file, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
			if err != nil {
				return err
			}
			scanFile(t, &scan, rel, file)
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	return scan
}

func scanFile(t *testing.T, scan *wireScan, path string, file *ast.File) {
	t.Helper()
	owner := ""
	ast.Inspect(file, func(node ast.Node) bool {
		switch n := node.(type) {
		case *ast.FuncDecl:
			owner = "func " + n.Name.Name
			if n.Name.Name == "MarshalJSON" && n.Recv != nil && len(n.Recv.List) == 1 {
				scan.marshalers[path+":"+receiverName(n.Recv.List[0].Type)] = true
			}
		case *ast.TypeSpec:
			owner = n.Name.Name
		case *ast.StructType:
			for _, field := range n.Fields.List {
				if field.Tag == nil {
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
				kind := numericKind(field.Type)
				if kind == "" {
					continue
				}
				exact := kind == "int64" && hasOption(parts[1:], "string")
				for _, ident := range field.Names {
					scan.fields[path+":"+owner+"."+ident.Name+" "+parts[0]] = exact
				}
			}
		case *ast.CompositeLit:
			if !isAnyMap(n.Type) {
				return true
			}
			for _, element := range n.Elts {
				kv, ok := element.(*ast.KeyValueExpr)
				if !ok {
					continue
				}
				if name, ok := moneyKey(kv.Key); ok && !isStringExpr(kv.Value) {
					scan.dynamic = append(scan.dynamic, fmt.Sprintf("%s:%s %q", path, owner, name))
				}
			}
		case *ast.AssignStmt:
			// m["amount"] = v on any map: the map's type is not visible here, so
			// a monetary key assigned a non-string fails closed.
			for i, lhs := range n.Lhs {
				index, ok := lhs.(*ast.IndexExpr)
				if !ok || i >= len(n.Rhs) {
					continue
				}
				if name, ok := moneyKey(index.Index); ok && !isStringExpr(n.Rhs[i]) {
					scan.dynamic = append(scan.dynamic, fmt.Sprintf("%s:%s %q", path, owner, name))
				}
			}
		}
		return true
	})
}

// moneyKey reports a string-literal key that names money; keys naming ids
// (…_id, …_ids) never do.
func moneyKey(expr ast.Expr) (string, bool) {
	key, ok := expr.(*ast.BasicLit)
	if !ok || key.Kind != token.STRING {
		return "", false
	}
	name, err := strconv.Unquote(key.Value)
	if err != nil || strings.HasSuffix(name, "_id") || strings.HasSuffix(name, "_ids") || !moneyJSONName.MatchString(name) {
		return "", false
	}
	return name, true
}

// numericKind classifies a field type the money rule applies to: "int64" for
// int64/uint64 (exact with ",string"), "numeric" for every other integer or
// float, "dynamic" for any/map[string]any/json.RawMessage; "" otherwise.
func numericKind(expr ast.Expr) string {
	if star, ok := expr.(*ast.StarExpr); ok {
		expr = star.X
	}
	switch e := expr.(type) {
	case *ast.Ident:
		switch e.Name {
		case "int64", "uint64":
			return "int64"
		case "int", "int8", "int16", "int32", "uint", "uint8", "uint16", "uint32", "float32", "float64", "any":
			return "numeric"
		}
	case *ast.InterfaceType:
		return "dynamic"
	case *ast.MapType:
		if isAnyMap(expr) {
			return "dynamic"
		}
	case *ast.SelectorExpr:
		if pkg, ok := e.X.(*ast.Ident); ok && pkg.Name == "json" && e.Sel.Name == "RawMessage" {
			return "dynamic"
		}
	}
	return ""
}

func isAnyMap(expr ast.Expr) bool {
	m, ok := expr.(*ast.MapType)
	if !ok {
		return false
	}
	if key, ok := m.Key.(*ast.Ident); !ok || key.Name != "string" {
		return false
	}
	switch v := m.Value.(type) {
	case *ast.Ident:
		return v.Name == "any"
	case *ast.InterfaceType:
		return len(v.Methods.List) == 0
	}
	return false
}

// isStringExpr reports whether a map value is syntactically a string: a
// literal, a formatting call, a String()/Text() method or a conversion to
// string. Anything else (a raw int64, a struct, a float) fails closed.
func isStringExpr(expr ast.Expr) bool {
	switch e := expr.(type) {
	case *ast.BasicLit:
		return e.Kind == token.STRING
	case *ast.CallExpr:
		switch fn := e.Fun.(type) {
		case *ast.Ident:
			return fn.Name == "string" || stringFuncName(fn.Name)
		case *ast.SelectorExpr:
			return stringFuncName(fn.Sel.Name)
		}
	case *ast.BinaryExpr:
		return e.Op == token.ADD && isStringExpr(e.X) && isStringExpr(e.Y)
	}
	return false
}

// stringFuncName recognizes a call that produces a string by its name:
// strconv.Format*/Itoa, fmt.Sprint*, String()/Text() and format* helpers.
func stringFuncName(name string) bool {
	lower := strings.ToLower(name)
	return strings.HasPrefix(lower, "format") || strings.HasPrefix(lower, "sprint") || name == "Itoa" || name == "Text" || name == "Error" || strings.HasSuffix(name, "String")
}

func receiverName(expr ast.Expr) string {
	switch typed := expr.(type) {
	case *ast.StarExpr:
		return receiverName(typed.X)
	case *ast.IndexExpr:
		return receiverName(typed.X)
	case *ast.IndexListExpr:
		return receiverName(typed.X)
	case *ast.Ident:
		return typed.Name
	}
	return ""
}

func hasOption(options []string, want string) bool {
	for _, option := range options {
		if option == want {
			return true
		}
	}
	return false
}

// TestWireMoneyGuardDetects proves each rule fires on a minimal offender: a
// plain int, a float, an untyped field, a map literal, an index assignment
// and a custom marshaler are all caught; the exact spellings are not.
func TestWireMoneyGuardDetects(t *testing.T) {
	src := `package fixture
import "strconv"
type Row struct {
	Amount    int64   ` + "`json:\"amount,string\"`" + `
	Fee       int     ` + "`json:\"fee\"`" + `
	Price     float64 ` + "`json:\"price\"`" + `
	Amounts   any     ` + "`json:\"amounts\"`" + `
	Limit     int     ` + "`json:\"limit\"`" + `
	PriceID   string  ` + "`json:\"price_id\"`" + `
}
func (Row) MarshalJSON() ([]byte, error) { return nil, nil }
func build(v int64) map[string]any {
	m := map[string]any{"amount": v, "balance": strconv.FormatInt(v, 10), "price_id": v}
	m["fee"] = v
	m["tax"] = "0"
	return m
}
`
	file, err := parser.ParseFile(token.NewFileSet(), "fixture.go", src, parser.SkipObjectResolution)
	if err != nil {
		t.Fatal(err)
	}
	scan := wireScan{fields: map[string]bool{}, marshalers: map[string]bool{}}
	scanFile(t, &scan, "fixture.go", file)
	want := map[string]bool{
		"fixture.go:Row.Amount amount":   true,
		"fixture.go:Row.Fee fee":         false,
		"fixture.go:Row.Price price":     false,
		"fixture.go:Row.Amounts amounts": false,
		"fixture.go:Row.Limit limit":     false,
	}
	if !reflect.DeepEqual(want, scan.fields) {
		t.Fatalf("fields: got %v want %v", scan.fields, want)
	}
	sort.Strings(scan.dynamic)
	if wantDynamic := []string{`fixture.go:func build "amount"`, `fixture.go:func build "fee"`}; !reflect.DeepEqual(wantDynamic, scan.dynamic) {
		t.Fatalf("dynamic: got %v want %v", scan.dynamic, wantDynamic)
	}
	if !scan.marshalers["fixture.go:Row"] {
		t.Fatalf("marshaler not detected: %v", scan.marshalers)
	}
}
