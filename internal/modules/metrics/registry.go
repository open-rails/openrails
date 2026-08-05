package metrics

// The registry is the single source of truth for the metrics vocabulary:
// it drives validation, SQL compilation, /schema, and the LLM context doc.
// SQL text comes ONLY from fragments declared here; client strings are bind
// parameters, never SQL.

// Class is a measure's aggregation class.
type Class string

const (
	ClassAdditive Class = "additive" // SUM/COUNT over the grain
	ClassDistinct Class = "distinct" // COUNT(DISTINCT ...) per group; non-additive across buckets
	ClassRatio    Class = "ratio"    // numerator/denominator aggregated first, divided per row
	ClassSnapshot Class = "snapshot" // point-in-time; never summed across buckets
)

// Family is an internal source family: one SQL statement is issued per family
// involved in a query. Invisible to clients.
type Family string

const (
	FamPayments       Family = "payments"        // flow over openrails.payments (purchased_at)
	FamSubsNew        Family = "subs_new"        // flow over subscriptions (started_at)
	FamSubsCancelled  Family = "subs_cancelled"  // flow over subscriptions (cancelled_at)
	FamSubsEnded      Family = "subs_ended"      // flow over subscriptions (ended_at)
	FamGrants         Family = "grants"          // flow over credit-purchase grant lots (created_at)
	FamUsage          Family = "usage"           // flow over usage_events (occurred_at)
	FamTransitions    Family = "transitions"     // flow over subscription_status_transitions (occurred_at)
	FamDenials        Family = "denials"         // flow over admission_denials_hourly (hour_at)
	FamSubsSnapshot   Family = "subs_snapshot"   // interval reconstruction over subscriptions
	FamEntitlSnapshot Family = "entitl_snapshot" // interval reconstruction over entitlements
	FamBalance        Family = "balance"         // cumulative sums over ledger_transfers
	FamDepletion      Family = "depletion"       // per-payer balance vs trailing-7d burn
	FamWebhookHealth  Family = "webhook_health"  // snapshot over webhook_health watermarks (#786)
	FamWebhookDaily   Family = "webhook_daily"   // flow over webhook_health_daily counter buckets (#786)
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
	// BaseWhere: family-invariant predicate (no params).
	BaseWhere string
}

// Measure is one registry entry.
type Measure struct {
	Name        string
	Class       Class
	Family      Family
	Description string // one line, token-lean (rides in LLM context)
	Formula     string // /schema formula text
	Unit        string // "micros" | "count" | "ratio" | "days" | "seconds"
	// Expr is the aggregate SQL expression (flow + snapshot families).
	Expr string
	// Dims are the allowed group-by/filter dimensions (besides time).
	Dims []string
	// Money marks currency-sensitive measures (implicit currency group-by).
	Money bool
	// Ratio components (ClassRatio): resolved through the registry; may be
	// internal-only measures.
	Num, Den string
	// Internal measures are compiler components: not requestable, not in /schema.
	Internal bool
	// SnapshotAtPeriodStart: snapshot evaluated at bucket/range START instead of
	// bucket start/range END (used by churn's denominator).
	SnapshotAtPeriodStart bool
}

// Dimension is one group-by/filter axis.
type Dimension struct {
	Name        string
	Description string
	Values      []string // closed vocabulary when non-nil (validated on filters)
}

const (
	// monthlyNormExpr converts a price to a monthly-normalized amount (micros):
	// cycles >= ~27d divide by the rounded month count; shorter cycles scale up
	// by 730h/month. Deterministic; pinned by tests.
	monthlyNormExpr = `CASE
		WHEN pr.access_duration_hours IS NULL THEN 0
		WHEN pr.access_duration_hours >= 648 THEN round(pr.amount::numeric / GREATEST(round(pr.access_duration_hours / 730.0), 1))::bigint
		ELSE round(pr.amount::numeric * 730.0 / pr.access_duration_hours)::bigint END`

	// billingCycleExpr labels a price's cadence from access_duration_hours.
	billingCycleExpr = `CASE
		WHEN pr.access_duration_hours IS NULL THEN 'one_time'
		WHEN pr.access_duration_hours < 48 THEN 'daily'
		WHEN pr.access_duration_hours < 336 THEN 'weekly'
		WHEN pr.access_duration_hours < 1440 THEN 'monthly'
		WHEN pr.access_duration_hours < 3600 THEN 'quarterly'
		WHEN pr.access_duration_hours < 6480 THEN 'semiannual'
		ELSE 'annual' END`

	// streamExpr classifies a payment's revenue stream.
	streamExpr = `CASE
		WHEN p.subscription_id IS NOT NULL THEN 'subscription'
		WHEN p.credits_spec_snapshot IS NOT NULL AND p.credits_spec_snapshot::text NOT IN ('{}', 'null') THEN 'usage'
		ELSE 'one_time' END`

	// saleRows / mirror rows: a sale row has refunded_payment_id NULL; refund and
	// chargeback mirrors carry refunded_payment_id + reversal_kind.
	saleSettled = `p.status IN ('completed','refunded') AND p.refunded_payment_id IS NULL`
)

// Dimensions is the dimension registry.
var Dimensions = []Dimension{
	{Name: "time", Description: "bucket by the query grain (day|week|month|quarter|year, UTC calendar)"},
	{Name: "currency", Description: "currency code; added implicitly when a money measure is grouped without a single-currency filter"},
	{Name: "rail", Description: "payment rail (e.g. stripe, mobius, ccbill, solana)"},
	{Name: "rail_account", Description: "operator-declared rail merchant account label; VAMP thresholds apply per account"},
	{Name: "stream", Description: "revenue stream: subscription | one_time | usage", Values: []string{"subscription", "one_time", "usage"}},
	{Name: "product_id", Description: "product UUID"},
	{Name: "price_id", Description: "price UUID"},
	{Name: "billing_cycle", Description: "price cadence: daily|weekly|monthly|quarterly|semiannual|annual|one_time", Values: []string{"daily", "weekly", "monthly", "quarterly", "semiannual", "annual", "one_time"}},
	{Name: "cancel_type", Description: "cancellation type recorded on the subscription (e.g. user, merchant, chargeback, failed_payment, expired)"},
	{Name: "status", Description: "subscription status; snapshot measures group/filter by the CURRENT status of subs whose interval covers t", Values: []string{"pending", "active", "past_due", "cancelled", "unknown"}},
	{Name: "payer", Description: "paying customer UUID (usage/admission measures)"},
	{Name: "sku", Description: "usage resource slug (usage_events.resource)"},
	{Name: "rate_card", Description: "metered event type (usage_events.event_type; the key rate cards price)"},
	{Name: "card_brand", Description: "card brand on the payment (empty when not card-based)"},
	{Name: "token_type", Description: "credential form presented at charge time (#796): network_token | pan_via_proxy (custodian-held FPAN via proxy) | psp_token (the PSP's own stored credential) | unknown (legacy/non-card); approval_rate by token_type = the network-token uplift", Values: []string{"network_token", "pan_via_proxy", "psp_token", "unknown"}},
	{Name: "attempt_kind", Description: "payment attempt kind stamped at write time: initial | renewal | unknown (pre-instrumentation or imported rows)", Values: []string{"initial", "renewal", "unknown"}},
	{Name: "failure_reason", Description: "normalized decline category on failed payments (raw code kept verbatim in failure_code)", Values: []string{"insufficient_funds", "card_declined", "expired_card", "cvv_avs", "card_unsupported", "stop_recurring", "duplicate_transaction", "fraud_suspected", "processor_error", "config_error", "generic_decline", "unknown"}},
	{Name: "subscriber_type", Description: "first_time | returning (customer had an earlier ended subscription with this merchant)", Values: []string{"first_time", "returning"}},
	{Name: "entitlement", Description: "entitlement key string (e.g. premium)"},
	{Name: "discount_code", Description: "discount code on the payment (empty = none)"},
	{Name: "denial_reason", Description: "admission denial reason", Values: []string{"insufficient_balance", "insufficient_credit", "budget_exceeded", "failure_rate_limited", "delegated_spend_not_allowed"}},
}

// families declares each family's SQL skeleton inputs.
var families = map[Family]familySpec{
	FamPayments: {
		Kind:     "flow",
		From:     `openrails.payments p`,
		TimeExpr: `p.purchased_at`,
		DimJoins: map[string]string{
			"product_id":    `LEFT JOIN openrails.prices pr ON pr.id = p.price_id`,
			"billing_cycle": `LEFT JOIN openrails.prices pr ON pr.id = p.price_id`,
			"rail_account":  `LEFT JOIN openrails.psps rma ON rma.id = p.psp_id`,
		},
		DimExprs: map[string]string{
			"currency":       `p.currency`,
			"rail":           `p.rail`,
			"rail_account":   `COALESCE(rma.account_id, 'unknown')`,
			"stream":         streamExpr,
			"product_id":     `COALESCE(pr.product_id::text, '')`,
			"price_id":       `p.price_id::text`,
			"billing_cycle":  billingCycleExpr,
			"card_brand":     `COALESCE(p.card_brand, '')`,
			"token_type":     `COALESCE(p.token_type, 'unknown')`,
			"attempt_kind":   `COALESCE(p.attempt_kind, 'unknown')`,
			"failure_reason": `COALESCE(p.failure_reason, 'unknown')`,
			"discount_code":  `COALESCE(p.discount_code, '')`,
		},
	},
	FamSubsNew: {
		Kind:     "flow",
		From:     `openrails.subscriptions s`,
		TimeExpr: `s.started_at`,
		DimJoins: map[string]string{
			"currency":      `LEFT JOIN openrails.prices pr ON pr.id = s.price_id`,
			"billing_cycle": `LEFT JOIN openrails.prices pr ON pr.id = s.price_id`,
			"rail_account":  `LEFT JOIN openrails.psps rma ON rma.id = s.psp_id`,
		},
		DimExprs: map[string]string{
			"currency":      `COALESCE(pr.currency, '')`,
			"rail":          `s.rail`,
			"rail_account":  `COALESCE(rma.account_id, 'unknown')`,
			"product_id":    `s.product_id::text`,
			"price_id":      `COALESCE(s.price_id::text, '')`,
			"billing_cycle": billingCycleExpr,
			"subscriber_type": `CASE WHEN EXISTS (
				SELECT 1 FROM openrails.subscriptions s2
				WHERE s2.merchant_id = s.merchant_id AND s2.customer_id = s.customer_id
				  AND s2.id <> s.id AND s2.ended_at IS NOT NULL AND s2.ended_at <= s.started_at
			) THEN 'returning' ELSE 'first_time' END`,
		},
	},
	FamSubsCancelled: {
		Kind:     "flow",
		From:     `openrails.subscriptions s`,
		TimeExpr: `s.cancelled_at`,
		DimJoins: map[string]string{
			"currency":     `LEFT JOIN openrails.prices pr ON pr.id = s.price_id`,
			"rail_account": `LEFT JOIN openrails.psps rma ON rma.id = s.psp_id`,
		},
		DimExprs: map[string]string{
			"currency":     `COALESCE(pr.currency, '')`,
			"rail":         `s.rail`,
			"rail_account": `COALESCE(rma.account_id, 'unknown')`,
			"product_id":   `s.product_id::text`,
			"price_id":     `COALESCE(s.price_id::text, '')`,
			"cancel_type":  `COALESCE(s.cancel_type, 'unknown')`,
		},
		BaseWhere: `s.cancelled_at IS NOT NULL`,
	},
	FamSubsEnded: {
		Kind:     "flow",
		From:     `openrails.subscriptions s`,
		TimeExpr: `s.ended_at`,
		DimExprs: map[string]string{
			"rail":        `s.rail`,
			"product_id":  `s.product_id::text`,
			"price_id":    `COALESCE(s.price_id::text, '')`,
			"cancel_type": `COALESCE(s.cancel_type, 'unknown')`,
		},
		BaseWhere: `s.ended_at IS NOT NULL`,
	},
	FamGrants: {
		Kind:     "flow",
		From:     `openrails.grants g`,
		TimeExpr: `g.created_at`,
		DimExprs: map[string]string{
			"currency":   `g.currency`,
			"product_id": `COALESCE(g.product_id::text, '')`,
			"payer":      `g.customer_id::text`,
		},
		BaseWhere: `g.kind = 'credit' AND g.event = 'grant' AND g.source_type = 'purchase'`,
	},
	FamUsage: {
		Kind:     "flow",
		From:     `openrails.usage_events ue`,
		TimeExpr: `ue.occurred_at`,
		DimExprs: map[string]string{
			"currency":  `ue.currency`,
			"payer":     `ue.customer_id::text`,
			"sku":       `COALESCE(ue.resource, '')`,
			"rate_card": `ue.event_type`,
		},
	},
	FamTransitions: {
		Kind:     "flow",
		From:     `openrails.subscription_status_transitions st`,
		TimeExpr: `st.occurred_at`,
		DimExprs: map[string]string{
			"cancel_type": `COALESCE(st.cancel_type, 'unknown')`,
		},
	},
	FamDenials: {
		Kind:     "flow",
		From:     `openrails.admission_denials_hourly ad`,
		TimeExpr: `ad.hour_at`,
		DimExprs: map[string]string{
			"denial_reason": `ad.denial_reason`,
			"payer":         `ad.customer_id::text`,
		},
	},
	FamSubsSnapshot: {
		Kind: "snapshot",
		From: `openrails.subscriptions s LEFT JOIN openrails.prices pr ON pr.id = s.price_id`,
		// Interval predicate: sub existed at t.
		BaseWhere: `s.started_at <= edge.bucket AND (s.ended_at IS NULL OR s.ended_at > edge.bucket)`,
		DimJoins: map[string]string{
			"rail_account": `LEFT JOIN openrails.psps rma ON rma.id = s.psp_id`,
		},
		DimExprs: map[string]string{
			"currency":      `COALESCE(pr.currency, '')`,
			"rail":          `s.rail`,
			"rail_account":  `COALESCE(rma.account_id, 'unknown')`,
			"product_id":    `s.product_id::text`,
			"price_id":      `COALESCE(s.price_id::text, '')`,
			"billing_cycle": billingCycleExpr,
			"status":        `s.status::text`,
		},
	},
	FamEntitlSnapshot: {
		Kind: "snapshot",
		From: `openrails.entitlements e`,
		BaseWhere: `e.start_at <= edge.bucket AND (e.end_at IS NULL OR e.end_at > edge.bucket)
		  AND (e.revoked_at IS NULL OR e.revoked_at > edge.bucket) AND e.deleted_at IS NULL`,
		DimExprs: map[string]string{
			"entitlement": `e.entitlement`,
		},
	},
	FamBalance: {
		Kind: "balance",
		From: `openrails.ledger_transfers lt
		JOIN openrails.ledger_accounts da ON da.id = lt.debit_account_id
		JOIN openrails.ledger_accounts ca ON ca.id = lt.credit_account_id`,
		TimeExpr: `lt.created_at`,
		DimExprs: map[string]string{
			"currency": `lt.currency`,
		},
	},
	FamDepletion: {
		Kind: "depletion",
	},
	FamWebhookHealth: {
		Kind: "snapshot",
		From: `openrails.webhook_health wh`,
		DimExprs: map[string]string{
			"rail": `wh.rail`,
		},
	},
	FamWebhookDaily: {
		Kind:     "flow",
		From:     `openrails.webhook_health_daily whd`,
		TimeExpr: `whd.day_at`,
		DimExprs: map[string]string{
			"rail": `whd.rail`,
		},
	},
}

// depletionRiskDays: a payer is at depletion risk when their prepaid balance
// covers <= this many days of their trailing-7d average burn.
const depletionRiskDays = 7

// Measures is the measure registry (CORE tier + internal ratio components).
var Measures = []Measure{
	// --- payments: additive money -------------------------------------------
	{Name: "gross_revenue", Class: ClassAdditive, Family: FamPayments, Money: true, Unit: "micros",
		Description: "sum of settled sale amounts (micros), before refunds/chargebacks; gross of processor fees",
		Formula:     "SUM(amount) over settled sale payments (incl. later-refunded)",
		Expr:        `COALESCE(SUM(p.amount) FILTER (WHERE ` + saleSettled + ` AND p.amount > 0), 0)::bigint`,
		Dims:        []string{"currency", "rail", "rail_account", "stream", "product_id", "price_id", "billing_cycle", "card_brand", "attempt_kind", "discount_code"}},
	{Name: "net_revenue", Class: ClassAdditive, Family: FamPayments, Money: true, Unit: "micros",
		Description: "gross_revenue minus refunds and chargebacks (net of returns, NOT net of fees); Stripe's 'net volume'",
		Formula:     "gross_revenue - refunds - chargebacks",
		Expr:        `COALESCE(SUM(p.amount) FILTER (WHERE p.status IN ('completed','refunded')), 0)::bigint`,
		Dims:        []string{"currency", "rail", "rail_account", "stream", "product_id", "price_id", "billing_cycle", "card_brand", "attempt_kind", "discount_code"}},
	{Name: "refunds", Class: ClassAdditive, Family: FamPayments, Money: true, Unit: "micros",
		Description: "sum of refunded amounts (micros, positive), from refund mirror rows",
		Formula:     "SUM(-amount) over reversal_kind='refund' mirror rows",
		Expr:        `COALESCE(SUM(-p.amount) FILTER (WHERE p.status = 'completed' AND p.reversal_kind = 'refund'), 0)::bigint`,
		Dims:        []string{"currency", "rail", "rail_account", "stream", "product_id", "price_id", "card_brand"}},
	{Name: "chargebacks", Class: ClassAdditive, Family: FamPayments, Money: true, Unit: "micros",
		Description: "sum of chargeback amounts (micros, positive), net of won disputes",
		Formula:     "SUM(-amount) over reversal_kind IN ('chargeback','dispute_reversal') mirror rows",
		Expr:        `COALESCE(SUM(-p.amount) FILTER (WHERE p.status = 'completed' AND p.reversal_kind IN ('chargeback','dispute_reversal')), 0)::bigint`,
		Dims:        []string{"currency", "rail", "rail_account", "stream", "product_id", "price_id", "card_brand"}},
	// --- payments: additive counts -------------------------------------------
	{Name: "payment_count", Class: ClassAdditive, Family: FamPayments, Unit: "count",
		Description: "count of settled sale payments (approved attempts)",
		Formula:     "COUNT(settled sale payments)",
		Expr:        `COUNT(*) FILTER (WHERE ` + saleSettled + `)`,
		Dims:        []string{"currency", "rail", "rail_account", "stream", "product_id", "price_id", "billing_cycle", "card_brand", "token_type", "attempt_kind", "discount_code"}},
	{Name: "payment_failures", Class: ClassAdditive, Family: FamPayments, Unit: "count",
		Description: "count of failed payment attempts (status=failed)",
		Formula:     "COUNT(payments with status='failed')",
		Expr:        `COUNT(*) FILTER (WHERE p.status = 'failed' AND p.refunded_payment_id IS NULL)`,
		Dims:        []string{"currency", "rail", "rail_account", "stream", "product_id", "price_id", "card_brand", "token_type", "attempt_kind", "failure_reason"}},
	{Name: "refund_count", Class: ClassAdditive, Family: FamPayments, Unit: "count",
		Description: "count of refunds issued",
		Formula:     "COUNT(reversal_kind='refund' mirror rows)",
		Expr:        `COUNT(*) FILTER (WHERE p.status = 'completed' AND p.reversal_kind = 'refund')`,
		Dims:        []string{"currency", "rail", "rail_account", "stream", "product_id", "price_id", "card_brand"}},
	{Name: "chargeback_count", Class: ClassAdditive, Family: FamPayments, Unit: "count",
		Description: "count of chargebacks/disputes received",
		Formula:     "COUNT(reversal_kind='chargeback' mirror rows)",
		Expr:        `COUNT(*) FILTER (WHERE p.status = 'completed' AND p.reversal_kind = 'chargeback')`,
		Dims:        []string{"currency", "rail", "rail_account", "stream", "product_id", "price_id", "card_brand"}},
	// --- payments: distinct ---------------------------------------------------
	{Name: "unique_failed_customers", Class: ClassDistinct, Family: FamPayments, Unit: "count",
		Description: "distinct customers with at least one failed payment attempt in the bucket",
		Formula:     "COUNT(DISTINCT customer_id) over failed payments",
		Expr:        `COUNT(DISTINCT p.customer_id) FILTER (WHERE p.status = 'failed' AND p.refunded_payment_id IS NULL)`,
		Dims:        []string{"currency", "rail", "rail_account", "attempt_kind", "failure_reason"}},
	{Name: "unique_rebilled_customers", Class: ClassDistinct, Family: FamPayments, Unit: "count",
		Description: "distinct customers successfully rebilled (settled renewal payments) in the bucket",
		Formula:     "COUNT(DISTINCT customer_id) over settled attempt_kind='renewal' payments",
		Expr:        `COUNT(DISTINCT p.customer_id) FILTER (WHERE ` + saleSettled + ` AND p.attempt_kind = 'renewal')`,
		Dims:        []string{"currency", "rail", "rail_account"}},
	{Name: "paying_customers", Class: ClassDistinct, Family: FamPayments, Unit: "count", Internal: true,
		Expr: `COUNT(DISTINCT p.customer_id) FILTER (WHERE ` + saleSettled + `)`,
		Dims: []string{"currency", "rail", "rail_account", "stream", "product_id", "price_id", "billing_cycle", "card_brand", "attempt_kind", "discount_code"}},
	{Name: "payment_attempts", Class: ClassAdditive, Family: FamPayments, Unit: "count", Internal: true,
		Expr: `COUNT(*) FILTER (WHERE (` + saleSettled + `) OR (p.status = 'failed' AND p.refunded_payment_id IS NULL))`,
		Dims: []string{"currency", "rail", "rail_account", "stream", "product_id", "price_id", "card_brand", "token_type", "attempt_kind"}},
	// --- payments: ratios ------------------------------------------------------
	{Name: "approval_rate", Class: ClassRatio, Unit: "ratio", Num: "payment_count", Den: "payment_attempts",
		Description: "parsed approvals / (parsed approvals + parsed declines): the denominator is settled sale payments plus status='failed' attempt rows ONLY — transport-ambiguous/parked outcomes never write failed rows and are excluded until resolved (verify-not-decline, #674); by token_type = the network-token auth-rate uplift (#796)",
		Formula:     "payment_count / (payment_count + payment_failures)",
		Dims:        []string{"currency", "rail", "rail_account", "stream", "product_id", "price_id", "card_brand", "token_type", "attempt_kind"}},
	{Name: "chargeback_rate", Class: ClassRatio, Unit: "ratio", Num: "chargeback_count", Den: "payment_count",
		Description: "chargebacks / settled payments; group by rail_account for the per-account VAMP number",
		Formula:     "chargeback_count / payment_count",
		Dims:        []string{"currency", "rail", "rail_account", "stream", "product_id", "price_id", "card_brand"}},
	{Name: "realized_revenue_per_customer", Class: ClassRatio, Unit: "micros", Money: true, Num: "net_revenue", Den: "paying_customers",
		Description: "net revenue per distinct paying customer in the window (LTV-to-date when windowed over lifetime)",
		Formula:     "net_revenue / COUNT(DISTINCT paying customers)",
		Dims:        []string{"currency", "rail", "rail_account", "stream", "product_id", "price_id", "billing_cycle", "attempt_kind", "discount_code"}},
	// --- subscriptions: flow ----------------------------------------------------
	{Name: "new_subscriptions", Class: ClassAdditive, Family: FamSubsNew, Unit: "count",
		Description: "subscriptions started in the bucket (excludes never-activated pending)",
		Formula:     "COUNT(subs with started_at in bucket, status <> pending)",
		// or#893: `failed` left the lifecycle enum, and naming a label the type
		// no longer has is a runtime cast error, not a no-op predicate. A
		// subscription whose first charge failed is past_due or cancelled; only
		// `pending` still means never-activated.
		Expr: `COUNT(*) FILTER (WHERE s.status <> 'pending')`,
		Dims: []string{"currency", "rail", "rail_account", "product_id", "price_id", "billing_cycle", "subscriber_type"}},
	{Name: "cancellations", Class: ClassAdditive, Family: FamSubsCancelled, Unit: "count",
		Description: "subscriptions cancelled in the bucket, by cancel_type",
		Formula:     "COUNT(subs with cancelled_at in bucket)",
		Expr:        `COUNT(*)`,
		Dims:        []string{"currency", "rail", "rail_account", "product_id", "price_id", "cancel_type"}},
	{Name: "ended_membership_days", Class: ClassAdditive, Family: FamSubsEnded, Unit: "days", Internal: true,
		Expr: `COALESCE(SUM(EXTRACT(EPOCH FROM (s.ended_at - s.started_at)) / 86400.0), 0)::float8`,
		Dims: []string{"rail", "product_id", "price_id", "cancel_type"}},
	{Name: "ended_subscriptions", Class: ClassAdditive, Family: FamSubsEnded, Unit: "count", Internal: true,
		Expr: `COUNT(*)`,
		Dims: []string{"rail", "product_id", "price_id", "cancel_type"}},
	{Name: "avg_membership_duration_days", Class: ClassRatio, Unit: "days", Num: "ended_membership_days", Den: "ended_subscriptions",
		Description: "average started->ended lifetime (days) of subs that ENDED in the bucket; survivors not counted (censoring)",
		Formula:     "SUM(ended_at - started_at) / COUNT(subs ended in bucket)",
		Dims:        []string{"rail", "product_id", "price_id", "cancel_type"}},
	{Name: "subscriptions_at_period_start", Class: ClassSnapshot, Family: FamSubsSnapshot, Unit: "count", Internal: true,
		SnapshotAtPeriodStart: true,
		Expr:                  `COUNT(s.id)`,
		Dims:                  []string{"currency", "rail", "rail_account", "product_id", "price_id", "billing_cycle", "status"}},
	{Name: "churn_rate", Class: ClassRatio, Unit: "ratio", Num: "cancellations", Den: "subscriptions_at_period_start",
		Description: "cancellations in the bucket / subscriptions existing at bucket start; currency groups by price currency",
		Formula:     "cancellations / subscriptions(at period start)",
		Dims:        []string{"currency", "rail", "product_id", "price_id"}},
	// --- transitions: dunning recovery -------------------------------------------
	{Name: "dunning_recoveries", Class: ClassAdditive, Family: FamTransitions, Unit: "count", Internal: true,
		Expr: `COUNT(*) FILTER (WHERE st.from_status = 'past_due' AND st.to_status = 'active')`,
		Dims: []string{"cancel_type"}},
	{Name: "dunning_outcomes", Class: ClassAdditive, Family: FamTransitions, Unit: "count", Internal: true,
		Expr: `COUNT(*) FILTER (WHERE st.from_status = 'past_due' AND st.to_status IN ('active','cancelled'))`,
		Dims: []string{"cancel_type"}},
	{Name: "recovery_rate", Class: ClassRatio, Unit: "ratio", Num: "dunning_recoveries", Den: "dunning_outcomes",
		Description: "past_due->active / (past_due->active + past_due->cancelled) transitions in the bucket (data from transitions-table go-live)",
		Formula:     "recovered / (recovered + lost) dunning exits",
		Dims:        []string{}},
	// --- grants / usage-credits ---------------------------------------------------
	{Name: "credits_sold", Class: ClassAdditive, Family: FamGrants, Money: true, Unit: "micros",
		Description: "prepaid credit lots purchased (cash-in, micros); NOT recognized revenue until consumed",
		Formula:     "SUM(grant lot amounts) over purchased credit grants",
		Expr:        `COALESCE(SUM(g.amount), 0)::bigint`,
		Dims:        []string{"currency", "product_id", "payer"}},
	{Name: "credit_topups", Class: ClassAdditive, Family: FamGrants, Unit: "count", Internal: true,
		Expr: `COUNT(*)`,
		Dims: []string{"currency", "product_id", "payer"}},
	{Name: "repeat_topups", Class: ClassAdditive, Family: FamGrants, Unit: "count", Internal: true,
		Expr: `COALESCE(SUM(CASE WHEN EXISTS (
			SELECT 1 FROM openrails.grants g2
			WHERE g2.merchant_id = g.merchant_id AND g2.customer_id = g.customer_id
			  AND g2.kind = 'credit' AND g2.event = 'grant' AND g2.source_type = 'purchase'
			  AND g2.created_at < g.created_at
		) THEN 1 ELSE 0 END), 0)::bigint`,
		Dims: []string{"currency", "product_id", "payer"}},
	{Name: "repeat_topup_rate", Class: ClassRatio, Unit: "ratio", Num: "repeat_topups", Den: "credit_topups",
		Description: "share of credit purchases in the bucket made by customers with an earlier credit purchase",
		Formula:     "repeat top-ups / all top-ups",
		Dims:        []string{"currency", "product_id"}},
	{Name: "usage_revenue", Class: ClassAdditive, Family: FamUsage, Money: true, Unit: "micros",
		Description: "consumed (recognized) usage spend in micros, from usage events; cash-in is credits_sold",
		Formula:     "SUM(usage_events.amount)",
		Expr:        `COALESCE(SUM(ue.amount), 0)::bigint`,
		Dims:        []string{"currency", "payer", "sku", "rate_card"}},
	{Name: "usage_units", Class: ClassAdditive, Family: FamUsage, Unit: "count",
		Description: "count of metered usage events (raw volume; per-dimension token counts live host-side)",
		Formula:     "COUNT(usage_events)",
		Expr:        `COUNT(*)`,
		Dims:        []string{"currency", "payer", "sku", "rate_card"}},
	{Name: "active_payers", Class: ClassDistinct, Family: FamUsage, Unit: "count",
		Description: "distinct payers with any usage in the bucket (the API platform's WAU/MAU)",
		Formula:     "COUNT(DISTINCT customer_id) over usage events",
		Expr:        `COUNT(DISTINCT ue.customer_id)`,
		Dims:        []string{"currency", "sku", "rate_card"}},
	{Name: "credit_utilization", Class: ClassRatio, Unit: "ratio", Money: true, Num: "usage_revenue", Den: "credits_sold",
		Description: "usage consumed / credits sold in the same window (per currency)",
		Formula:     "usage_revenue / credits_sold",
		Dims:        []string{"currency"}},
	{Name: "admission_denials", Class: ClassAdditive, Family: FamDenials, Unit: "count",
		Description: "admission denials (hourly aggregates flushed from the Redis hot path; current hour may lag one flush cycle)",
		Formula:     "SUM(hourly denial counters)",
		Expr:        `COALESCE(SUM(ad.denials), 0)::bigint`,
		Dims:        []string{"denial_reason", "payer"}},
	// --- snapshots ------------------------------------------------------------------
	{Name: "subscriptions", Class: ClassSnapshot, Family: FamSubsSnapshot, Unit: "count",
		Description: "subscriptions existing at t (interval reconstruction); filter/group by status for active/past_due/pending views",
		Formula:     "COUNT(subs with started_at <= t < ended_at)",
		Expr:        `COUNT(s.id)`,
		Dims:        []string{"currency", "rail", "rail_account", "product_id", "price_id", "billing_cycle", "status"}},
	{Name: "mrr", Class: ClassSnapshot, Family: FamSubsSnapshot, Money: true, Unit: "micros",
		Description: "monthly-normalized recurring price of auto-renew subs existing at t; group by status to split healthy vs in-dunning",
		Formula:     "SUM(monthly_normalized(price)) over auto-renew subs at t",
		Expr:        `COALESCE(SUM(` + monthlyNormExpr + `) FILTER (WHERE pr.auto_renew), 0)::bigint`,
		Dims:        []string{"currency", "rail", "rail_account", "product_id", "price_id", "billing_cycle", "status"}},
	{Name: "billable_subscriptions", Class: ClassSnapshot, Family: FamSubsSnapshot, Unit: "count",
		Description: "subs at t projected to keep billing: auto-renew price, non-terminal status, not cancelled/scheduled for deletion",
		Formula:     "COUNT(subs at t with auto_renew AND status IN (pending,active,past_due,unknown) AND cancelled_at IS NULL AND deletion_scheduled_at IS NULL)",
		Expr:        `COUNT(s.id) FILTER (WHERE pr.auto_renew AND s.status IN ('pending','active','past_due','unknown') AND s.cancelled_at IS NULL AND s.deletion_scheduled_at IS NULL)`,
		Dims:        []string{"currency", "rail", "rail_account", "product_id", "price_id", "billing_cycle"}},
	{Name: "entitled_customers", Class: ClassSnapshot, Family: FamEntitlSnapshot, Unit: "count",
		Description: "distinct customers holding a live entitlement at t (includes timed/comped access, not just subscribers)",
		Formula:     "COUNT(DISTINCT customer_id) over entitlement windows covering t",
		Expr:        `COUNT(DISTINCT e.customer_id)`,
		Dims:        []string{"entitlement"}},
	{Name: "outstanding_credit_liability", Class: ClassSnapshot, Family: FamBalance, Money: true, Unit: "micros",
		Description: "unconsumed prepaid credit (deferred revenue) at t: net customer_balance across the ledger",
		Formula:     "SUM(credits - debits) of customer_balance accounts from transfers before t",
		Expr: `COALESCE(SUM(
			CASE WHEN ca.account_type = 'customer_balance' THEN lt.amount ELSE 0 END
			- CASE WHEN da.account_type = 'customer_balance' THEN lt.amount ELSE 0 END), 0)::bigint`,
		Dims: []string{"currency"}},
	{Name: "outstanding_owed", Class: ClassSnapshot, Family: FamBalance, Money: true, Unit: "micros",
		Description: "arrears accounts receivable at t: accrued-but-unpaid usage debt",
		Formula:     "SUM(debits - credits) of arrears_liability accounts from transfers before t",
		Expr: `COALESCE(SUM(
			CASE WHEN da.account_type = 'arrears_liability' THEN lt.amount ELSE 0 END
			- CASE WHEN ca.account_type = 'arrears_liability' THEN lt.amount ELSE 0 END), 0)::bigint`,
		Dims: []string{"currency"}},
	{Name: "payers_at_depletion_risk", Class: ClassSnapshot, Family: FamDepletion, Unit: "count",
		Description: "payers whose prepaid balance at t covers <= 7 days of their trailing-7d burn (in any currency)",
		Formula:     "COUNT(payers with balance / (7d burn / 7) <= 7 days)",
		Dims:        []string{}},
	// --- webhook health (#786) --------------------------------------------------------
	{Name: "webhook_silence_age_seconds", Class: ClassSnapshot, Family: FamWebhookHealth, Unit: "seconds",
		Description: "seconds since the last signature-VERIFIED inbound webhook per rail at t (since tracking began when none was ever accepted); rejects never advance it",
		Formula:     "t - COALESCE(last_accepted_at, created_at) per (merchant, rail)",
		Expr:        `MAX(GREATEST(EXTRACT(EPOCH FROM (edge.bucket - COALESCE(wh.last_accepted_at, wh.created_at))), 0))::float8`,
		Dims:        []string{"rail"}},
	{Name: "webhook_rejects", Class: ClassAdditive, Family: FamWebhookDaily, Unit: "count",
		Description: "inbound webhooks that FAILED verification (bad signature / disallowed source), UTC-day buckets — a spike means webhooks arrive but the signing secret is wrong",
		Formula:     "SUM(daily rejected counters)",
		Expr:        `COALESCE(SUM(whd.rejected), 0)::bigint`,
		Dims:        []string{"rail"}},
	{Name: "webhook_drift_events", Class: ClassAdditive, Family: FamWebhookDaily, Unit: "count",
		Description: "provider-state corrections that arrived by PULL with no webhook accepted since the previous pull (partial webhook breakage), UTC-day buckets",
		Formula:     "SUM(daily drift counters)",
		Expr:        `COALESCE(SUM(whd.drift), 0)::bigint`,
		Dims:        []string{"rail"}},
}

// DerivedFormula documents a DERIVED-tier metric: /schema-only, composed
// client-side from CORE measures.
type DerivedFormula struct {
	Name        string `json:"name"`
	Formula     string `json:"formula"`
	Description string `json:"description"`
}

// Derived is the /schema-documented DERIVED tier (not implemented server-side).
var Derived = []DerivedFormula{
	{Name: "arr", Formula: "mrr * 12", Description: "annualized recurring revenue"},
	{Name: "arpu", Formula: "mrr / subscriptions(filter status:active)", Description: "average recurring revenue per active subscription"},
	{Name: "net_new_subscriptions", Formula: "new_subscriptions - cancellations", Description: "net member change per bucket"},
	{Name: "refund_rate", Formula: "refund_count / payment_count", Description: "refunds per settled payment"},
	{Name: "effective_unit_price", Formula: "usage_revenue / usage_units", Description: "average recognized revenue per usage event"},
	{Name: "subscription_revenue", Formula: "gross_revenue filter stream:subscription", Description: "gross revenue from the subscription stream"},
	{Name: "one_time_revenue", Formula: "gross_revenue filter stream:one_time", Description: "gross revenue from one-time products"},
	{Name: "units_sold", Formula: "payment_count filter stream:one_time", Description: "one-time product units sold"},
	{Name: "subscriptions_in_dunning", Formula: "subscriptions filter status:past_due", Description: "subs currently in dunning at t"},
}

// Deferred names measures deliberately NOT built yet (so they are not
// relitigated); add when a dashboard proves the need.
var Deferred = []string{
	"new_mrr", "churned_mrr", "reactivated_mrr", "expansion_mrr", "contraction_mrr",
	"breakage", "avg_days_to_recover", "payment_methods_expiring", "entitlement_grants", "unique_purchasers",
}

// Caveats are documented limitations surfaced verbatim in /schema.
var Caveats = []string{
	"all revenue is GROSS OF PROCESSOR FEES (fees are not captured per-charge); dashboard totals differ from bank deposits by fee amounts",
	"money never sums across currencies; when a money measure is grouped without a single-currency filter, 'currency' is added as an implicit group-by dimension",
	"snapshot measures reconstruct state from interval columns; the 'status' dimension reflects each subscription's CURRENT status, not its status at historical t",
	"snapshot series evaluate at each bucket's START instant (UTC); without a time grouping they evaluate at the range end (balance measures: strictly-before semantics)",
	"token_type exists from #796 instrumentation onward: NULL/legacy rows read 'unknown' and are excluded from token_type-filtered analyses; failure_reason/attempt_kind exist from instrumentation onward; older and imported rows read 'unknown'. CCBill renewal declines depend on CCBill webhook delivery; Stripe renewal-decline attempts are visible via subscription status transitions but may lack per-attempt failed payment rows; Solana has no card-decline taxonomy (on-chain failure codes are recorded verbatim)",
	"recovery_rate and other transition-derived numbers begin at the transitions-table go-live; earlier history is not reconstructible",
	"admission_denials are hourly aggregates flushed periodically from Redis; the current hour can lag one flush cycle",
	"avg_membership_duration_days only counts subscriptions that ENDED in the bucket (right-censored: long-lived survivors are not included until they end)",
	"time buckets zero-fill: a bucket present with zeros means genuinely zero activity, not missing data",
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
	for _, d := range m.Dims {
		if d == dim {
			return true
		}
	}
	return false
}

// components returns the leaf measures a measure needs computed (itself for
// non-ratios; num+den leaves for ratios).
func (m *Measure) components() []*Measure {
	if m.Class != ClassRatio {
		return []*Measure{m}
	}
	num, den := measureByName[m.Num], measureByName[m.Den]
	var out []*Measure
	out = append(out, num.components()...)
	out = append(out, den.components()...)
	return out
}
