package metrics

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/decline"
	"github.com/open-rails/openrails/internal/disputerules"
	"github.com/open-rails/openrails/internal/modules/solana/solanasubs"
	"github.com/open-rails/openrails/internal/reconcile"
)

// The registry is the single source of truth for the metrics vocabulary:
// it drives validation, SQL compilation, /schema, and the LLM context doc.
// SQL text comes ONLY from fragments declared here; client strings are bind
// parameters, never SQL.
//
// A funnel is one measure (attempts, purchases, rebills, payments) whose
// family exposes an outcome or kind dimension, and its rates are ratios over
// slices of it (NumSlice, DenSlice): a rate and the dimension it slices share
// one SQL expression, so they cannot disagree.

// Class is a measure's aggregation class.
type Class = billing.MetricsClass

const (
	ClassAdditive   Class = "additive"   // SUM/COUNT over the grain
	ClassDistinct   Class = "distinct"   // COUNT(DISTINCT ...) per group; non-additive across buckets
	ClassRatio      Class = "ratio"      // numerator/denominator aggregated first, divided per row
	ClassDifference Class = "difference" // Num minus Den, each aggregated first
	ClassSnapshot   Class = "snapshot"   // point-in-time; never summed across buckets
)

// Group is the view a measure belongs to.
type Group = billing.MetricsGroup

const (
	GroupBusiness Group = "business" // the health overview and reports
	GroupOps      Group = "ops"      // operator signals: the ops view and findings
)

// Direction is which way a change is good news.
type Direction = billing.MetricsDirection

const (
	UpIsGood   Direction = "up_is_good"
	DownIsGood Direction = "down_is_good"
	Neutral    Direction = "neutral"
)

// Family is an internal source family: one SQL statement is issued per family
// involved in a query. Invisible to clients.
type Family string

const (
	FamPayments       Family = "payments"        // flow over billing.payments (purchased_at)
	FamGrants         Family = "grants"          // flow over credit-purchase grant lots (created_at)
	FamUsage          Family = "usage"           // flow over the event store's usage history (day)
	FamUsageCharged   Family = "usage_charged"   // flow over usage history, one row per charge (day)
	FamLimitsReached  Family = "limits_reached"  // flow over limit windows (reached_at)
	FamSubsSnapshot   Family = "subs_snapshot"   // interval reconstruction over subscriptions
	FamEntitlSnapshot Family = "entitl_snapshot" // interval reconstruction over product access and product keys
	FamBalance        Family = "balance"         // cumulative sums over ledger_transfers
	FamDepletion      Family = "depletion"       // per-customer balance vs trailing-7d burn
	FamWebhookHealth  Family = "webhook_health"  // snapshot over webhook_health watermarks
	FamWebhookDaily   Family = "webhook_daily"   // flow over webhook_health_daily counter buckets
	FamAttempts       Family = "attempts"        // flow over payment_attempts (attempted_at)
	FamPurchases      Family = "purchases"       // flow over purchases, one per purchase_id (first attempt)
	FamRebillCycles   Family = "rebill_cycles"   // cohort over rebill_cycles (due_at: the period that came due)
	FamNMIHistory     Family = "nmi_history"     // flow over nmi_history_months (month_at)
	FamFindings       Family = "findings"        // snapshot over reconciliation_findings: open at t
	FamDisputes       Family = "disputes"        // flow over disputes (a chargeback when it became one, else disputed_at)
	FamFraudReports   Family = "fraud_reports"   // flow over fraud_reports (reported_at)
	FamNetwork        Family = "network"         // flow over network_activity_daily (day_at)
	FamNetworkPrior   Family = "network_prior"   // flow over network_activity_daily, each day counted a month later
	FamRiskTotals     Family = "risk_totals"     // at-risk MRR totals less the flows after t
	FamRiskFlows      Family = "risk_flows"      // flow over mrr_risk_flows (bucket_at)
	FamMRR            Family = "mrr"             // MRR totals less the flows after t; by product, the product flows before t
	FamMRRProduct     Family = "mrr_product"     // product totals less the product flows after t: FamMRR by product
	FamMRRCustomers   Family = "mrr_customers"   // distinct customers with MRR: FamMRR without a currency
	FamMRRFlows       Family = "mrr_flows"       // flow over mrr_flows: movements by kind
	FamMRRChurn       Family = "mrr_churn"       // flow over mrr_streak_flows' churn rows of the opening base, by churn_type
	FamMRRStreak      Family = "mrr_streak"      // flow over mrr_streak_flows: the opening base's expansion, contraction, churn and corrections
	FamMRRTrailing    Family = "mrr_trailing"    // the opening base and its churn over the three months ending at each bucket's end
	FamScheduled      Family = "mrr_scheduled"   // flow over pending mrr_scheduled entries (effective_at)
	FamCohort         Family = "mrr_cohort"      // mrr_cohort_flows by cohort month and months since it
	FamCancellations  Family = "cancellations"   // flow over subscription_cancellations (decided_at)
)

// familySpec describes how a family's single statement is assembled.
type familySpec struct {
	Kind     string // "flow" | "snapshot" | "balance" | "depletion"
	From     string // FROM clause (base table + always-on joins), no WHERE
	TimeExpr string // flow: the event-time column (bucket + range predicate)
	// DimJoins: extra JOIN clauses required when a given dimension is used.
	DimJoins map[string]string
	// DimExprs: dimension name -> SQL expression valid in this family.
	DimExprs map[string]string
	// DimValues: a dimension's closed vocabulary in this family, where it
	// differs from the dimension's own (outcome, kind).
	DimValues map[string][]string
	// BaseWhere: family-invariant predicate (no params).
	BaseWhere string
	// Calendar: TimeExpr is the UTC midnight of a day (or month) a daily
	// rollup was kept for. It groups as that date in any zone: there is no
	// finer time to regroup.
	Calendar bool
}

// Slice restricts a funnel measure to the rows whose dimensions take the
// given values: registry constants, inlined as SQL literals.
type Slice map[string][]string

// Measure is one registry entry.
type Measure struct {
	Name        string
	Class       Class
	Family      Family
	Group       Group     // GroupBusiness when empty
	Direction   Direction // required on every public measure
	Thresholds  []billing.MetricsThreshold
	Description string // one line, token-lean (rides in LLM context)
	Formula     string // /schema formula text
	Unit        string // UnitMoney | "count" | "ratio" | "days" | "seconds"
	// Expr is the aggregate SQL expression (flow + snapshot families).
	Expr string
	// Agg builds a funnel measure's aggregate over the rows an extra
	// predicate admits ("" admits all); it sets Expr and lets rates slice it.
	Agg func(extra string) string
	// Dims are the allowed group-by/filter dimensions (besides time). A
	// sliced ratio without Dims takes its components' dimensions less the
	// sliced ones.
	Dims []string
	// Money marks currency-sensitive measures (implicit currency group-by).
	Money bool
	// Components of a ratio (Num / Den) or a difference (Num - Den):
	// resolved through the registry; may be internal-only measures.
	Num, Den string
	// NumSlice and DenSlice restrict a ratio's components to slices of their
	// funnel measures.
	NumSlice, DenSlice Slice
	// Internal measures are compiler components: not requestable, not in /schema.
	Internal bool
	// SnapshotAtPeriodStart: snapshot evaluated at bucket/range START instead of
	// bucket start/range END (used by churn's denominator).
	SnapshotAtPeriodStart bool
	// ByProduct is a FamMRR measure's Expr over FamMRRProduct, read when a
	// query groups or filters by product_id.
	ByProduct string
	// ByCustomer is a FamMRR measure's Expr over FamMRRCustomers, read when a
	// query neither groups nor filters by currency: each customer once.
	ByCustomer string
	// PerMonth is a rate over a period read as a monthly rate: the period's
	// rate p over m months (in the accounting timezone) is 1 - (1 - p)^(1/m).
	// Its period is the bucket's, or with Trailing the three months ending
	// at the bucket's end.
	PerMonth, Trailing bool
	// Program is the dispute monitoring program whose ratio the measure is.
	Program billing.DisputeProgram
	// Untimed measures take no time grouping (their own dimensions order them).
	Untimed bool

	// values: each allowed dimension's closed vocabulary on this measure
	// (nil: open), set by registry.
	values map[string][]string
}

// Dimension is one group-by/filter axis.
type Dimension struct {
	Name        string
	Description string
	Values      []string // closed vocabulary when non-nil (validated on filters)
	// Parse validates and canonicalizes one filter value (typed ids); nil
	// accepts any string.
	Parse func(string) (string, error)
}

// typedDimValue accepts exactly the typed id's wire spelling; the bare UUID
// or another kind's prefix is an invalid filter value.
func typedDimValue(parse func(string) (fmt.Stringer, error)) func(string) (string, error) {
	return func(raw string) (string, error) {
		id, err := parse(raw)
		if err != nil {
			return "", err
		}
		if id.String() == "" {
			return "", fmt.Errorf("empty id")
		}
		return id.String(), nil
	}
}

const (
	// A subscription's MRR is billing.price_mrr, the one MRR the subscription
	// rows and movements count; the coarse cycle labels never set a billing
	// boundary.
	priceMRRExpr     = `billing.price_mrr(pr, s.quantity)`
	billingCycleExpr = `billing.billing_cycle_label(COALESCE(pr.billing_interval_hours, pr.billing_interval_months * 730))`

	// streamExpr classifies a payment's revenue stream.
	streamExpr = `CASE
		WHEN p.subscription_id IS NOT NULL THEN 'subscription'
		ELSE 'one_time' END`

	// A sale row has no refunded_payment_id; a reversal (refund, chargeback,
	// won dispute) names its sale and its reversal_kind. Settled rows count;
	// a pending or failed refund does not.
	settledRow     = `p.status IN ('succeeded','refunded')`
	reversalKindOf = `CASE WHEN p.refunded_payment_id IS NULL THEN '%s'
		WHEN p.reversal_kind = 'refund' THEN 'refund'
		WHEN p.reversal_kind = 'chargeback' THEN 'chargeback'
		ELSE 'dispute_won' END`
)

// Dimensions is the dimension registry.
var Dimensions = []Dimension{
	{Name: "time", Description: "bucket by the query grain (auto|day|week|month|quarter|year), on the calendar of the merchant's accounting timezone"},
	{Name: "currency", Description: "currency code; added implicitly when a money measure is grouped without a single-currency filter"},
	{Name: "rail", Description: "payment rail: nmi, ccbill, stripe or solana"},
	{Name: "psp", Description: "the PSP's id (psp_…), empty when none"},
	{Name: "stream", Description: "revenue stream: subscription | one_time", Values: []string{"subscription", "one_time"}},
	{Name: "product_id", Description: "product id (prod_<uuid>, the catalog's spelling)", Parse: typedDimValue(func(s string) (fmt.Stringer, error) { return billing.ParseProductID(s) })},
	{Name: "price_id", Description: "immutable price id (product:price:v1)", Parse: typedDimValue(func(s string) (fmt.Stringer, error) { return billing.ParsePriceID(s) })},
	{Name: "billing_cycle", Description: "price cadence: hourly|daily|weekly|monthly|quarterly|semiannual|annual|one_time", Values: []string{"hourly", "daily", "weekly", "monthly", "quarterly", "semiannual", "annual", "one_time"}},
	{Name: "cancel_type", Description: "cancellation type recorded on the subscription (e.g. user, merchant, chargeback, failed_payment, expired)"},
	{Name: "reversed", Description: "whether a cancellation was later reversed (a resume): true | false", Values: []string{"true", "false"}},
	{Name: "movement", Description: "what moved MRR: new (first MRR) | expansion | contraction | churn (to none) | reactivation (MRR again after none) | opening (an import adopting subscriptions) | correction (a prune or its undo); on scheduled_mrr churn | contraction | expansion", Values: []string{"new", "expansion", "contraction", "churn", "reactivation", "opening", "correction"}},
	{Name: "churn_type", Description: "why a customer churned: voluntary (their or the merchant's cancel) | chargeback | involuntary (failed payment, expiry and the rest)", Values: []string{"voluntary", "involuntary", "chargeback"}},
	{Name: "cohort", Description: "the month (YYYY-MM, accounting timezone) a customer first had MRR"},
	{Name: "months_since_start", Description: "whole months since the cohort's month: 0 is the cohort's own month"},
	{Name: "basis", Description: "what cohort_retention counts: subscribers (customers with MRR; default) | mrr", Values: []string{"subscribers", "mrr"}},
	{Name: "customer", Description: "the customer id (usage and limit measures)", Parse: typedDimValue(func(s string) (fmt.Stringer, error) { return billing.ParseCustomerID(s) })},
	{Name: "invoker", Description: "the invoker that fired a usage event (usage measures)"},
	{Name: "outcome", Description: "how a funnel's row ended; its values depend on the measure (dim_values): attempts and nmi_history_authorizations approved | declined (the issuer said no) | refused (the merchant or its gateway refused it first) | error (it did not complete); purchases approved | recovered | failed | open; rebills approved | recovered | lost | open; usage succeeded | failed; disputes open | won | lost | closed | unreported; fraud_reports recorded | refunded | nothing_to_refund | disputed"},
	{Name: "cash_kind", Description: "what moved the cash: payment | refund | chargeback | dispute_won (a won dispute's return)", Values: []string{"payment", "refund", "chargeback", "dispute_won"}},
	{Name: "payment_kind", Description: "sale | refund | chargeback | dispute_won", Values: []string{"sale", "refund", "chargeback", "dispute_won"}},
	{Name: "charge", Description: "priced usage: charged (billed to the customer) | forgiven (failed usage the customer's grace absorbed)", Values: []string{"charged", "forgiven"}},
	{Name: "rate_card", Description: "metered event type (the usage's event_type; the key rate cards price)"},
	{Name: "card_brand", Description: "card brand on the payment (empty when not card-based)"},
	{Name: "token_type", Description: "credential form presented at charge time: network_token | pan_via_proxy (custodian-held FPAN via proxy) | psp_token (the PSP's own stored credential) | unknown (legacy/non-card); authorization_rate by token_type = the network-token uplift", Values: []string{"network_token", "pan_via_proxy", "psp_token", "unknown"}},
	{Name: "entitlement", Description: "entitlement key string (e.g. premium)"},
	{Name: "discount_code", Description: "discount code on the payment (empty = none)"},
	{Name: "limit", Description: "a declared usage limit's key (limit measures)"},
	{Name: "owner", Description: "who collects the subscription: engine (OpenRails) | nmi_schedule (NMI charges, OpenRails duns) | provider (Stripe Billing, CCBill) | none (no subscription)", Values: []string{"engine", "nmi_schedule", "provider", "none"}},
	{Name: "kind", Description: "what was charged; on attempts verify ($0 card verification) | initial | upgrade | rebill (a cycle's first try) | dunning_retry | customer_retry | collection (a usage order's); on cash, payments and disputes (the disputed charge) initial | renewal | unknown (imported or pre-instrumentation rows)"},
	{Name: "network", Description: "card network: visa | mastercard | amex | discover | jcb | diners | unionpay | other | unknown", Values: networkValues},
	{Name: "refund_reason", Description: "why a refund was made (empty on other rows): requested_by_customer | duplicate | fraudulent | fraud_warning (an issuer's fraud report) | dispute_prevention (an RDR resolution) | charged_after_cancel | other | unknown", Values: refundReasonValues},
	{Name: "dispute_kind", Description: "what a dispute case is: inquiry (nothing withdrawn) | chargeback | prevented (resolved before it was filed) | unknown", Values: []string{"inquiry", "chargeback", "prevented", "unknown"}},
	{Name: "dispute_reason", Description: "the reason family of a dispute's network code or provider reason", Values: disputeReasonValues},
	{Name: "card_entry", Description: "new (card entered in this checkout or card add) | saved", Values: []string{"new", "saved"}},
	{Name: "source", Description: "who sent the attempt: openrails | provider_schedule (the PSP's own schedule) | external", Values: []string{"openrails", "provider_schedule", "external"}},
	{Name: "observed_via", Description: "how OpenRails learned the answer: response | webhook | pull", Values: []string{"response", "webhook", "pull"}},
	{Name: "decline_cause", Description: "who or what refused it: customer | issuer (its discretion) | fraud (suspected) | merchant (or its gateway) | processing | unknown (empty on approvals)", Values: append([]string{""}, declineCauseValues()...)},
	{Name: "reason", Description: "decline reason from the one classifier (empty on approvals)", Values: append([]string{""}, declineReasonValues()...)},
	{Name: "response_code", Description: "the PSP's response code verbatim (group with limit for the top codes)"},
	{Name: "issuer_code", Description: "the issuer's raw answer (NMI processor_response_code); empty until enriched"},
	{Name: "avs_result", Description: "AVS result letter or check (empty when absent)"},
	{Name: "cvv_result", Description: "CVV result letter or check (empty when absent)"},
	{Name: "card_bin", Description: "card BIN (first 6-8 digits; empty until known): the issuing bank's range"},
	{Name: "advice_code", Description: "the PSP's advice on a refusal, verbatim (Stripe advice_code: do_not_try_again, confirm_card_data, try_again_later; empty otherwise)"},
	{Name: "network_advice_code", Description: "the card network's advice on a refusal, verbatim (Mastercard's Merchant Advice Code: 01 new card data, 02 try later, 03 do not try again, 21 payment canceled, 24-30 retry after; empty otherwise)"},
	{Name: "visa_category", Description: "Visa's category of a refused Visa card's issuer code: 1 never approve | 2 cannot approve now | 3 fix the data | 4 other (empty on approvals, without an issuer code or on another brand)", Values: []string{"", "1", "2", "3", "4"}},
	{Name: "network_transaction_id", Description: "whether a charge on a stored credential sent the network transaction id of its storing transaction: sent (Stripe links its own) | missing | none (no stored credential cited: a new card or the storing charge); on rebills the first attempt's, empty without one", Values: []string{"sent", "missing", "none"}},
	{Name: "first_outcome", Description: "a rebill cycle's first outcome: approved | declined | refused | error | missed | pending (nothing yet)", Values: []string{"approved", "declined", "refused", "error", "missed", "pending"}},
	{Name: "first_decline_cause", Description: "the decline cause of a cycle's failed first attempt (empty otherwise)", Values: append([]string{""}, declineCauseValues()...)},
	{Name: "first_failure_reason", Description: "the decline reason of a cycle's failed first attempt (empty otherwise)"},
	{Name: "miss_reason", Description: "why a cycle's rebill never happened (empty when it did)", Values: []string{"", "refused", "method_unusable", "not_attempted", "provider_skipped", "provider_stalled", "provider_reversed", "provider_unrecorded", "schedule_gone"}},
	{Name: "recovered_by", Description: "what collected a cycle whose first outcome failed: dunning_retry | customer_retry | updated_card | late_provider_charge (empty otherwise)", Values: []string{"", "dunning_retry", "customer_retry", "updated_card", "late_provider_charge"}},
	{Name: "recovery_attempt", Description: "the attempt that collected a failed cycle, counting the first: 1 (a missed cycle's first attempt) | 2 | 3 | 4 | 5+ (empty otherwise)", Values: []string{"", "1", "2", "3", "4", "5+"}},
	{Name: "days_to_recover", Description: "whole days from a cycle's first failure to its collection (empty otherwise)"},
	// Findings: problems OpenRails found.
	{Name: "finding_type", Description: "finding type, e.g. life.provider_intent.stuck or pull.subscription.mismatch"},
	{Name: "finding_group", Description: "orphaned (payment without access) | freeloader (access without payment) | duplicate_coverage (the same coverage paid twice) | other", Values: []string{"orphaned", "freeloader", "duplicate_coverage", "other"}},
	{Name: "severity", Description: "finding severity", Values: []string{"critical", "high", "medium", "low"}},
	{Name: "finding_status", Description: "requires_review (needs a person; the console's bell counts these) | reconcile_required (OpenRails is repairing it)", Values: []string{"requires_review", "reconcile_required"}},
	{Name: "risk_reason", Description: "why MRR is at risk: past_due | needs_payment_method | unverified | card_expiring (its card expires before the renewal) | delinquent (its customer, in its currency); on at_risk_recovery_rate, the reason the case entered with", Values: riskReasonValues()},
	{Name: "nmi_kind", Description: "what NMI's own history tells apart: verification ($0 card verification) | one_off_sale (initial sales, upgrades and retries of declined rebills, whoever sent them) | scheduled_rebill (NMI's own schedule charge)", Values: []string{"verification", "one_off_sale", "scheduled_rebill"}},
}

// A payment settles its whole order, not each line independently. Attribute
// it to a price only when every line names the same price. Multi-price and
// non-catalog bills remain unassigned rather than multiplying payment money.
const paymentPriceJoin = `LEFT JOIN LATERAL (
  SELECT min(l.price_id::text)::uuid AS price_id
  FROM billing.order_lines l WHERE l.merchant_id=p.merchant_id AND l.order_id=p.order_id
  HAVING count(*) = count(l.price_id) AND count(DISTINCT l.price_id)=1
) paid_line ON true
LEFT JOIN billing.prices pr ON pr.merchant_id=p.merchant_id AND pr.id=paid_line.price_id`

var (
	attemptOutcomes = attemptOutcomeValues()
	usageOutcomes   = []string{"succeeded", "failed"}
	networkValues   = []string{"visa", "mastercard", "amex", "discover", "jcb", "diners", "unionpay", "other", "unknown"}
	// Sales and other reversals have no reason: "".
	refundReasonValues  = []string{"", "requested_by_customer", "duplicate", "fraudulent", "fraud_warning", "dispute_prevention", "charged_after_cancel", "other", "unknown"}
	disputeReasonValues = []string{"fraud", "unrecognized", "not_received", "canceled_recurring", "not_as_described", "credit_not_processed", "duplicate", "processing_error", "authorization", "other", "unknown"}
)

// families declares each family's SQL skeleton inputs.
var families = map[Family]familySpec{
	// At-risk MRR at t is the total now less the flows of the buckets from t
	// on; riskSpec picks the per-reason rows or, without risk_reason, the
	// rows that count each subscription once.
	// MRR at t is the totals now less the movements of the buckets from t on,
	// a product's its totals less its flows (FamMRRProduct). Each churn_type
	// sees every subscriber, so a rate by it divides by all.
	FamMRR: {
		Kind:     "mrr",
		From:     `billing.mrr_totals t`,
		DimJoins: map[string]string{"churn_type": `CROSS JOIN (VALUES ('voluntary'), ('involuntary'), ('chargeback')) ct(churn_type)`},
		DimExprs: map[string]string{"currency": `t.currency`, "churn_type": `ct.churn_type`},
	},
	FamMRRProduct: {
		Kind:     "snapshot",
		From:     `billing.mrr_product_totals pt`,
		DimExprs: map[string]string{"currency": `pt.currency`, "product_id": `'prod_' || pt.product_id::text`},
	},
	FamMRRCustomers: {
		Kind: "snapshot",
		From: `billing.mrr_customer_totals ct`,
	},
	FamMRRFlows: {
		Kind:      "flow",
		From:      `billing.mrr_flows f`,
		TimeExpr:  `f.bucket_at`,
		DimExprs:  map[string]string{"currency": `f.currency`, "movement": `f.measure`},
		BaseWhere: `f.measure NOT IN ('churn_voluntary', 'churn_chargeback')`,
	},
	// The opening base's churn: a churn row is involuntary unless its
	// voluntary or chargeback part says otherwise, each part moving its count
	// from involuntary.
	FamMRRChurn: {
		Kind: "flow",
		From: `billing.mrr_streak_flows sf CROSS JOIN LATERAL (SELECT ct.churn_type, ct.sign FROM (VALUES
			('churn', 'involuntary', 1), ('churn_voluntary', 'voluntary', 1), ('churn_voluntary', 'involuntary', -1),
			('churn_chargeback', 'chargeback', 1), ('churn_chargeback', 'involuntary', -1)) ct(measure, churn_type, sign)
			WHERE ct.measure = sf.measure) ct`,
		TimeExpr:  `sf.bucket_at`,
		DimExprs:  map[string]string{"currency": `sf.currency`, "churn_type": `ct.churn_type`},
		BaseWhere: `sf.measure IN ('churn', 'churn_voluntary', 'churn_chargeback')`,
	},
	// A run begun before a period's start is in its opening base:
	// @period_start is the start of the row's bucket.
	FamMRRStreak: {
		Kind:     "flow",
		From:     `billing.mrr_streak_flows sf`,
		TimeExpr: `sf.bucket_at`,
		DimJoins: map[string]string{"churn_type": `CROSS JOIN (VALUES ('voluntary'), ('involuntary'), ('chargeback')) ct(churn_type)`},
		DimExprs: map[string]string{"currency": `sf.currency`, "churn_type": `ct.churn_type`},
	},
	// Each window is the three months ending at a bucket's end (w.starts_at,
	// w.ends_at), labelled by the bucket.
	FamMRRTrailing: {
		Kind:     "trailing",
		From:     `billing.mrr_totals t`,
		DimExprs: map[string]string{"currency": `t.currency`},
	},
	FamScheduled: {
		Kind:      "flow",
		From:      `billing.mrr_scheduled ms`,
		TimeExpr:  `ms.effective_at`,
		DimExprs:  map[string]string{"currency": `ms.currency`, "movement": `ms.kind`},
		DimValues: map[string][]string{"movement": {"churn", "contraction", "expansion"}},
		BaseWhere: `ms.status = 'scheduled'`,
	},
	FamCohort: {
		Kind:     "cohort",
		From:     `billing.mrr_cohort_flows c`,
		DimExprs: map[string]string{"currency": `c.currency`},
	},
	// A cancel decision is a subscription's move into canceled, dated by its
	// writer's clock; a resume reverses it.
	FamCancellations: {
		Kind: "flow",
		From: `billing.subscription_cancellations sc
		JOIN billing.subscriptions s ON s.merchant_id = sc.merchant_id AND s.id = sc.subscription_id`,
		TimeExpr: `sc.decided_at`,
		DimJoins: map[string]string{
			"currency": `LEFT JOIN billing.prices pr ON pr.merchant_id = s.merchant_id AND pr.id = s.price_id`,
			"price_id": `LEFT JOIN billing.prices pr ON pr.merchant_id = s.merchant_id AND pr.id = s.price_id`,
		},
		DimExprs: map[string]string{
			"currency":    `COALESCE(pr.currency, '')`,
			"rail":        `s.rail`,
			"psp":         `COALESCE('psp_' || s.psp_id::text, '')`,
			"product_id":  `'prod_' || s.product_id::text`,
			"price_id":    `COALESCE(pr.public_id, '')`,
			"cancel_type": `COALESCE(sc.cancel_type, 'unknown')`,
			"reversed":    `CASE WHEN sc.reversed_at IS NULL THEN 'false' ELSE 'true' END`,
		},
	},
	FamRiskTotals: {
		Kind: "risk",
		From: `billing.mrr_risk_totals t`,
		DimExprs: map[string]string{
			"currency":    `t.currency`,
			"risk_reason": `t.reason`,
		},
	},
	FamRiskFlows: {
		Kind:      "flow",
		From:      `billing.mrr_risk_flows rf`,
		TimeExpr:  `rf.bucket_at`,
		BaseWhere: `rf.reason <> 'any'`,
		DimExprs: map[string]string{
			"currency":    `rf.currency`,
			"risk_reason": `rf.reason`,
		},
	},
	FamPayments: {
		Kind:     "flow",
		From:     `billing.payments p`,
		TimeExpr: `p.purchased_at`,
		DimJoins: map[string]string{
			"product_id":    paymentPriceJoin,
			"price_id":      paymentPriceJoin,
			"billing_cycle": paymentPriceJoin,
		},
		DimExprs: map[string]string{
			"currency":      `p.currency`,
			"rail":          `p.rail`,
			"psp":           `COALESCE('psp_' || p.psp_id::text, '')`,
			"stream":        streamExpr,
			"product_id":    `COALESCE('prod_' || pr.product_id::text, '')`,
			"price_id":      `pr.public_id`,
			"billing_cycle": billingCycleExpr,
			"card_brand":    `COALESCE(p.card_brand, '')`,
			"token_type":    `COALESCE(p.token_type, 'unknown')`,
			"kind":          `COALESCE(p.attempt_kind, 'unknown')`,
			"discount_code": `COALESCE(p.discount_code, '')`,
			"cash_kind":     fmt.Sprintf(reversalKindOf, "payment"),
			"payment_kind":  fmt.Sprintf(reversalKindOf, "sale"),
			"network":       `billing.card_network(p.card_brand)`,
			"refund_reason": `COALESCE(p.refund_reason, '')`,
		},
		DimValues: map[string][]string{"kind": {"initial", "renewal", "unknown"}},
	},
	FamDisputes: {
		Kind:     "flow",
		From:     `billing.disputes d`,
		TimeExpr: `COALESCE(d.chargeback_at, d.disputed_at)`,
		DimJoins: map[string]string{
			"kind": `LEFT JOIN billing.payments dp ON dp.merchant_id = d.merchant_id AND dp.id = d.payment_id`,
		},
		DimExprs: map[string]string{
			"currency":       `COALESCE(d.currency, '')`,
			"rail":           `d.rail`,
			"psp":            `'psp_' || d.psp_id::text`,
			"network":        `d.network`,
			"kind":           `COALESCE(dp.attempt_kind, 'unknown')`,
			"dispute_kind":   `d.kind`,
			"dispute_reason": `d.reason`,
			"outcome":        `d.status`,
		},
		DimValues: map[string][]string{"kind": {"initial", "renewal", "unknown"}, "outcome": {"open", "won", "lost", "closed", "unreported"}},
	},
	FamFraudReports: {
		Kind:     "flow",
		From:     `billing.fraud_reports fr`,
		TimeExpr: `fr.reported_at`,
		DimExprs: map[string]string{
			"currency": `fr.currency`,
			"rail":     `fr.rail`,
			"psp":      `'psp_' || fr.psp_id::text`,
			"network":  `fr.network`,
			"outcome":  `fr.action`,
		},
		DimValues: map[string][]string{"outcome": {"pending", "recorded", "refunded", "nothing_to_refund", "disputed"}},
	},
	// What the monitoring programs divide, per card PSP, network and day.
	FamNetwork: {
		Kind:     "flow",
		From:     `billing.network_activity_daily na`,
		TimeExpr: `na.day_at`,
		Calendar: true,
		DimExprs: map[string]string{
			"rail": `na.rail`,
			"psp":  `'psp_' || na.psp_id::text`,
		},
	},
	// The month before's, for a ratio over the previous month's sales.
	FamNetworkPrior: {
		Kind:     "flow",
		From:     `billing.network_activity_daily nap`,
		TimeExpr: `(((nap.day_at AT TIME ZONE 'UTC') + interval '1 month') AT TIME ZONE 'UTC')`,
		Calendar: true,
		DimExprs: map[string]string{
			"rail": `nap.rail`,
			"psp":  `'psp_' || nap.psp_id::text`,
		},
	},
	FamGrants: {
		Kind:     "flow",
		From:     `billing.credit_grants g`,
		TimeExpr: `g.created_at`,
		DimExprs: map[string]string{
			"currency":   `g.unit`,
			"product_id": `COALESCE('prod_' || g.product_id::text, '')`,
			"customer":   `g.customer_id::text`,
		},
		BaseWhere: `g.funding_source IN ('purchase', 'recurring')`,
	},
	// Usage is the event store's history, priced at read (usage.go): each
	// row once for what it charged and once for what grace forgave, so
	// charge splits one total. The expressions name its columns.
	FamUsage: {
		Kind: "store",
		DimExprs: map[string]string{
			"currency":  `currency`,
			"customer":  `customer_id`,
			"invoker":   `invoker`,
			"outcome":   `outcome`,
			"rate_card": `event_type`,
		},
		DimValues: map[string][]string{"outcome": usageOutcomes},
	},
	FamUsageCharged: {
		Kind: "store",
		DimExprs: map[string]string{
			"currency":  `currency`,
			"customer":  `customer_id`,
			"invoker":   `invoker`,
			"outcome":   `outcome`,
			"rate_card": `event_type`,
			"charge":    `charge`,
		},
		DimValues: map[string][]string{"outcome": usageOutcomes},
	},
	FamLimitsReached: {
		Kind:     "flow",
		From:     `billing.limit_windows lw`,
		TimeExpr: `lw.reached_at`,
		DimExprs: map[string]string{
			"limit":    `lw.limit_key`,
			"customer": `lw.customer_id::text`,
		},
		// Spend windows are policy, not declared limits.
		BaseWhere: `lw.reached_at IS NOT NULL AND lw.limit_key NOT LIKE 'spend:%'`,
	},
	FamSubsSnapshot: {
		Kind: "snapshot",
		From: `billing.subscriptions s LEFT JOIN billing.prices pr ON pr.merchant_id = s.merchant_id AND pr.id = s.price_id`,
		// Interval predicate: sub existed at t.
		BaseWhere: `s.started_at <= edge.bucket AND (s.ended_at IS NULL OR s.ended_at > edge.bucket)`,
		DimJoins:  map[string]string{},
		DimExprs: map[string]string{
			"currency":      `COALESCE(pr.currency, '')`,
			"rail":          `s.rail`,
			"psp":           `COALESCE('psp_' || s.psp_id::text, '')`,
			"product_id":    `'prod_' || s.product_id::text`,
			"price_id":      `COALESCE(pr.public_id, '')`,
			"billing_cycle": billingCycleExpr,
		},
	},
	FamEntitlSnapshot: {
		Kind: "snapshot",
		From: `billing.product_access e
		JOIN billing.product_entitlements pe ON pe.merchant_id = e.merchant_id AND pe.product_id = e.product_id`,
		BaseWhere: `e.starts_at <= edge.bucket AND (e.ends_at IS NULL OR e.ends_at > edge.bucket)
		  AND (e.revoked_at IS NULL OR e.revoked_at > edge.bucket) AND e.deleted_at IS NULL
		  AND pe.added_at <= edge.bucket AND (pe.removed_at IS NULL OR pe.removed_at > edge.bucket)`,
		DimExprs: map[string]string{
			"entitlement": `pe.entitlement`,
			"product_id":  `'prod_' || e.product_id::text`,
		},
	},
	FamBalance: {
		Kind: "balance",
		From: `billing.ledger_transfers lt
		JOIN billing.ledger_accounts da ON da.merchant_id = lt.merchant_id AND da.id = lt.debit_account_id
		JOIN billing.ledger_accounts ca ON ca.merchant_id = lt.merchant_id AND ca.id = lt.credit_account_id`,
		BaseWhere: `lt.book = 'units'`,
		TimeExpr:  `lt.created_at`,
		DimExprs: map[string]string{
			"currency": `lt.currency`,
		},
	},
	FamDepletion: {
		Kind: "depletion",
	},
	FamWebhookHealth: {
		Kind: "snapshot",
		From: `billing.webhook_health wh
		LEFT JOIN billing.psps whp ON whp.merchant_id = wh.merchant_id AND whp.id = wh.psp_id
		LEFT JOIN billing.custodians whc ON whc.merchant_id = wh.merchant_id AND whc.id = wh.custodian_id`,
		DimExprs: map[string]string{
			"rail": `COALESCE(whp.rail, whc.kind)`,
			"psp":  `COALESCE('psp_' || wh.psp_id::text, '')`,
		},
	},
	FamWebhookDaily: {
		Kind: "flow",
		From: `billing.webhook_health_daily whd
		LEFT JOIN billing.psps whp ON whp.merchant_id = whd.merchant_id AND whp.id = whd.psp_id
		LEFT JOIN billing.custodians whc ON whc.merchant_id = whd.merchant_id AND whc.id = whd.custodian_id`,
		TimeExpr: `whd.day_at`,
		Calendar: true,
		DimExprs: map[string]string{
			"rail": `COALESCE(whp.rail, whc.kind)`,
			"psp":  `COALESCE('psp_' || whd.psp_id::text, '')`,
		},
	},
	FamAttempts: {
		Kind:     "flow",
		From:     `billing.payment_attempts a`,
		TimeExpr: `a.attempted_at`,
		DimExprs: map[string]string{
			"currency":               `COALESCE(a.currency, '')`,
			"rail":                   `a.rail`,
			"psp":                    `COALESCE('psp_' || a.psp_id::text, '')`,
			"owner":                  `a.owner`,
			"kind":                   `a.kind`,
			"card_entry":             `a.card_entry`,
			"source":                 `a.source`,
			"observed_via":           `a.observed_via`,
			"decline_cause":          `COALESCE(a.decline_cause, '')`,
			"reason":                 `COALESCE(a.reason, '')`,
			"response_code":          `COALESCE(a.response_code, '')`,
			"issuer_code":            `COALESCE(a.issuer_code, '')`,
			"avs_result":             `COALESCE(a.avs_result, '')`,
			"cvv_result":             `COALESCE(a.cvv_result, '')`,
			"card_brand":             `COALESCE(a.card_brand, '')`,
			"card_bin":               `COALESCE(a.card_bin, '')`,
			"token_type":             `COALESCE(a.token_type, 'unknown')`,
			"outcome":                `a.outcome`,
			"advice_code":            `COALESCE(a.advice_code, '')`,
			"network_advice_code":    `COALESCE(a.network_advice_code, '')`,
			"visa_category":          visaCategory("a"),
			"network_transaction_id": networkTransactionID("a"),
		},
		DimValues: map[string][]string{
			"kind":    {"verify", "initial", "upgrade", "rebill", "dunning_retry", "customer_retry", "collection"},
			"outcome": attemptOutcomes,
		},
	},
	FamPurchases: {
		Kind:     "flow",
		From:     purchasesFrom,
		TimeExpr: `ck.started_at`,
		DimExprs: map[string]string{
			"currency":   `ck.currency`,
			"rail":       `ck.rail`,
			"psp":        `COALESCE('psp_' || ck.psp_id::text, '')`,
			"owner":      `ck.owner`,
			"card_entry": `CASE WHEN ck.new_card THEN 'new' ELSE 'saved' END`,
			"outcome":    purchaseOutcome,
		},
		DimValues: map[string][]string{"outcome": {"approved", "recovered", "failed", "open"}},
	},
	FamRebillCycles: {
		Kind:     "flow",
		From:     rebillCyclesFrom,
		TimeExpr: `cy.due_at`,
		DimExprs: map[string]string{
			"currency":               `cy.currency`,
			"rail":                   `cy.rail`,
			"psp":                    `COALESCE('psp_' || cy.psp_id::text, '')`,
			"owner":                  `cy.owner`,
			"outcome":                rebillOutcome,
			"first_outcome":          `cy.first_outcome`,
			"first_decline_cause":    `COALESCE(cy.first_cause, '')`,
			"card_brand":             `COALESCE(cy.first_card_brand, '')`,
			"card_bin":               `COALESCE(cy.first_card_bin, '')`,
			"token_type":             `COALESCE(cy.first_token_type, 'unknown')`,
			"network_transaction_id": `COALESCE(cy.first_transaction_link, '')`,
			"first_failure_reason":   `COALESCE(cy.first_reason, '')`,
			"miss_reason":            `COALESCE(cy.miss_reason, '')`,
			"recovered_by":           `cy.recovered_by`,
			"recovery_attempt": `CASE WHEN NOT cy.first_failed OR cy.won_ordinal IS NULL THEN ''
				WHEN cy.won_ordinal >= 5 THEN '5+' ELSE cy.won_ordinal::text END`,
			"days_to_recover": `CASE WHEN NOT cy.first_failed OR cy.won_at IS NULL THEN ''
				ELSE floor(EXTRACT(EPOCH FROM cy.won_at - COALESCE(cy.missed_at, cy.first_at)) / 86400)::bigint::text END`,
		},
		DimValues: map[string][]string{"outcome": {"approved", "recovered", "lost", "open"}, "network_transaction_id": {"", "sent", "missing", "none"}},
	},
	FamFindings: {
		Kind: "snapshot",
		From: `billing.reconciliation_findings f`,
		// Open at t: seen by t and unresolved then. A finding that reopened
		// counts from its first sighting.
		BaseWhere: `f.created_at <= edge.bucket AND ((f.resolved_at IS NULL AND f.status IN ('requires_review','reconcile_required')) OR f.resolved_at > edge.bucket)`,
		DimExprs: map[string]string{
			"finding_type":   `f.finding_type`,
			"finding_group":  findingGroup,
			"severity":       `f.severity`,
			"finding_status": `CASE WHEN f.status IN ('reconcile_required','auto_fixed') THEN 'reconcile_required' ELSE 'requires_review' END`,
		},
	},
	FamNMIHistory: {
		Kind:     "flow",
		From:     `billing.nmi_history_months h`,
		TimeExpr: `h.month_at`,
		Calendar: true,
		DimExprs: map[string]string{
			"psp":           `COALESCE('psp_' || h.psp_id::text, '')`,
			"nmi_kind":      `h.kind`,
			"decline_cause": `COALESCE(h.decline_cause, '')`,
			"reason":        `h.reason`,
			"outcome":       `h.outcome`,
		},
		DimValues: map[string][]string{"outcome": attemptOutcomes},
	},
}

// A renewal the engine owns is missed when nothing attempted it by its
// deadline after the period ended and none is in flight: the due pass admits
// one within minutes, and a Solana one's pull runs PullMargin after the
// boundary. Read as of the query, never recorded.
const (
	EngineRenewalDeadline = time.Hour
	SolanaRenewalDeadline = solanasubs.PullMargin + EngineRenewalDeadline
)

// rebillCycles are the recorded cycles and the engine renewals missed as of
// @to (a cycle row exists only once attempted).
var rebillCycles = fmt.Sprintf(`(SELECT c.merchant_id, c.id, c.subscription_id, c.psp_id, c.rail, c.owner, c.due_at, c.currency, c.missed_at, c.miss_reason
		FROM billing.rebill_cycles c
	UNION ALL
	SELECT s.merchant_id, md5('renewal:' || s.id::text || ':' || extract(epoch FROM s.current_period_ends_at)::text)::uuid, s.id, s.psp_id, s.rail,
		'engine'::text, s.current_period_ends_at, pr.currency, s.current_period_ends_at + %[1]s,
		CASE WHEN s.status = 'awaiting_method' THEN 'method_unusable'
			WHEN EXISTS (SELECT 1 FROM billing.reconciliation_findings f
				WHERE f.merchant_id = s.merchant_id AND f.finding_type = 'life.due_pass.refused' AND f.subject_key = s.id::text
				AND f.status IN ('reconcile_required', 'requires_review')) THEN 'refused'
			ELSE 'not_attempted' END::text
		FROM billing.subscriptions s
		JOIN billing.prices pr ON pr.merchant_id = s.merchant_id AND pr.id = s.price_id
		WHERE s.collection_policy = 'engine' AND s.status IN ('active', 'awaiting_method') AND s.deleted_at IS NULL
		AND s.current_period_ends_at + %[1]s <= @to
		AND NOT EXISTS (SELECT 1 FROM billing.rebill_cycles c
			WHERE c.merchant_id = s.merchant_id AND c.subscription_id = s.id AND c.due_at = s.current_period_ends_at)
		AND NOT EXISTS (SELECT 1 FROM billing.provider_intents i
			WHERE i.merchant_id = s.merchant_id AND i.subscription_id = s.id
			AND i.intent_type IN ('subscription_collection', 'solana_pull')
			AND i.status IN ('pending', 'in_flight', 'unknown_needs_verify', 'failed_retryable')))`,
	fmt.Sprintf("CASE WHEN s.rail = 'solana' THEN interval '%d seconds' ELSE interval '%d seconds' END",
		int(SolanaRenewalDeadline.Seconds()), int(EngineRenewalDeadline.Seconds())))

// Each rebill cycle with its first attempt, the attempt that collected it and
// when it closes: collected, the subscription canceled, or 15 days past due
// (the dunning window is at most 14). ListRenewals derives the same facts.
var rebillCyclesFrom = `(SELECT c.merchant_id, c.id, c.psp_id, c.rail, c.owner, c.due_at, c.currency, c.missed_at, c.miss_reason,
		f.outcome AS first_attempt_outcome, f.decline_cause AS first_cause, f.reason AS first_reason, f.attempted_at AS first_at,
		f.card_brand AS first_card_brand, f.card_bin AS first_card_bin, f.token_type AS first_token_type, f.transaction_link AS first_transaction_link,
		w.attempted_at AS won_at, w.ordinal AS won_ordinal,
		(c.missed_at IS NOT NULL OR COALESCE(f.outcome <> 'approved', false)) AS first_failed,
		CASE WHEN c.missed_at IS NOT NULL THEN 'missed'
			WHEN f.outcome IS NULL THEN 'pending'
			ELSE f.outcome END AS first_outcome,
		LEAST(w.attempted_at, CASE WHEN s.canceled_at IS NOT NULL THEN GREATEST(s.canceled_at, c.due_at) END, c.due_at + interval '15 days') AS closed_at,
		CASE WHEN w.id IS NULL OR NOT (c.missed_at IS NOT NULL OR COALESCE(f.outcome <> 'approved', false)) THEN ''
			WHEN w.source = 'provider_schedule' THEN 'late_provider_charge'
			WHEN EXISTS (SELECT 1 FROM billing.payment_method_versions u
				WHERE u.merchant_id = c.merchant_id AND u.payment_method_id = w.payment_method_id
				AND u.kind IN ('updated', 'brand_changed')
				AND u.effective_at >= COALESCE(c.missed_at, f.attempted_at) AND u.effective_at <= w.attempted_at) THEN 'updated_card'
			WHEN w.kind = 'customer_retry' THEN 'customer_retry'
			ELSE 'dunning_retry' END AS recovered_by
	FROM ` + rebillCycles + ` c
	LEFT JOIN LATERAL (SELECT a.outcome, a.decline_cause, a.reason, a.attempted_at, a.card_brand, a.card_bin, a.token_type,
			` + networkTransactionID("a") + ` AS transaction_link FROM billing.payment_attempts a
		WHERE a.merchant_id = c.merchant_id AND a.cycle_id = c.id ORDER BY a.attempted_at, a.id LIMIT 1) f ON true
	LEFT JOIN LATERAL (SELECT a.id, a.kind, a.source, a.attempted_at, a.payment_method_id,
			(SELECT count(*) FROM billing.payment_attempts b
				WHERE b.merchant_id = c.merchant_id AND b.cycle_id = c.id AND (b.attempted_at, b.id) <= (a.attempted_at, a.id)) AS ordinal
		FROM billing.payment_attempts a
		WHERE a.merchant_id = c.merchant_id AND a.cycle_id = c.id AND a.outcome = 'approved'
		ORDER BY a.attempted_at, a.id LIMIT 1) w ON true
	LEFT JOIN billing.subscriptions s ON s.merchant_id = c.merchant_id AND s.id = c.subscription_id AND s.deleted_at IS NULL) cy`

// rebillOutcome is a cycle as of the range end: approved when its first try
// collected it, recovered when a later one did, lost when it closed
// uncollected, open otherwise.
const rebillOutcome = `CASE WHEN cy.won_at <= @to AND NOT cy.first_failed THEN 'approved'
	WHEN cy.won_at <= @to THEN 'recovered'
	WHEN cy.closed_at <= @to THEN 'lost'
	ELSE 'open' END`

// A purchase is one buyer's attempts on one target (purchase_id). It is
// approved when its target is: a verification for a card save, an initial or
// upgrade charge for a purchase.
const purchasesFrom = `(SELECT a.merchant_id, a.purchase_id,
		MIN(a.attempted_at) AS started_at, MAX(a.attempted_at) AS last_at, COUNT(*) AS attempts,
		bool_or(a.outcome = 'approved' AND CASE WHEN a.purchase_target = 'card_save' THEN a.kind = 'verify' ELSE a.kind IN ('initial', 'upgrade') END) AS approved,
		bool_or(a.outcome <> 'approved') AS had_failure,
		bool_or(a.card_entry = 'new') AS new_card,
		(array_agg(a.rail ORDER BY a.attempted_at, a.id))[1] AS rail,
		(array_agg(a.psp_id ORDER BY a.attempted_at, a.id))[1] AS psp_id,
		(array_agg(a.owner ORDER BY a.attempted_at, a.id))[1] AS owner,
		COALESCE((array_agg(a.currency ORDER BY a.attempted_at, a.id) FILTER (WHERE a.currency IS NOT NULL))[1], '') AS currency
	FROM billing.payment_attempts a
	WHERE a.purchase_id IS NOT NULL
	GROUP BY a.merchant_id, a.purchase_id) ck`

// purchaseOutcome: approved at the first try, recovered after a failure,
// failed when quiet for an hour before the range end without an approval,
// open otherwise.
const purchaseOutcome = `CASE WHEN ck.approved AND NOT ck.had_failure THEN 'approved'
	WHEN ck.approved THEN 'recovered'
	WHEN ck.last_at < @to - interval '1 hour' THEN 'failed'
	ELSE 'open' END`

// findingGroup sorts open findings into what they mean for the merchant.
var findingGroup = `CASE WHEN f.finding_type IN (` + quoted(reconcile.OrphanedFindingTypes) + `) THEN 'orphaned'
	WHEN f.finding_type IN (` + quoted(reconcile.FreeloaderFindingTypes) + `) THEN 'freeloader'
	WHEN f.finding_type IN (` + quoted(reconcile.DuplicateCoverageFindingTypes) + `) THEN 'duplicate_coverage'
	ELSE 'other' END`

func quoted(values []string) string {
	out := make([]string, len(values))
	for i, v := range values {
		if strings.ContainsRune(v, '\'') {
			panic(fmt.Sprintf("metrics: %q cannot be a SQL literal", v))
		}
		out[i] = "'" + v + "'"
	}
	return strings.Join(out, ", ")
}

// Dimension sets.
var (
	attemptDims  = []string{"currency", "rail", "psp", "owner", "kind", "card_entry", "source", "observed_via", "decline_cause", "reason", "response_code", "issuer_code", "avs_result", "cvv_result", "card_brand", "card_bin", "token_type", "advice_code", "network_advice_code", "visa_category", "network_transaction_id"}
	purchaseDims = []string{"currency", "rail", "psp", "owner", "card_entry"}
	cycleDims    = []string{"currency", "rail", "psp", "owner", "first_outcome", "first_decline_cause", "first_failure_reason", "miss_reason", "recovered_by", "recovery_attempt", "days_to_recover", "card_brand", "card_bin", "token_type", "network_transaction_id"}
	nmiHistDims  = []string{"psp", "nmi_kind", "decline_cause", "reason"}
	cashDims     = []string{"currency", "rail", "psp", "stream", "product_id", "price_id", "billing_cycle", "card_brand", "network", "kind", "discount_code"}
	disputeDims  = []string{"currency", "rail", "psp", "network", "kind", "dispute_kind", "dispute_reason", "outcome"}
	networkDims  = []string{"rail", "psp"}
	usageDims    = []string{"currency", "customer", "invoker", "outcome", "rate_card"}
	creditDims   = []string{"currency", "product_id", "customer"}
	webhookDims  = []string{"rail", "psp"}
	// What a sale and its reversals both carry.
	reversalRateDims = []string{"currency", "rail", "psp", "stream", "product_id", "price_id", "billing_cycle", "card_brand", "network"}
)

// with is dims and more, copied.
func with(dims []string, more ...string) []string {
	return append(slices.Clone(dims), more...)
}

// MRR and its customers at t: the totals less the flows of the buckets from t
// on (a voluntary or chargeback churn row is a part of churn's).
const (
	mrrAt = `COALESCE(SUM(t.mrr - COALESCE((SELECT SUM(f.amount) FROM billing.mrr_flows f
		WHERE f.merchant_id = t.merchant_id AND f.currency = t.currency AND f.measure NOT IN ('churn_voluntary', 'churn_chargeback')
		  AND f.bucket_at >= edge.bucket), 0)), 0)::bigint`
	subscribersAt = `COALESCE(SUM(t.customers - COALESCE((SELECT SUM(f.logos) FROM billing.mrr_flows f
		WHERE f.merchant_id = t.merchant_id AND f.currency = t.currency AND f.measure NOT IN ('churn_voluntary', 'churn_chargeback')
		  AND f.bucket_at >= edge.bucket), 0)), 0)::bigint`
)

// productAt is a product's total column at t: its totals now less its flows
// of the buckets from t on.
func productAt(flow, total string) string {
	return `COALESCE(SUM(pt.` + total + ` - COALESCE((SELECT SUM(pf.` + flow + `) FROM billing.mrr_product_flows pf
		WHERE pf.merchant_id = pt.merchant_id AND pf.currency = pt.currency AND pf.product_id = pt.product_id
		  AND pf.bucket_at >= edge.bucket), 0)), 0)::bigint`
}

// verificationPressure is an unverified subscription past its paid-through
// date at t.
const verificationPressure = `s.status = 'unverified' AND s.deleted_at IS NULL AND s.current_period_ends_at < edge.bucket`

// UnitMoney marks a measure whose cells are MoneyCell values: exact native
// units of the row's currency (the registry scale, not always millionths).
const UnitMoney = "money"

// depletionRiskDays: a customer is at depletion risk when their prepaid balance
// covers <= this many days of their trailing-7d average burn.
const depletionRiskDays = 7

// count is COUNT(*) over the rows admits (all when empty) and extra narrow.
func count(admits string) func(string) string {
	return func(extra string) string {
		if where := both(admits, extra); where != "" {
			return `COUNT(*) FILTER (WHERE ` + where + `)`
		}
		return `COUNT(*)`
	}
}

// sum is the bigint sum of value over the rows admits and extra narrow.
func sum(value, admits string) func(string) string {
	return func(extra string) string {
		if where := both(admits, extra); where != "" {
			return `COALESCE(SUM(` + value + `) FILTER (WHERE ` + where + `), 0)::bigint`
		}
		return `COALESCE(SUM(` + value + `), 0)::bigint`
	}
}

func both(a, b string) string {
	switch {
	case a == "":
		return b
	case b == "":
		return a
	}
	return "(" + a + ") AND (" + b + ")"
}

// Measures is the measure registry: the declared measures completed by
// registry (sliced ratio components are internal measures).
var Measures = registry(declared)

var declared = []Measure{
	// --- cash and payments ------------------------------------------------------
	{Name: "cash", Class: ClassAdditive, Family: FamPayments, Money: true, Unit: UnitMoney, Direction: UpIsGood,
		Description: "money received less money returned, signed by cash_kind: payment and dispute_won add, refund and chargeback subtract; the total is net cash, gross of processor fees",
		Formula:     "SUM(signed amount) over settled payments and their reversals",
		Agg:         sum(`p.amount`, settledRow),
		Dims:        with(cashDims, "cash_kind", "refund_reason")},
	{Name: "payments", Class: ClassAdditive, Family: FamPayments, Unit: "count", Direction: Neutral,
		Description: "settled payments and reversals, by payment_kind: sale, refund, chargeback, dispute_won",
		Formula:     "COUNT(settled payments and reversals)",
		Agg:         count(settledRow),
		Dims:        with(cashDims, "token_type", "payment_kind", "refund_reason")},
	{Name: "refund_rate", Class: ClassRatio, Unit: "ratio", Direction: DownIsGood,
		Num: "payments", NumSlice: Slice{"payment_kind": {"refund"}}, Den: "payments", DenSlice: Slice{"payment_kind": {"sale"}},
		Description: "refunds / settled sales, by count, in the same bucket",
		Formula:     "payments(refund) / payments(sale)",
		Dims:        reversalRateDims},
	{Name: "chargeback_rate", Class: ClassRatio, Unit: "ratio", Direction: DownIsGood,
		Num: "payments", NumSlice: Slice{"payment_kind": {"chargeback"}}, Den: "payments", DenSlice: Slice{"payment_kind": {"sale"}},
		Description: "chargebacks / settled sales, by count, in the same bucket; group by psp and network for each PSP's on each network. The networks' programs divide otherwise: vamp_ratio and ecm_ratio",
		Formula:     "payments(chargeback) / payments(sale)",
		Dims:        reversalRateDims},
	{Name: "vamp_count", Class: ClassAdditive, Family: FamNetwork, Unit: "count", Internal: true,
		Expr: `COALESCE(SUM(na.chargebacks + na.fraud_reports) FILTER (WHERE na.network = 'visa'), 0)::bigint`, Dims: networkDims},
	{Name: "vamp_sales", Class: ClassAdditive, Family: FamNetwork, Unit: "count", Internal: true,
		Expr: `COALESCE(SUM(na.sales) FILTER (WHERE na.network = 'visa'), 0)::bigint`, Dims: networkDims},
	{Name: "vamp_ratio", Class: ClassRatio, Unit: "ratio", Direction: DownIsGood, Num: "vamp_count", Den: "vamp_sales",
		Description: "Visa's VAMP ratio as the dispute monitor counts it: Visa chargebacks and fraud reports / settled Visa sales, in the same bucket (use grain month); its lines are VAMP's levels",
		Formula:     "(visa chargebacks + visa fraud reports) / visa sales",
		Thresholds:  disputerules.ProgramLines(billing.ProgramVisaVAMP), Program: billing.ProgramVisaVAMP,
		Dims: networkDims},
	{Name: "ecm_count", Class: ClassAdditive, Family: FamNetwork, Unit: "count", Internal: true,
		Expr: `COALESCE(SUM(na.chargebacks) FILTER (WHERE na.network = 'mastercard'), 0)::bigint`, Dims: networkDims},
	{Name: "ecm_prior_sales", Class: ClassAdditive, Family: FamNetworkPrior, Unit: "count", Internal: true,
		Expr: `COALESCE(SUM(nap.sales) FILTER (WHERE nap.network = 'mastercard'), 0)::bigint`, Dims: networkDims},
	{Name: "ecm_ratio", Class: ClassRatio, Unit: "ratio", Direction: DownIsGood, Num: "ecm_count", Den: "ecm_prior_sales",
		Description: "Mastercard's excessive chargeback ratio as the dispute monitor counts it: Mastercard chargebacks / settled Mastercard sales of the month before (use grain month); its lines are its levels",
		Formula:     "mastercard chargebacks / mastercard sales a month earlier",
		Thresholds:  disputerules.ProgramLines(billing.ProgramMastercardECP), Program: billing.ProgramMastercardECP,
		Dims: networkDims},
	{Name: "disputes", Class: ClassAdditive, Family: FamDisputes, Unit: "count", Direction: DownIsGood,
		Description: "provider dispute cases opened in the bucket, by dispute_kind (inquiry, chargeback, prevented), dispute_reason, outcome and kind (the disputed charge: initial or renewal)",
		Formula:     "COUNT(disputes opened in the bucket)",
		Agg:         count(""),
		Dims:        disputeDims},
	{Name: "dispute_win_rate", Class: ClassRatio, Unit: "ratio", Direction: UpIsGood,
		Num: "disputes", NumSlice: Slice{"outcome": {"won"}}, Den: "disputes", DenSlice: Slice{"outcome": {"won", "lost"}},
		Description: "disputes won / disputes decided (won or lost), by when they opened; NMI and CCBill report no outcome",
		Formula:     "disputes(won) / disputes(won, lost)"},
	{Name: "fraud_reports", Class: ClassAdditive, Family: FamFraudReports, Unit: "count", Direction: DownIsGood,
		Description: "issuer fraud reports (Visa TC40, Mastercard SAFE) a rail passed on, by outcome: what OpenRails did (recorded, refunded, nothing_to_refund, disputed); Stripe passes them on as early fraud warnings",
		Formula:     "COUNT(fraud reports in the bucket)",
		Agg:         count(""),
		Dims:        []string{"currency", "rail", "psp", "network", "outcome"}},
	// --- subscriptions ------------------------------------------------------------
	{Name: "mrr", Class: ClassSnapshot, Family: FamMRR, Money: true, Unit: UnitMoney, Direction: UpIsGood,
		Description: "monthly recurring revenue at t: each subscription's price normalised to a month times its seats (trials count none, a cancel at period end until its end), from the MRR rollups; by product_id, each product's",
		Formula:     "MRR now - SUM(MRR movements from t on); by product, the product's MRR now - SUM(its flows from t on)",
		Expr:        mrrAt, ByProduct: productAt("amount", "mrr"),
		Dims: []string{"currency", "product_id"}},
	{Name: "subscribers", Class: ClassSnapshot, Family: FamMRR, Unit: "count", Direction: UpIsGood,
		Description: "customers with MRR at t: without a currency each customer once, by currency those with MRR in it; by product_id, the product's subscriptions adding MRR",
		Formula:     "customers with MRR now - SUM(their changes from t on)",
		Expr:        subscribersAt, ByProduct: productAt("subscribers", "subscribers"),
		ByCustomer: `COALESCE(SUM(ct.customers - COALESCE((SELECT SUM(cf.logos) FROM billing.mrr_customer_flows cf
			WHERE cf.merchant_id = ct.merchant_id AND cf.bucket_at >= edge.bucket), 0)), 0)::bigint`,
		Dims: []string{"currency", "product_id"}},
	{Name: "mrr_at_period_start", Class: ClassSnapshot, Family: FamMRR, Money: true, Unit: UnitMoney, Internal: true,
		SnapshotAtPeriodStart: true, Expr: mrrAt, Dims: []string{"currency"}},
	{Name: "subscribers_at_period_start", Class: ClassSnapshot, Family: FamMRR, Unit: "count", Internal: true,
		SnapshotAtPeriodStart: true, Expr: subscribersAt, Dims: []string{"currency", "churn_type"}},
	{Name: "mrr_movements", Class: ClassAdditive, Family: FamMRRFlows, Money: true, Unit: UnitMoney, Direction: UpIsGood,
		Description: "MRR change in the bucket by movement: new, expansion, contraction, churn, reactivation, opening (an import adopting subscriptions), correction (a prune or its undo); its total is net new MRR",
		Formula:     "SUM(MRR movements)",
		Expr:        `COALESCE(SUM(f.amount), 0)::bigint`,
		Dims:        []string{"currency", "movement"}},
	{Name: "subscriber_movements", Class: ClassAdditive, Family: FamMRRFlows, Unit: "count", Direction: UpIsGood,
		Description: "change in customers with MRR in the bucket by movement: new, reactivation, churn (negative), opening, correction; its total is net new subscribers",
		Formula:     "SUM(customers gaining MRR from none - customers losing all of it)",
		Expr:        `COALESCE(SUM(f.logos), 0)::bigint`,
		Dims:        []string{"currency", "movement"}},
	{Name: "scheduled_mrr", Class: ClassAdditive, Family: FamScheduled, Money: true, Unit: UnitMoney, Direction: Neutral,
		Description: "MRR still scheduled to change, by when it takes effect and movement: churn (a cancel at period end) and contraction (negative) or expansion (a scheduled change at a renewal)",
		Formula:     "SUM(pending scheduled MRR changes effective in the bucket)",
		Expr:        `COALESCE(SUM(ms.mrr_delta), 0)::bigint`,
		Dims:        []string{"currency", "movement"}},
	{Name: "cancellations", Class: ClassAdditive, Family: FamCancellations, Unit: "count", Direction: DownIsGood,
		Description: "cancel decisions, at period end or at once, dated when they were made, by cancel_type and reversed (a resume took it back since)",
		Formula:     "COUNT(subscriptions moved into canceled)",
		Expr:        `COUNT(*)`,
		Dims:        []string{"currency", "rail", "psp", "product_id", "price_id", "cancel_type", "reversed"}},
	{Name: "churned_subscribers", Class: ClassAdditive, Family: FamMRRChurn, Unit: "count", Internal: true,
		Expr: `COALESCE(-SUM(sf.logos * ct.sign) FILTER (WHERE sf.streak_at < @period_start), 0)::bigint`, Dims: []string{"currency", "churn_type"}},
	{Name: "base_pruned", Class: ClassAdditive, Family: FamMRRStreak, Unit: "count", Internal: true,
		Expr: `COALESCE(SUM(-sf.logos) FILTER (WHERE sf.streak_at < @period_start AND sf.measure = 'correction'), 0)::bigint`, Dims: []string{"currency", "churn_type"}},
	{Name: "churn_base", Class: ClassDifference, Unit: "count", Internal: true,
		Num: "subscribers_at_period_start", Den: "base_pruned", Dims: []string{"currency", "churn_type"}},
	{Name: "subscriber_churn_rate", Class: ClassRatio, Unit: "ratio", Direction: DownIsGood, Num: "churned_subscribers", Den: "churn_base", PerMonth: true,
		Description: "the period's own churn as a monthly rate: of the customers with MRR at its start, less those a prune took out in it (neither churned nor retained), the share whose MRR went to none in it; a period of m months in the accounting timezone with rate p reads 1 - (1 - p)^(1/m), a calendar month as counted; by churn_type, each type alone",
		Formula:     "1 - (1 - churned customers of the opening base / (customers with MRR at period start - those pruned in it))^(1 / months in the period)",
		Dims:        []string{"currency", "churn_type"}},
	{Name: "trailing_base", Class: ClassSnapshot, Family: FamMRRTrailing, Unit: "count", Internal: true,
		Expr: `COALESCE(SUM(t.customers - COALESCE((SELECT SUM(f.logos) FROM billing.mrr_flows f
			WHERE f.merchant_id = t.merchant_id AND f.currency = t.currency AND f.measure NOT IN ('churn_voluntary', 'churn_chargeback')
			  AND f.bucket_at >= w.starts_at), 0)
			+ COALESCE((SELECT SUM(sf.logos) FROM billing.mrr_streak_flows sf
			WHERE sf.merchant_id = t.merchant_id AND sf.currency = t.currency AND sf.measure = 'correction'
			  AND sf.bucket_at >= w.starts_at AND sf.bucket_at < w.ends_at AND sf.streak_at < w.starts_at), 0)), 0)::bigint`,
		Dims: []string{"currency"}},
	{Name: "trailing_churned", Class: ClassSnapshot, Family: FamMRRTrailing, Unit: "count", Internal: true,
		Expr: `COALESCE(SUM(-COALESCE((SELECT SUM(sf.logos) FROM billing.mrr_streak_flows sf
			WHERE sf.merchant_id = t.merchant_id AND sf.currency = t.currency AND sf.measure = 'churn'
			  AND sf.bucket_at >= w.starts_at AND sf.bucket_at < w.ends_at AND sf.streak_at < w.starts_at), 0)), 0)::bigint`,
		Dims: []string{"currency"}},
	{Name: "trailing_churn_rate", Class: ClassRatio, Unit: "ratio", Internal: true, Num: "trailing_churned", Den: "trailing_base", PerMonth: true, Trailing: true,
		Dims: []string{"currency"}},
	{Name: "base_mrr_drop", Class: ClassAdditive, Family: FamMRRStreak, Money: true, Unit: UnitMoney, Internal: true,
		Expr: `COALESCE(-SUM(sf.amount) FILTER (WHERE sf.streak_at < @period_start AND sf.measure IN ('expansion', 'contraction', 'churn')), 0)::bigint`, Dims: []string{"currency"}},
	{Name: "base_mrr_lost", Class: ClassAdditive, Family: FamMRRStreak, Money: true, Unit: UnitMoney, Internal: true,
		Expr: `COALESCE(-SUM(sf.amount) FILTER (WHERE sf.streak_at < @period_start AND sf.measure IN ('contraction', 'churn')), 0)::bigint`, Dims: []string{"currency"}},
	{Name: "base_mrr_corrected", Class: ClassAdditive, Family: FamMRRStreak, Money: true, Unit: UnitMoney, Internal: true,
		Expr: `COALESCE(-SUM(sf.amount) FILTER (WHERE sf.streak_at < @period_start AND sf.measure = 'correction'), 0)::bigint`, Dims: []string{"currency"}},
	{Name: "retention_base", Class: ClassDifference, Money: true, Unit: UnitMoney, Internal: true,
		Num: "mrr_at_period_start", Den: "base_mrr_corrected", Dims: []string{"currency"}},
	{Name: "nrr_retained", Class: ClassDifference, Money: true, Unit: UnitMoney, Internal: true,
		Num: "retention_base", Den: "base_mrr_drop", Dims: []string{"currency"}},
	{Name: "grr_retained", Class: ClassDifference, Money: true, Unit: UnitMoney, Internal: true,
		Num: "retention_base", Den: "base_mrr_lost", Dims: []string{"currency"}},
	{Name: "nrr", Class: ClassRatio, Money: true, Unit: "ratio", Direction: UpIsGood, Num: "nrr_retained", Den: "retention_base",
		Description: "net revenue retention over the period: of the customers with MRR at its start, their MRR then plus their expansion and less their contraction and churn in it, / their MRR then; a correction (a prune or its undo) is neither churn nor retained and leaves the base; monthly NRR is grain month",
		Formula:     "(base MRR + expansion - contraction - churn of the opening base) / base MRR, base MRR = MRR at start less the base's corrections",
		Dims:        []string{"currency"}},
	{Name: "grr", Class: ClassRatio, Money: true, Unit: "ratio", Direction: UpIsGood, Num: "grr_retained", Den: "retention_base",
		Description: "gross revenue retention: nrr without expansion, at most 100%",
		Formula:     "(base MRR - contraction - churn of the opening base) / base MRR",
		Dims:        []string{"currency"}},
	{Name: "arpa", Class: ClassRatio, Money: true, Unit: UnitMoney, Direction: UpIsGood, Num: "mrr", Den: "subscribers",
		Description: "average MRR per customer with MRR at t",
		Formula:     "mrr / subscribers",
		Dims:        []string{"currency"}},
	{Name: "ltv", Class: ClassRatio, Money: true, Unit: UnitMoney, Direction: UpIsGood, Num: "arpa", Den: "trailing_churn_rate",
		Description: "lifetime value, revenue only: arpa / the monthly subscriber churn rate over the trailing three months ending at the bucket's end (the range's end without time), at every grain",
		Formula:     "arpa / monthly subscriber churn over the trailing 3 months",
		Dims:        []string{"currency"}},
	{Name: "cohort_value", Class: ClassAdditive, Family: FamCohort, Unit: "count", Internal: true,
		Dims: []string{"currency", "cohort", "months_since_start", "basis"}},
	{Name: "cohort_size", Class: ClassAdditive, Family: FamCohort, Unit: "count", Internal: true,
		Dims: []string{"currency", "cohort", "months_since_start", "basis"}},
	{Name: "cohort_retention", Class: ClassRatio, Money: true, Unit: "ratio", Direction: UpIsGood, Untimed: true, Num: "cohort_value", Den: "cohort_size",
		Description: "each cohort (the customers who first had MRR in a month) at the end of each month since, by cohort and months_since_start; basis subscribers (default): its members with MRR / the members it acquired, at most 100%; basis mrr: their MRR / their MRR at the end of month 0, above 100% with expansion. No time grouping: the range picks the cohorts",
		Formula:     "subscribers: cohort members with MRR at the end of month k / members acquired; mrr: cohort MRR at the end of month k / at the end of month 0",
		Dims:        []string{"currency", "cohort", "months_since_start", "basis"}},
	{Name: "mrr_at_risk", Class: ClassSnapshot, Family: FamRiskTotals, Money: true, Unit: UnitMoney, Direction: DownIsGood,
		Description: "MRR of subscriptions at risk at t, by risk_reason; without it each subscription counts once whatever its reasons",
		Formula:     "at-risk MRR now − SUM(at-risk MRR flows from t on)",
		Expr: `COALESCE(SUM(t.mrr - COALESCE((SELECT SUM(f.amount) FROM billing.mrr_risk_flows f
			WHERE f.merchant_id = t.merchant_id AND f.currency = t.currency AND f.reason = t.reason AND f.bucket_at >= edge.bucket), 0)), 0)::bigint`,
		Dims: []string{"currency", "risk_reason"}},
	{Name: "at_risk_recovered_mrr", Class: ClassAdditive, Family: FamRiskFlows, Money: true, Unit: UnitMoney, Internal: true,
		Expr: `COALESCE(SUM(rf.recovered_mrr), 0)::bigint`, Dims: []string{"currency", "risk_reason"}},
	{Name: "at_risk_closed_mrr", Class: ClassAdditive, Family: FamRiskFlows, Money: true, Unit: UnitMoney, Internal: true,
		Expr: `COALESCE(SUM(rf.recovered_mrr + rf.churned_mrr), 0)::bigint`, Dims: []string{"currency", "risk_reason"}},
	{Name: "at_risk_recovery_rate", Class: ClassRatio, Money: true, Unit: "ratio", Direction: UpIsGood, Num: "at_risk_recovered_mrr", Den: "at_risk_closed_mrr",
		Description: "recovered MRR over recovered and churned MRR of the at-risk cases closed in the bucket, by risk_reason the case entered with",
		Formula:     "SUM(recovered MRR) / SUM(recovered + churned MRR) of closed cases",
		Dims:        []string{"currency", "risk_reason"}},
	// --- payment health: one measure per funnel, its rates slice it ---------------
	{Name: "attempts", Class: ClassAdditive, Family: FamAttempts, Unit: "count", Direction: Neutral,
		Description: "authorizations a PSP answered (card verifications, sales, rebills and retries), by outcome (approved, declined, refused, error) and decline_cause; the decline mix is attempts by decline_cause with outcome declined",
		Formula:     "COUNT(payment attempts)",
		Agg:         count(""),
		Dims:        with(attemptDims, "outcome")},
	{Name: "authorization_rate", Class: ClassRatio, Unit: "ratio", Direction: UpIsGood,
		Num: "attempts", NumSlice: Slice{"outcome": {"approved"}}, Den: "attempts", DenSlice: Slice{"outcome": {"approved", "declined"}},
		Description: "approved / authorizations the issuer answered; refusals by the merchant or its gateway and errors are left out. kind=rebill is each renewal's first try, kind in (initial, rebill) Visa's first attempts",
		Formula:     "attempts(approved) / attempts(approved, declined)"},
	{Name: "issuer_discretion_decline_rate", Class: ClassRatio, Unit: "ratio", Direction: DownIsGood,
		Num: "attempts", NumSlice: Slice{"outcome": {"declined"}, "decline_cause": {"issuer", "fraud"}}, Den: "attempts", DenSlice: Slice{"outcome": {"approved", "declined"}},
		Description: "declines at the issuer's discretion (do not honor, generic, not permitted, restricted, suspected fraud) / authorizations the issuer answered",
		Formula:     "attempts(declined: issuer, fraud) / attempts(approved, declined)"},
	{Name: "purchases", Class: ClassAdditive, Family: FamPurchases, Unit: "count", Direction: Neutral,
		Description: "purchases (a buyer's attempts on one order or card save) started in the bucket, by outcome as of the range end: approved at the first try, recovered after a failure, failed (no approval and quiet for an hour), open",
		Formula:     "COUNT(purchases)",
		Agg:         count(""),
		Dims:        with(purchaseDims, "outcome")},
	{Name: "rebills", Class: ClassAdditive, Family: FamRebillCycles, Unit: "count", Direction: Neutral,
		Description: "rebill cycles by the period that came due, attempted or missed; by outcome as of the range end: approved (the first try collected), recovered (collected after a failed or missed first try), lost (closed uncollected), open (in dunning)",
		Formula:     "COUNT(rebill cycles)",
		Agg:         count(""),
		Dims:        with(cycleDims, "outcome")},
	{Name: "first_try_rebill_approval_rate", Class: ClassRatio, Unit: "ratio", Direction: UpIsGood,
		Num: "rebills", NumSlice: Slice{"first_outcome": {"approved"}}, Den: "rebills", DenSlice: Slice{"first_outcome": {"approved", "declined", "refused", "error"}},
		Description: "renewals whose first attempt was approved / renewals with a first attempt, by the period that came due; a missed renewal is rebills by miss_reason",
		Formula:     "rebills(first_outcome approved) / rebills(first_outcome approved, declined, refused, error)"},
	{Name: "rebill_rate", Class: ClassRatio, Unit: "ratio", Direction: UpIsGood,
		Num: "rebills", NumSlice: Slice{"outcome": {"approved", "recovered"}}, Den: "rebills", DenSlice: Slice{"outcome": {"approved", "recovered", "lost"}},
		Description: "collected / closed rebill cycles: the share of renewals that came due and were paid",
		Formula:     "rebills(approved, recovered) / rebills(approved, recovered, lost)"},
	{Name: "dunning_recovery_rate", Class: ClassRatio, Unit: "ratio", Direction: UpIsGood,
		Num: "rebills", NumSlice: Slice{"outcome": {"recovered"}},
		Den: "rebills", DenSlice: Slice{"outcome": {"recovered", "lost"}, "first_outcome": {"declined", "refused", "error", "missed"}},
		Description: "collected / closed cycles whose first rebill failed or was missed",
		Formula:     "rebills(recovered) / rebills(recovered, lost with first_outcome declined, refused, error or missed)",
		Dims:        cycleDims},
	{Name: "credits_sold", Class: ClassAdditive, Family: FamGrants, Money: true, Unit: UnitMoney, Direction: UpIsGood,
		Description: "prepaid credit lots purchased (cash-in, native currency units); NOT recognized revenue until consumed",
		Formula:     "SUM(grant lot amounts) over purchased credit grants",
		Expr:        `COALESCE(SUM(g.units), 0)::bigint`,
		Dims:        creditDims},
	{Name: "credit_topups", Class: ClassAdditive, Family: FamGrants, Unit: "count", Internal: true,
		Expr: `COUNT(*)`,
		Dims: creditDims},
	{Name: "repeat_topups", Class: ClassAdditive, Family: FamGrants, Unit: "count", Internal: true,
		Expr: `COALESCE(SUM(CASE WHEN EXISTS (
			SELECT 1 FROM billing.credit_grants g2
			WHERE g2.merchant_id = g.merchant_id AND g2.customer_id = g.customer_id
			  AND g2.funding_source IN ('purchase', 'recurring')
			  AND g2.created_at < g.created_at
		) THEN 1 ELSE 0 END), 0)::bigint`,
		Dims: creditDims},
	{Name: "repeat_topup_rate", Class: ClassRatio, Unit: "ratio", Direction: UpIsGood, Num: "repeat_topups", Den: "credit_topups",
		Description: "share of credit purchases in the bucket made by customers with an earlier credit purchase",
		Formula:     "repeat top-ups / all top-ups",
		Dims:        []string{"currency", "product_id"}},
	{Name: "usage_charged", Class: ClassAdditive, Family: FamUsageCharged, Money: true, Unit: UnitMoney, Direction: UpIsGood,
		Description: "priced usage from usage history, by charge: charged (the day's share of its period's price, and failed usage charged) or forgiven (failed usage the customer's grace absorbed, or usage in a merchant's unit its lots could not take when it posted, moved out of charged); cash-in is credits_sold",
		Formula:     "SUM(charged amount) + SUM(forgiven amount) of usage history",
		Agg:         sum(`uc.amount`, ""),
		Dims:        with(usageDims, "charge")},
	{Name: "usage_units", Class: ClassAdditive, Family: FamUsage, Unit: "count", Direction: UpIsGood,
		Description: "count of settled usage items (raw volume; per-dimension token counts live host-side)",
		Formula:     "SUM(requests) of usage history's event rows",
		Expr:        `COALESCE(SUM(ud.requests) FILTER (WHERE ud.meter_key = ''), 0)::bigint`,
		Dims:        usageDims},
	{Name: "active_customers", Class: ClassDistinct, Family: FamUsage, Unit: "count", Direction: UpIsGood,
		Description: "distinct customers with any usage in the bucket (the API platform's WAU/MAU)",
		Formula:     "COUNT(DISTINCT customers) over usage history",
		Expr:        `COUNT(DISTINCT ud.customer_id)`,
		Dims:        []string{"currency", "rate_card"}},
	{Name: "limits_reached", Class: ClassAdditive, Family: FamLimitsReached, Unit: "count", Direction: Neutral,
		Description: "limit windows that reached capacity, dated when they first did",
		Formula:     "COUNT(limit windows by reached_at)",
		Expr:        `COUNT(*)`,
		Dims:        []string{"limit", "customer"}},
	{Name: "outstanding_credit_liability", Class: ClassSnapshot, Family: FamBalance, Money: true, Unit: UnitMoney, Direction: Neutral,
		Description: "unconsumed prepaid credit at t, at face: net customer_units across the units book",
		Formula:     "SUM(customer balances) at t",
		Expr: `COALESCE(SUM(
			CASE WHEN ca.account_type = 'customer_units' THEN lt.amount ELSE 0 END
			- CASE WHEN da.account_type = 'customer_units' THEN lt.amount ELSE 0 END), 0)::bigint`,
		Dims: []string{"currency"}},
	{Name: "outstanding_owed", Class: ClassSnapshot, Family: FamBalance, Money: true, Unit: UnitMoney, Direction: DownIsGood,
		Description: "arrears accounts receivable at t: accrued-but-unpaid usage debt",
		Formula:     "SUM(arrears owed) at t",
		Expr: `COALESCE(SUM(
			CASE WHEN da.account_type = 'customer_owed' THEN lt.amount ELSE 0 END
			- CASE WHEN ca.account_type = 'customer_owed' THEN lt.amount ELSE 0 END), 0)::bigint`,
		Dims: []string{"currency"}},
	{Name: "customers_at_depletion_risk", Class: ClassSnapshot, Family: FamDepletion, Unit: "count", Direction: DownIsGood,
		Description: "customers whose prepaid balance at t covers <= 7 days of their trailing-7d burn (in any currency)",
		Formula:     "COUNT(customers with balance / (7d burn / 7) <= 7 days)",
		Dims:        []string{}},
	// --- ops: operator signals, off the business overview ------------------------------
	{Name: "open_findings", Class: ClassSnapshot, Family: FamFindings, Unit: "count", Group: GroupOps, Direction: DownIsGood,
		Description: "findings open at t: problems OpenRails found that need a person or that it is repairing (finding_status), by finding_group: orphaned (payment without access; nonzero after a full sweep means the billing state machine is failing), freeloader (access without payment), duplicate_coverage, other",
		Formula:     "COUNT(findings open at t)",
		Expr:        `COUNT(f.id)`,
		Dims:        []string{"finding_type", "finding_group", "severity", "finding_status"}},
	{Name: "verification_pressure", Class: ClassSnapshot, Family: FamSubsSnapshot, Unit: "count", Group: GroupOps, Direction: DownIsGood,
		Description: "subscriptions awaiting provider verification past their paid-through date at t; its age trending up means verification (pull, probe, converge) is down",
		Formula:     "COUNT(unverified subscriptions past paid-through at t)",
		Expr:        `COUNT(s.id) FILTER (WHERE ` + verificationPressure + `)`,
		Dims:        webhookDims},
	{Name: "verification_pressure_age_seconds", Class: ClassSnapshot, Family: FamSubsSnapshot, Unit: "seconds", Group: GroupOps, Direction: DownIsGood,
		Description: "seconds the oldest unverified subscription is past its paid-through date at t (0 when none)",
		Formula:     "MAX(t - paid-through) over unverified subscriptions past it",
		Expr:        `COALESCE(MAX(EXTRACT(EPOCH FROM (edge.bucket - s.current_period_ends_at))) FILTER (WHERE ` + verificationPressure + `), 0)::float8`,
		Dims:        webhookDims},
	{Name: "webhook_silence_age_seconds", Class: ClassSnapshot, Family: FamWebhookHealth, Unit: "seconds", Group: GroupOps, Direction: DownIsGood,
		Description: "seconds since the last signature-VERIFIED inbound webhook per PSP at t (since tracking began when none was ever accepted); rejects never advance it",
		Formula:     "t - the last accepted webhook (or the PSP's creation), per PSP",
		Expr:        `MAX(GREATEST(EXTRACT(EPOCH FROM (edge.bucket - COALESCE(wh.last_accepted_at, wh.created_at))), 0))::float8`,
		Dims:        webhookDims},
	{Name: "webhook_rejects", Class: ClassAdditive, Family: FamWebhookDaily, Unit: "count", Group: GroupOps, Direction: DownIsGood,
		Description: "inbound webhooks that FAILED verification (bad signature / disallowed source), UTC-day buckets — a spike means webhooks arrive but the signing secret is wrong",
		Formula:     "SUM(daily rejected counters)",
		Expr:        `COALESCE(SUM(whd.rejected), 0)::bigint`,
		Dims:        webhookDims},
	{Name: "webhook_drift_events", Class: ClassAdditive, Family: FamWebhookDaily, Unit: "count", Group: GroupOps, Direction: DownIsGood,
		Description: "provider-state corrections that arrived by PULL with no webhook accepted since the previous pull (partial webhook breakage), UTC-day buckets",
		Formula:     "SUM(daily drift counters)",
		Expr:        `COALESCE(SUM(whd.drift), 0)::bigint`,
		Dims:        webhookDims},
	{Name: "nmi_history_authorizations", Class: ClassAdditive, Family: FamNMIHistory, Unit: "count", Group: GroupOps, Direction: Neutral,
		Description: "authorizations NMI answered, read daily from its transaction history (card verifications, one-off sales and NMI-scheduled rebills, whoever sent them), by outcome (approved, declined, refused, error) and decline_cause; dated by month",
		Formula:     "SUM(authorizations in NMI's history)",
		Agg:         sum(`h.authorizations`, ""),
		Dims:        with(nmiHistDims, "outcome")},
	{Name: "entitled_customers", Class: ClassSnapshot, Family: FamEntitlSnapshot, Unit: "count", Group: GroupOps, Direction: UpIsGood,
		Description: "distinct customers holding a live entitlement at t: a key of a product they hold (includes timed/comped access, not just subscribers)",
		Formula:     "COUNT(DISTINCT customers) with an entitlement at t",
		Expr:        `COUNT(DISTINCT e.customer_id)`,
		Dims:        []string{"entitlement", "product_id"}},
}

// Retired is what replaces a removed measure: Measure to request instead
// (empty when only a planned one does) and How to ask it.
type Retired struct{ Measure, How string }

// Removed names each retired measure's replacement; a query naming one is
// refused with it. Pre-launch there are no aliases.
var Removed = map[string]Retired{
	"gross_revenue":                 {"cash", "with by cash_kind: payment"},
	"net_revenue":                   {"cash", "its total"},
	"refunds":                       {"cash", "with by cash_kind: refund, signed negative"},
	"chargebacks":                   {"cash", "with by cash_kind: chargeback and dispute_won, signed"},
	"payment_count":                 {"payments", "with by payment_kind: sale"},
	"refund_count":                  {"payments", "with by payment_kind: refund"},
	"chargeback_count":              {"payments", "with by payment_kind: chargeback"},
	"unique_rebilled_customers":     {"rebills", "renewal cycles by outcome"},
	"realized_revenue_per_customer": {"arpa", "with ltv"},
	"avg_membership_duration_days":  {"cohort_retention", "with ltv"},
	"approved_attempts":             {"attempts", "with filters outcome: approved"},
	"failed_attempts":               {"attempts", "with filters outcome: declined, refused, error"},
	"failed_purchases":              {"purchases", "with filters outcome: failed"},
	"purchase_recovery_rate":        {"purchases", "by outcome: recovered against approved, recovered and failed"},
	"attempts_per_purchase":         {"attempts", "divided by purchases"},
	"rebills_due":                   {"rebills", "its total"},
	"rebills_open":                  {"rebills", "with filters outcome: open"},
	"rebills_attempted":             {"rebills", "with filters first_outcome: approved, declined, refused, error, missed"},
	"rebill_first_failures":         {"rebills", "with filters first_outcome: declined, refused, error, missed"},
	"rebills_missed":                {"rebills", "with filters first_outcome: missed, by miss_reason"},
	"dunning_recovered":             {"rebills", "with filters outcome: recovered"},
	"rebill_collection_rate":        {"rebill_rate", "renamed"},
	"rebill_missed_rate":            {"rebills", "with filters first_outcome: missed, by miss_reason (the ops view)"},
	"rebill_loss_rate":              {"rebill_rate", "1 - rebill_rate"},
	"nmi_history_approved":          {"nmi_history_authorizations", "with filters outcome: approved"},
	"nmi_history_refused":           {"nmi_history_authorizations", "with filters outcome: declined, refused, error"},
	"nmi_history_refusal_rate":      {"nmi_history_authorizations", "by outcome"},
	"usage_revenue":                 {"usage_charged", "with filters charge: charged"},
	"forgiven_usage":                {"usage_charged", "with filters charge: forgiven"},
	"credit_utilization":            {"usage_charged", "beside credits_sold"},
	"billable_subscriptions":        {"subscribers", "less scheduled_mrr by movement churn"},
	"subscriptions":                 {"subscribers", "customers with MRR; by product_id, the product's subscriptions; mrr_at_risk by risk_reason for the ones in trouble"},
	"new_subscriptions":             {"subscriber_movements", "with filters movement: new, reactivation"},
	"net_new_subscriptions":         {"subscriber_movements", "its total"},
	"churn_rate":                    {"subscriber_churn_rate", "customers whose MRR went to none / customers with MRR at the start"},
	"attempt_failure_rate":          {"authorization_rate", "1 - authorization_rate; declines by decline_cause"},
	"purchase_failure_rate":         {"purchases", "with by outcome: failed against approved, recovered and failed; authorization_rate"},
	"rebill_first_failure_rate":     {"first_try_rebill_approval_rate", "1 - first_try_rebill_approval_rate"},
	"orphaned_members":              {"open_findings", "with filters finding_group: orphaned"},
	"freeloaders":                   {"open_findings", "with filters finding_group: freeloader"},
	"duplicate_coverage":            {"open_findings", "with filters finding_group: duplicate_coverage"},
}

// RemovedDimensions names each retired dimension's replacement.
var RemovedDimensions = map[string]string{"attempt_kind": "kind", "category": "decline_cause", "first_failure_category": "first_decline_cause", "status": "risk_reason"}

// Deferred names the measures later phases build (MRR movements, retention,
// the books, trials); naming one is refused as planned.
var Deferred = []string{
	"recognized_revenue", "balance", "trials", "trial_conversion_rate", "revenue_nrr",
}

// Caveats are documented limitations surfaced verbatim in /schema.
var Caveats = []string{
	"all money is GROSS OF PROCESSOR FEES (fees are not captured per-charge); cash differs from bank deposits by fee amounts",
	"money never sums across currencies; when a money measure is grouped without a single-currency filter, 'currency' is added as an implicit group-by dimension",
	"cash is signed: refunds and chargebacks are negative, a won dispute's return positive; its total is net cash",
	"MRR measures read the MRR rollups: a value at a past t is today's totals less the movements recorded after t, so a correction (a prune or its undo) moves the past it corrects; netted per customer and currency per transaction",
	"snapshot series evaluate at each bucket's START instant (UTC); without a time grouping they evaluate at the range end (balance measures: strictly-before semantics)",
	"token_type exists from its instrumentation onward: NULL/legacy rows read 'unknown' and are excluded from token_type-filtered analyses; kind on payments exists from instrumentation onward; older and imported rows read 'unknown'. Declines are measured on attempts, not payments: Stripe- and CCBill-owned renewals are recorded as their providers report them",
	"attempts, purchases and rebill cycles begin at their instrumentation: earlier history lives in payments and the provider. Purchases and cycles are dated by their first attempt and due date; their outcome is as of the range end: a purchase quiet for an hour has failed, a cycle past its 15-day dunning window is lost",
	"nmi_history measures are NMI's own transaction history, read daily per NMI PSP and kept 25 months: they include what OpenRails sent and what came before it. Rows are dated by their month's first instant, so use month-aligned ranges. one_off_sale cannot separate initial sales from retries of declined rebills",
	"limits_reached counts each window of a declared limit once, when settled and held usage first took it to capacity; a rate or lifetime limit's one window counts its first time only",
	"time buckets zero-fill: a bucket present with zeros means genuinely zero activity, not missing data",
}

// registry completes the declared measures: a funnel measure's Expr from its
// Agg, each sliced ratio's components as internal measures, every measure's
// dimension values. It checks the whole registry, so a bad entry fails at
// start, never in a query.
func registry(declared []Measure) []Measure {
	out := slices.Clone(declared)
	index := map[string]int{}
	for i := range out {
		m := &out[i]
		if _, dup := index[m.Name]; dup {
			panic("metrics: measure " + m.Name + " declared twice")
		}
		index[m.Name] = i
		if m.Agg != nil {
			m.Expr = m.Agg("")
		}
		if m.Group == "" {
			m.Group = GroupBusiness
		}
		if !m.Internal && m.Direction == "" {
			panic("metrics: measure " + m.Name + " declares no direction")
		}
		if _, gone := Removed[m.Name]; gone {
			panic("metrics: measure " + m.Name + " is both live and removed")
		}
	}
	var parts []Measure
	for i := range out {
		m := &out[i]
		if !m.composite() {
			continue
		}
		num, okNum := index[m.Num]
		den, okDen := index[m.Den]
		if !okNum || !okDen {
			panic("metrics: measure " + m.Name + " names a missing component")
		}
		base := slices.Clone(out[num].Dims)
		base = slices.DeleteFunc(base, func(d string) bool { return !out[den].allowsDim(d) })
		if m.NumSlice != nil {
			parts = append(parts, slicePart(m.Name+"__num", out[num], m.NumSlice))
			m.Num = m.Name + "__num"
		}
		if m.DenSlice != nil {
			parts = append(parts, slicePart(m.Name+"__den", out[den], m.DenSlice))
			m.Den = m.Name + "__den"
		}
		if m.Dims == nil {
			m.Dims = slices.DeleteFunc(base, func(d string) bool { return m.NumSlice[d] != nil || m.DenSlice[d] != nil })
		}
		for _, d := range m.Dims {
			if !slices.Contains(base, d) {
				panic("metrics: measure " + m.Name + " allows " + d + ", which a component lacks")
			}
		}
	}
	out = append(out, parts...)
	byName := map[string]*Measure{}
	for i := range out {
		byName[out[i].Name] = &out[i]
	}
	for i := range out {
		m := &out[i]
		fam := m.Family
		for leaf := m; leaf.composite(); leaf = byName[leaf.Num] {
			fam = byName[leaf.Num].Family
		}
		spec := families[fam]
		m.values = map[string][]string{}
		for _, d := range m.Dims {
			dim, ok := dimensionByName[d]
			if !ok {
				panic("metrics: measure " + m.Name + " allows unknown dimension " + d)
			}
			if v, ok := spec.DimValues[d]; ok {
				m.values[d] = v
			} else {
				m.values[d] = dim.Values
			}
		}
	}
	return out
}

// slicePart is the internal measure counting base over one slice.
func slicePart(name string, base Measure, s Slice) Measure {
	if base.Agg == nil {
		panic("metrics: " + name + " slices " + base.Name + ", which declares no Agg")
	}
	spec := families[base.Family]
	dims := make([]string, 0, len(s))
	for d := range s {
		dims = append(dims, d)
	}
	slices.Sort(dims)
	preds := make([]string, 0, len(dims))
	for _, d := range dims {
		expr, ok := spec.DimExprs[d]
		if !ok || !base.allowsDim(d) {
			panic("metrics: " + name + " slices " + base.Name + " by " + d + ", which it lacks")
		}
		if _, joined := spec.DimJoins[d]; joined {
			panic("metrics: " + name + " slices by " + d + ", which needs a join")
		}
		vocabulary, ok := spec.DimValues[d]
		if !ok {
			vocabulary = dimensionByName[d].Values
		}
		for _, v := range s[d] {
			if vocabulary != nil && !slices.Contains(vocabulary, v) {
				panic(fmt.Sprintf("metrics: %s slices %s by %s=%q, not one of its values", name, base.Name, d, v))
			}
		}
		preds = append(preds, "("+expr+") IN ("+quoted(s[d])+")")
	}
	return Measure{Name: name, Class: ClassAdditive, Family: base.Family, Group: base.Group, Unit: base.Unit, Money: base.Money,
		Expr: base.Agg(strings.Join(preds, " AND ")), Dims: base.Dims, Internal: true}
}

// --- lookups -----------------------------------------------------------------

var measureByName = func() map[string]*Measure {
	m := make(map[string]*Measure, len(Measures))
	for i := range Measures {
		m[Measures[i].Name] = &Measures[i]
	}
	return m
}()

var dimensionByName = func() map[string]*Dimension {
	m := make(map[string]*Dimension, len(Dimensions))
	for i := range Dimensions {
		m[Dimensions[i].Name] = &Dimensions[i]
	}
	return m
}()

// PublicMeasureNames lists requestable (non-internal) measures.
func PublicMeasureNames() []string {
	out := make([]string, 0, len(Measures))
	for i := range Measures {
		if !Measures[i].Internal {
			out = append(out, Measures[i].Name)
		}
	}
	return out
}

// DimensionNames lists all dimensions (including time).
func DimensionNames() []string {
	out := make([]string, 0, len(Dimensions))
	for i := range Dimensions {
		out = append(out, Dimensions[i].Name)
	}
	return out
}

func (m *Measure) allowsDim(dim string) bool {
	return slices.Contains(m.Dims, dim)
}

// valuesOf is dim's closed vocabulary on m; nil is open.
func (m *Measure) valuesOf(dim string) []string {
	return m.values[dim]
}

// composite reports whether the measure is computed from two others.
func (m *Measure) composite() bool {
	return m.Class == ClassRatio || m.Class == ClassDifference
}

// components returns the leaf measures a measure needs computed (itself for
// a leaf; its components' leaves for a ratio or a difference).
// utcDays reports a measure any of whose components reads a rollup kept per
// UTC date (a Calendar family).
func (m *Measure) utcDays() bool {
	for _, leaf := range m.components() {
		if families[leaf.Family].Calendar {
			return true
		}
	}
	return false
}

func (m *Measure) components() []*Measure {
	if !m.composite() {
		return []*Measure{m}
	}
	num, den := measureByName[m.Num], measureByName[m.Den]
	var out []*Measure
	out = append(out, num.components()...)
	out = append(out, den.components()...)
	return out
}

// riskReasonValues are the risk_reason dimension's values.
func riskReasonValues() []string {
	var out []string
	for _, r := range billing.MRRRiskReasons() {
		out = append(out, string(r))
	}
	return out
}

// declineReasonValues are the reason dimension's values.
// visaCategory is Visa's category of a declined Visa row's issuer code.
func visaCategory(alias string) string {
	in := func(codes []string) string { return "('" + strings.Join(codes, "', '") + "')" }
	codes := decline.VisaCategoryCodes()
	return fmt.Sprintf(`CASE WHEN %[1]s.outcome = 'approved' OR %[1]s.card_brand IS DISTINCT FROM 'visa' OR %[1]s.issuer_code IS NULL THEN ''
		WHEN upper(btrim(%[1]s.issuer_code)) IN %[2]s THEN '1' WHEN upper(btrim(%[1]s.issuer_code)) IN %[3]s THEN '2'
		WHEN upper(btrim(%[1]s.issuer_code)) IN %[4]s THEN '3' ELSE '4' END`, alias, in(codes[1]), in(codes[2]), in(codes[3]))
}

// networkTransactionID is whether a row's stored-credential charge cited
// its storing transaction: an initial transaction id sent, or a Stripe card
// Stripe stored and links itself.
func networkTransactionID(alias string) string {
	return fmt.Sprintf(`CASE WHEN %[1]s.mandate_id IS NULL THEN 'none'
		WHEN %[1]s.sent_initial_transaction_id IS NOT NULL OR %[1]s.rail = 'stripe' THEN 'sent' ELSE 'missing' END`, alias)
}

func attemptOutcomeValues() []string {
	var out []string
	for _, o := range billing.AttemptOutcomes() {
		out = append(out, string(o))
	}
	return out
}

func declineCauseValues() []string {
	var out []string
	for _, c := range billing.DeclineCauses() {
		out = append(out, string(c))
	}
	return out
}

func declineReasonValues() []string {
	var out []string
	for _, r := range billing.DeclineReasons() {
		out = append(out, string(r))
	}
	slices.Sort(out)
	return out
}
