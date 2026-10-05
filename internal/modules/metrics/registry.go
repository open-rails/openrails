package metrics

import (
	"fmt"
	"slices"

	"github.com/open-rails/openrails/billing"
)

// The registry is the single source of truth for the metrics vocabulary:
// it drives validation, SQL compilation, /schema, and the LLM context doc.
// SQL text comes ONLY from fragments declared here; client strings are bind
// parameters, never SQL.

// Class is a measure's aggregation class.
type Class = billing.MetricsClass

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
	FamPayments       Family = "payments"        // flow over billing.payments (purchased_at)
	FamSubsNew        Family = "subs_new"        // flow over subscriptions (started_at)
	FamSubsCanceled   Family = "subs_canceled"   // flow over subscriptions (canceled_at)
	FamSubsEnded      Family = "subs_ended"      // flow over subscriptions (ended_at)
	FamGrants         Family = "grants"          // flow over credit-purchase grant lots (created_at)
	FamUsage          Family = "usage"           // flow over usage_events (occurred_at)
	FamTransitions    Family = "transitions"     // flow over subscription_status_transitions (occurred_at)
	FamDenials        Family = "denials"         // flow over admission_denials_hourly (hour_at)
	FamSubsSnapshot   Family = "subs_snapshot"   // interval reconstruction over subscriptions
	FamEntitlSnapshot Family = "entitl_snapshot" // interval reconstruction over entitlements
	FamBalance        Family = "balance"         // cumulative sums over ledger_transfers
	FamDepletion      Family = "depletion"       // per-customer balance vs trailing-7d burn
	FamWebhookHealth  Family = "webhook_health"  // snapshot over webhook_health watermarks (#786)
	FamWebhookDaily   Family = "webhook_daily"   // flow over webhook_health_daily counter buckets (#786)
	FamAttempts       Family = "attempts"        // flow over payment_attempts (attempted_at, #1116)
	FamCheckouts      Family = "checkouts"       // flow over checkouts, one per checkout_id (first attempt)
	FamRebillCycles   Family = "rebill_cycles"   // cohort over rebill_cycles (due_at: the period that came due)
	FamNMIHistory     Family = "nmi_history"     // flow over nmi_history_months (month, #1120)
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
	Unit        string // UnitMoney | "count" | "ratio" | "days" | "seconds"
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
	// monthlyNormExpr and billingCycleExpr are the schema's shared definitions
	// (migration 0004), also used by fleet MRR.
	monthlyNormExpr  = `billing.monthly_normalized_amount(pr.amount, pr.access_duration_hours)`
	billingCycleExpr = `billing.billing_cycle_label(pr.access_duration_hours)`

	// streamExpr classifies a payment's revenue stream.
	streamExpr = `CASE
		WHEN p.subscription_id IS NOT NULL THEN 'subscription'
		ELSE 'one_time' END`

	// saleRows / mirror rows: a sale row has refunded_payment_id NULL; refund and
	// chargeback mirrors carry refunded_payment_id + reversal_kind.
	saleSettled = `p.status IN ('completed','refunded') AND p.refunded_payment_id IS NULL`
)

// Dimensions is the dimension registry.
var Dimensions = []Dimension{
	{Name: "time", Description: "bucket by the query grain (day|week|month|quarter|year, UTC calendar)"},
	{Name: "currency", Description: "currency code; added implicitly when a money measure is grouped without a single-currency filter"},
	{Name: "rail", Description: "payment rail: nmi, ccbill, stripe or solana"},
	{Name: "psp", Description: "the PSP's id (psp_…), empty when none; VAMP thresholds apply per PSP"},
	{Name: "stream", Description: "revenue stream: subscription | one_time", Values: []string{"subscription", "one_time"}},
	{Name: "product_id", Description: "product id (prod_<uuid>, the catalog's spelling)", Parse: typedDimValue(func(s string) (fmt.Stringer, error) { return billing.ParseProductID(s) })},
	{Name: "price_id", Description: "price id (price_<uuid>, the catalog's spelling)", Parse: typedDimValue(func(s string) (fmt.Stringer, error) { return billing.ParsePriceID(s) })},
	{Name: "billing_cycle", Description: "price cadence: hourly|daily|weekly|monthly|quarterly|semiannual|annual|one_time", Values: []string{"hourly", "daily", "weekly", "monthly", "quarterly", "semiannual", "annual", "one_time"}},
	{Name: "cancel_type", Description: "cancellation type recorded on the subscription (e.g. user, merchant, chargeback, failed_payment, expired)"},
	{Name: "status", Description: "subscription status; snapshot measures group/filter by the CURRENT status of subs whose interval covers t", Values: []string{"pending", "active", "past_due", "canceled", "awaiting_method", "unverified"}},
	{Name: "customer", Description: "the customer id (usage and admission measures)", Parse: typedDimValue(func(s string) (fmt.Stringer, error) { return billing.ParseCustomerID(s) })},
	{Name: "sku", Description: "usage resource slug (usage_events.resource)"},
	{Name: "rate_card", Description: "metered event type (usage_events.event_type; the key rate cards price)"},
	{Name: "card_brand", Description: "card brand on the payment (empty when not card-based)"},
	{Name: "token_type", Description: "credential form presented at charge time (#796): network_token | pan_via_proxy (custodian-held FPAN via proxy) | psp_token (the PSP's own stored credential) | unknown (legacy/non-card); attempt_failure_rate by token_type = the network-token uplift", Values: []string{"network_token", "pan_via_proxy", "psp_token", "unknown"}},
	{Name: "attempt_kind", Description: "payment attempt kind stamped at write time: initial | renewal | unknown (pre-instrumentation or imported rows)", Values: []string{"initial", "renewal", "unknown"}},
	{Name: "subscriber_type", Description: "first_time | returning (customer had an earlier ended subscription with this merchant)", Values: []string{"first_time", "returning"}},
	{Name: "entitlement", Description: "entitlement key string (e.g. premium)"},
	{Name: "discount_code", Description: "discount code on the payment (empty = none)"},
	{Name: "denial_reason", Description: "admission denial reason", Values: []string{"insufficient_balance", "insufficient_credit", "budget_exceeded", "failure_rate_limited", "delegated_spend_not_allowed"}},
	// #1116 attempt and rebill-cycle dimensions.
	{Name: "owner", Description: "who collects the subscription: engine (OpenRails) | nmi_schedule (NMI charges, OpenRails duns) | provider (Stripe Billing, CCBill) | none (no subscription)", Values: []string{"engine", "nmi_schedule", "provider", "none"}},
	{Name: "kind", Description: "attempt kind: verify ($0 card verification) | initial | upgrade | rebill (a cycle's first try) | dunning_retry | customer_retry | invoice", Values: []string{"verify", "initial", "upgrade", "rebill", "dunning_retry", "customer_retry", "invoice"}},
	{Name: "card_entry", Description: "new (card entered in this checkout or card add) | saved", Values: []string{"new", "saved"}},
	{Name: "source", Description: "who sent the attempt: openrails | provider_schedule (the PSP's own schedule) | external", Values: []string{"openrails", "provider_schedule", "external"}},
	{Name: "observed_via", Description: "how OpenRails learned the answer: response | webhook | pull", Values: []string{"response", "webhook", "pull"}},
	{Name: "category", Description: "decline category from the one classifier: approved | card_data | issuer_soft | issuer_hard | gateway_rule | system_error | unknown", Values: []string{"approved", "card_data", "issuer_soft", "issuer_hard", "gateway_rule", "system_error", "unknown"}},
	{Name: "reason", Description: "decline reason from the one classifier (empty on approvals)", Values: append([]string{""}, declineReasonValues()...)},
	{Name: "response_code", Description: "the PSP's response code verbatim (group with limit for the top codes)"},
	{Name: "issuer_code", Description: "the issuer's raw answer (NMI processor_response_code); empty until enriched"},
	{Name: "avs_result", Description: "AVS result letter or check (empty when absent)"},
	{Name: "cvv_result", Description: "CVV result letter or check (empty when absent)"},
	{Name: "card_bin", Description: "card BIN (first 6-8 digits; empty until known)"},
	{Name: "first_outcome", Description: "a rebill cycle's first outcome: approved | declined | error | missed | pending (nothing yet)", Values: []string{"approved", "declined", "error", "missed", "pending"}},
	{Name: "first_failure_category", Description: "the category of a cycle's failed first attempt (empty otherwise)"},
	{Name: "first_failure_reason", Description: "the decline reason of a cycle's failed first attempt (empty otherwise)"},
	{Name: "miss_reason", Description: "why a cycle's rebill never happened (empty when it did)", Values: []string{"", "held", "refused", "method_unusable", "not_attempted", "provider_skipped", "provider_stalled", "provider_reversed", "provider_unrecorded", "schedule_gone"}},
	{Name: "recovered_by", Description: "what collected a cycle whose first outcome failed: dunning_retry | customer_retry | updated_card | late_provider_charge (empty otherwise)", Values: []string{"", "dunning_retry", "customer_retry", "updated_card", "late_provider_charge"}},
	{Name: "recovery_attempt", Description: "the attempt that collected a failed cycle, counting the first: 1 (a missed cycle's first attempt) | 2 | 3 | 4 | 5+ (empty otherwise)", Values: []string{"", "1", "2", "3", "4", "5+"}},
	{Name: "days_to_recover", Description: "whole days from a cycle's first failure to its collection (empty otherwise)"},
	// #1120 NMI history.
	{Name: "nmi_kind", Description: "what NMI's own history tells apart: verification ($0 card verification) | one_off_sale (initial sales, upgrades and retries of declined rebills, whoever sent them) | scheduled_rebill (NMI's own schedule charge)", Values: []string{"verification", "one_off_sale", "scheduled_rebill"}},
}

// families declares each family's SQL skeleton inputs.
var families = map[Family]familySpec{
	FamPayments: {
		Kind:     "flow",
		From:     `billing.payments p`,
		TimeExpr: `p.purchased_at`,
		DimJoins: map[string]string{
			"product_id":    `LEFT JOIN billing.prices pr ON pr.merchant_id = p.merchant_id AND pr.id = p.price_id`,
			"billing_cycle": `LEFT JOIN billing.prices pr ON pr.merchant_id = p.merchant_id AND pr.id = p.price_id`,
		},
		DimExprs: map[string]string{
			"currency":      `p.currency`,
			"rail":          `p.rail`,
			"psp":           `COALESCE('psp_' || p.psp_id::text, '')`,
			"stream":        streamExpr,
			"product_id":    `COALESCE('prod_' || pr.product_id::text, '')`,
			"price_id":      `'price_' || p.price_id::text`,
			"billing_cycle": billingCycleExpr,
			"card_brand":    `COALESCE(p.card_brand, '')`,
			"token_type":    `COALESCE(p.token_type, 'unknown')`,
			"attempt_kind":  `COALESCE(p.attempt_kind, 'unknown')`,
			"discount_code": `COALESCE(p.discount_code, '')`,
		},
	},
	FamSubsNew: {
		Kind:     "flow",
		From:     `billing.subscriptions s`,
		TimeExpr: `s.started_at`,
		DimJoins: map[string]string{
			"currency":      `LEFT JOIN billing.prices pr ON pr.merchant_id = s.merchant_id AND pr.id = s.price_id`,
			"billing_cycle": `LEFT JOIN billing.prices pr ON pr.merchant_id = s.merchant_id AND pr.id = s.price_id`,
		},
		DimExprs: map[string]string{
			"currency":      `COALESCE(pr.currency, '')`,
			"rail":          `s.rail`,
			"psp":           `COALESCE('psp_' || s.psp_id::text, '')`,
			"product_id":    `'prod_' || s.product_id::text`,
			"price_id":      `COALESCE('price_' || s.price_id::text, '')`,
			"billing_cycle": billingCycleExpr,
			"subscriber_type": `CASE WHEN EXISTS (
				SELECT 1 FROM billing.subscriptions s2
				WHERE s2.merchant_id = s.merchant_id AND s2.customer_id = s.customer_id
				  AND s2.id <> s.id AND s2.ended_at IS NOT NULL AND s2.ended_at <= s.started_at
			) THEN 'returning' ELSE 'first_time' END`,
		},
	},
	FamSubsCanceled: {
		Kind:     "flow",
		From:     `billing.subscriptions s`,
		TimeExpr: `s.canceled_at`,
		DimJoins: map[string]string{
			"currency": `LEFT JOIN billing.prices pr ON pr.merchant_id = s.merchant_id AND pr.id = s.price_id`,
		},
		DimExprs: map[string]string{
			"currency":    `COALESCE(pr.currency, '')`,
			"rail":        `s.rail`,
			"psp":         `COALESCE('psp_' || s.psp_id::text, '')`,
			"product_id":  `'prod_' || s.product_id::text`,
			"price_id":    `COALESCE('price_' || s.price_id::text, '')`,
			"cancel_type": `COALESCE(s.cancel_type, 'unknown')`,
		},
		BaseWhere: `s.canceled_at IS NOT NULL`,
	},
	FamSubsEnded: {
		Kind:     "flow",
		From:     `billing.subscriptions s`,
		TimeExpr: `s.ended_at`,
		DimExprs: map[string]string{
			"rail":        `s.rail`,
			"product_id":  `'prod_' || s.product_id::text`,
			"price_id":    `COALESCE('price_' || s.price_id::text, '')`,
			"cancel_type": `COALESCE(s.cancel_type, 'unknown')`,
		},
		BaseWhere: `s.ended_at IS NOT NULL`,
	},
	FamGrants: {
		Kind:     "flow",
		From:     `billing.grants g`,
		TimeExpr: `g.created_at`,
		DimExprs: map[string]string{
			"currency":   `g.currency`,
			"product_id": `COALESCE('prod_' || g.product_id::text, '')`,
			"customer":   `g.customer_id::text`,
		},
		BaseWhere: `g.kind = 'credit' AND g.event = 'grant' AND g.source_type = 'purchase'`,
	},
	FamUsage: {
		Kind:     "flow",
		From:     `billing.usage_events ue`,
		TimeExpr: `ue.occurred_at`,
		DimExprs: map[string]string{
			"currency":  `ue.currency`,
			"customer":  `ue.customer_id::text`,
			"sku":       `COALESCE(ue.resource, '')`,
			"rate_card": `ue.event_type`,
		},
	},
	FamTransitions: {
		Kind:     "flow",
		From:     `billing.subscription_status_transitions st`,
		TimeExpr: `st.occurred_at`,
		DimExprs: map[string]string{
			"cancel_type": `COALESCE(st.cancel_type, 'unknown')`,
		},
	},
	FamDenials: {
		Kind:     "flow",
		From:     `billing.admission_denials_hourly ad`,
		TimeExpr: `ad.hour_at`,
		DimExprs: map[string]string{
			"denial_reason": `ad.denial_reason`,
			"customer":      `ad.customer_id::text`,
		},
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
			"price_id":      `COALESCE('price_' || s.price_id::text, '')`,
			"billing_cycle": billingCycleExpr,
			"status":        `s.status::text`,
		},
	},
	FamEntitlSnapshot: {
		Kind: "snapshot",
		From: `billing.entitlements e`,
		BaseWhere: `e.starts_at <= edge.bucket AND (e.ends_at IS NULL OR e.ends_at > edge.bucket)
		  AND (e.revoked_at IS NULL OR e.revoked_at > edge.bucket) AND e.deleted_at IS NULL`,
		DimExprs: map[string]string{
			"entitlement": `e.entitlement`,
		},
	},
	FamBalance: {
		Kind: "balance",
		From: `billing.ledger_transfers lt
		JOIN billing.ledger_accounts da ON da.merchant_id = lt.merchant_id AND da.id = lt.debit_account_id
		JOIN billing.ledger_accounts ca ON ca.merchant_id = lt.merchant_id AND ca.id = lt.credit_account_id`,
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
			"currency":      `COALESCE(a.currency, '')`,
			"rail":          `a.rail`,
			"psp":           `COALESCE('psp_' || a.psp_id::text, '')`,
			"owner":         `a.owner`,
			"kind":          `a.kind`,
			"card_entry":    `a.card_entry`,
			"source":        `a.source`,
			"observed_via":  `a.observed_via`,
			"category":      `a.category`,
			"reason":        `COALESCE(a.reason, '')`,
			"response_code": `COALESCE(a.response_code, '')`,
			"issuer_code":   `COALESCE(a.issuer_code, '')`,
			"avs_result":    `COALESCE(a.avs_result, '')`,
			"cvv_result":    `COALESCE(a.cvv_result, '')`,
			"card_brand":    `COALESCE(a.card_brand, '')`,
			"card_bin":      `COALESCE(a.card_bin, '')`,
			"token_type":    `COALESCE(a.token_type, 'unknown')`,
		},
	},
	FamCheckouts: {
		Kind:     "flow",
		From:     checkoutsFrom,
		TimeExpr: `ck.started_at`,
		DimExprs: map[string]string{
			"currency":   `ck.currency`,
			"rail":       `ck.rail`,
			"psp":        `COALESCE('psp_' || ck.psp_id::text, '')`,
			"owner":      `ck.owner`,
			"card_entry": `CASE WHEN ck.new_card THEN 'new' ELSE 'saved' END`,
		},
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
			"first_outcome":          `cy.first_outcome`,
			"first_failure_category": `CASE WHEN cy.first_category <> 'approved' THEN cy.first_category ELSE '' END`,
			"first_failure_reason":   `COALESCE(cy.first_reason, '')`,
			"miss_reason":            `COALESCE(cy.miss_reason, '')`,
			"recovered_by":           `cy.recovered_by`,
			"recovery_attempt": `CASE WHEN NOT cy.first_failed OR cy.won_ordinal IS NULL THEN ''
				WHEN cy.won_ordinal >= 5 THEN '5+' ELSE cy.won_ordinal::text END`,
			"days_to_recover": `CASE WHEN NOT cy.first_failed OR cy.won_at IS NULL THEN ''
				ELSE floor(EXTRACT(EPOCH FROM cy.won_at - COALESCE(cy.missed_at, cy.first_at)) / 86400)::bigint::text END`,
		},
	},
	FamNMIHistory: {
		Kind:     "flow",
		From:     `billing.nmi_history_months h`,
		TimeExpr: `h.month`,
		DimExprs: map[string]string{
			"psp":      `COALESCE('psp_' || h.psp_id::text, '')`,
			"nmi_kind": `h.kind`,
			"category": `h.category`,
			"reason":   `h.reason`,
		},
	},
}

// Each rebill cycle with its first attempt, the attempt that collected it and
// when it closes: collected, the subscription canceled, or 15 days past due
// (the dunning window is at most 14). ListRebillCycles derives the same facts.
const rebillCyclesFrom = `(SELECT c.merchant_id, c.id, c.psp_id, c.rail, c.owner, c.due_at, c.currency, c.missed_at, c.miss_reason,
		f.category AS first_category, f.reason AS first_reason, f.attempted_at AS first_at,
		w.attempted_at AS won_at, w.ordinal AS won_ordinal,
		(c.missed_at IS NOT NULL OR COALESCE(f.category <> 'approved', false)) AS first_failed,
		CASE WHEN c.missed_at IS NOT NULL THEN 'missed'
			WHEN f.category IS NULL THEN 'pending'
			WHEN f.category = 'approved' THEN 'approved'
			WHEN f.category = 'system_error' THEN 'error'
			ELSE 'declined' END AS first_outcome,
		LEAST(w.attempted_at, CASE WHEN s.canceled_at IS NOT NULL THEN GREATEST(s.canceled_at, c.due_at) END, c.due_at + interval '15 days') AS closed_at,
		CASE WHEN w.id IS NULL OR NOT (c.missed_at IS NOT NULL OR COALESCE(f.category <> 'approved', false)) THEN ''
			WHEN w.source = 'provider_schedule' THEN 'late_provider_charge'
			WHEN EXISTS (SELECT 1 FROM billing.payment_method_updates u
				WHERE u.merchant_id = c.merchant_id AND u.payment_method_id = w.payment_method_id AND u.kind = 'updated'
				AND u.at >= COALESCE(c.missed_at, f.attempted_at) AND u.at <= w.attempted_at) THEN 'updated_card'
			WHEN w.kind = 'customer_retry' THEN 'customer_retry'
			ELSE 'dunning_retry' END AS recovered_by
	FROM billing.rebill_cycles c
	LEFT JOIN LATERAL (SELECT a.category, a.reason, a.attempted_at FROM billing.payment_attempts a
		WHERE a.merchant_id = c.merchant_id AND a.cycle_id = c.id ORDER BY a.attempted_at, a.id LIMIT 1) f ON true
	LEFT JOIN LATERAL (SELECT a.id, a.kind, a.source, a.attempted_at, a.payment_method_id,
			(SELECT count(*) FROM billing.payment_attempts b
				WHERE b.merchant_id = c.merchant_id AND b.cycle_id = c.id AND (b.attempted_at, b.id) <= (a.attempted_at, a.id)) AS ordinal
		FROM billing.payment_attempts a
		WHERE a.merchant_id = c.merchant_id AND a.cycle_id = c.id AND a.category = 'approved'
		ORDER BY a.attempted_at, a.id LIMIT 1) w ON true
	LEFT JOIN billing.subscriptions s ON s.merchant_id = c.merchant_id AND s.id = c.subscription_id AND s.deleted_at IS NULL) cy`

// #1116: a checkout is one buyer's attempts on one target (checkout_id). It
// is approved when its target is: a verification for a card save, an initial
// or upgrade charge for a purchase.
const checkoutsFrom = `(SELECT a.merchant_id, a.checkout_id,
		MIN(a.attempted_at) AS started_at, MAX(a.attempted_at) AS last_at, COUNT(*) AS attempts,
		bool_or(a.category = 'approved' AND CASE WHEN a.checkout_target = 'card_save' THEN a.kind = 'verify' ELSE a.kind IN ('initial', 'upgrade') END) AS approved,
		bool_or(a.category <> 'approved') AS had_failure,
		bool_or(a.card_entry = 'new') AS new_card,
		(array_agg(a.rail ORDER BY a.attempted_at, a.id))[1] AS rail,
		(array_agg(a.psp_id ORDER BY a.attempted_at, a.id))[1] AS psp_id,
		(array_agg(a.owner ORDER BY a.attempted_at, a.id))[1] AS owner,
		COALESCE((array_agg(a.currency ORDER BY a.attempted_at, a.id) FILTER (WHERE a.currency IS NOT NULL))[1], '') AS currency
	FROM billing.payment_attempts a
	WHERE a.checkout_id IS NOT NULL
	GROUP BY a.merchant_id, a.checkout_id) ck`

// checkoutSettled: approved, or quiet for an hour before the query's end.
const checkoutSettled = `(ck.approved OR ck.last_at < @to - interval '1 hour')`

// #1116: rebill cycles as migration 0033's view derives them. Closed states
// are as of the query's range end.
const (
	cycleClosed = `(cy.closed_at <= @to)`
	cycleLost   = `(cy.won_at IS NULL AND cy.closed_at <= @to)`
)

// #1116 dimension sets.
var (
	attemptDims  = []string{"currency", "rail", "psp", "owner", "kind", "card_entry", "source", "observed_via", "category", "reason", "response_code", "issuer_code", "avs_result", "cvv_result", "card_brand", "card_bin", "token_type"}
	checkoutDims = []string{"currency", "rail", "psp", "owner", "card_entry"}
	cycleDims    = []string{"currency", "rail", "psp", "owner", "first_outcome", "first_failure_category", "first_failure_reason", "miss_reason", "recovered_by", "recovery_attempt", "days_to_recover"}
	nmiHistDims  = []string{"psp", "nmi_kind", "category", "reason"}
)

// UnitMoney marks a measure whose cells are MoneyCell values: exact native
// units of the row's currency (the registry scale, not always millionths).
const UnitMoney = "money"

// depletionRiskDays: a customer is at depletion risk when their prepaid balance
// covers <= this many days of their trailing-7d average burn.
const depletionRiskDays = 7

// Measures is the measure registry (CORE tier + internal ratio components).
var Measures = []Measure{
	// --- payments: additive money -------------------------------------------
	{Name: "gross_revenue", Class: ClassAdditive, Family: FamPayments, Money: true, Unit: "money",
		Description: "sum of settled sale amounts (native currency units), before refunds/chargebacks; gross of processor fees",
		Formula:     "SUM(amount) over settled sale payments (incl. later-refunded)",
		Expr:        `COALESCE(SUM(p.amount) FILTER (WHERE ` + saleSettled + ` AND p.amount > 0), 0)::bigint`,
		Dims:        []string{"currency", "rail", "psp", "stream", "product_id", "price_id", "billing_cycle", "card_brand", "attempt_kind", "discount_code"}},
	{Name: "net_revenue", Class: ClassAdditive, Family: FamPayments, Money: true, Unit: "money",
		Description: "gross_revenue minus refunds and chargebacks (net of returns, NOT net of fees); Stripe's 'net volume'",
		Formula:     "gross_revenue - refunds - chargebacks",
		Expr:        `COALESCE(SUM(p.amount) FILTER (WHERE p.status IN ('completed','refunded')), 0)::bigint`,
		Dims:        []string{"currency", "rail", "psp", "stream", "product_id", "price_id", "billing_cycle", "card_brand", "attempt_kind", "discount_code"}},
	{Name: "refunds", Class: ClassAdditive, Family: FamPayments, Money: true, Unit: "money",
		Description: "sum of refunded amounts (native currency units, positive), from refund mirror rows",
		Formula:     "SUM(amount) of refunds",
		Expr:        `COALESCE(SUM(-p.amount) FILTER (WHERE p.status = 'completed' AND p.reversal_kind = 'refund'), 0)::bigint`,
		Dims:        []string{"currency", "rail", "psp", "stream", "product_id", "price_id", "card_brand"}},
	{Name: "chargebacks", Class: ClassAdditive, Family: FamPayments, Money: true, Unit: "money",
		Description: "sum of chargeback amounts (native currency units, positive), net of won disputes",
		Formula:     "SUM(amount) of chargebacks and dispute reversals",
		Expr:        `COALESCE(SUM(-p.amount) FILTER (WHERE p.status = 'completed' AND p.reversal_kind IN ('chargeback','dispute_reversal')), 0)::bigint`,
		Dims:        []string{"currency", "rail", "psp", "stream", "product_id", "price_id", "card_brand"}},
	// --- payments: additive counts -------------------------------------------
	{Name: "payment_count", Class: ClassAdditive, Family: FamPayments, Unit: "count",
		Description: "count of settled sale payments (approved attempts)",
		Formula:     "COUNT(settled sale payments)",
		Expr:        `COUNT(*) FILTER (WHERE ` + saleSettled + `)`,
		Dims:        []string{"currency", "rail", "psp", "stream", "product_id", "price_id", "billing_cycle", "card_brand", "token_type", "attempt_kind", "discount_code"}},
	{Name: "refund_count", Class: ClassAdditive, Family: FamPayments, Unit: "count",
		Description: "count of refunds issued",
		Formula:     "COUNT(refunds)",
		Expr:        `COUNT(*) FILTER (WHERE p.status = 'completed' AND p.reversal_kind = 'refund')`,
		Dims:        []string{"currency", "rail", "psp", "stream", "product_id", "price_id", "card_brand"}},
	{Name: "chargeback_count", Class: ClassAdditive, Family: FamPayments, Unit: "count",
		Description: "count of chargebacks/disputes received",
		Formula:     "COUNT(chargebacks)",
		Expr:        `COUNT(*) FILTER (WHERE p.status = 'completed' AND p.reversal_kind = 'chargeback')`,
		Dims:        []string{"currency", "rail", "psp", "stream", "product_id", "price_id", "card_brand"}},
	// --- payments: distinct ---------------------------------------------------
	{Name: "unique_rebilled_customers", Class: ClassDistinct, Family: FamPayments, Unit: "count",
		Description: "distinct customers successfully rebilled (settled renewal payments) in the bucket",
		Formula:     "COUNT(DISTINCT customers) over settled renewal payments",
		Expr:        `COUNT(DISTINCT p.customer_id) FILTER (WHERE ` + saleSettled + ` AND p.attempt_kind = 'renewal')`,
		Dims:        []string{"currency", "rail", "psp"}},
	{Name: "paying_customers", Class: ClassDistinct, Family: FamPayments, Unit: "count", Internal: true,
		Expr: `COUNT(DISTINCT p.customer_id) FILTER (WHERE ` + saleSettled + `)`,
		Dims: []string{"currency", "rail", "psp", "stream", "product_id", "price_id", "billing_cycle", "card_brand", "attempt_kind", "discount_code"}},
	// --- payments: ratios ------------------------------------------------------
	{Name: "chargeback_rate", Class: ClassRatio, Unit: "ratio", Num: "chargeback_count", Den: "payment_count",
		Description: "chargebacks / settled payments; group by psp for the per-PSP VAMP number",
		Formula:     "chargeback_count / payment_count",
		Dims:        []string{"currency", "rail", "psp", "stream", "product_id", "price_id", "card_brand"}},
	{Name: "realized_revenue_per_customer", Class: ClassRatio, Unit: "money", Money: true, Num: "net_revenue", Den: "paying_customers",
		Description: "net revenue per distinct paying customer in the window (LTV-to-date when windowed over lifetime)",
		Formula:     "net_revenue / COUNT(DISTINCT paying customers)",
		Dims:        []string{"currency", "rail", "psp", "stream", "product_id", "price_id", "billing_cycle", "attempt_kind", "discount_code"}},
	// --- subscriptions: flow ----------------------------------------------------
	{Name: "new_subscriptions", Class: ClassAdditive, Family: FamSubsNew, Unit: "count",
		Description: "subscriptions started in the bucket (excludes never-activated pending)",
		Formula:     "COUNT(subscriptions started in the bucket, not pending)",
		// or#893: `failed` left the lifecycle enum, and naming a label the type
		// no longer has is a runtime cast error, not a no-op predicate. A
		// subscription whose first charge failed is past_due or canceled; only
		// `pending` still means never-activated.
		Expr: `COUNT(*) FILTER (WHERE s.status <> 'pending')`,
		Dims: []string{"currency", "rail", "psp", "product_id", "price_id", "billing_cycle", "subscriber_type"}},
	{Name: "cancellations", Class: ClassAdditive, Family: FamSubsCanceled, Unit: "count",
		Description: "subscriptions canceled in the bucket, by cancel_type",
		Formula:     "COUNT(subscriptions canceled in the bucket)",
		Expr:        `COUNT(*)`,
		Dims:        []string{"currency", "rail", "psp", "product_id", "price_id", "cancel_type"}},
	{Name: "ended_membership_days", Class: ClassAdditive, Family: FamSubsEnded, Unit: "days", Internal: true,
		Expr: `COALESCE(SUM(EXTRACT(EPOCH FROM (s.ended_at - s.started_at)) / 86400.0), 0)::float8`,
		Dims: []string{"rail", "product_id", "price_id", "cancel_type"}},
	{Name: "ended_subscriptions", Class: ClassAdditive, Family: FamSubsEnded, Unit: "count", Internal: true,
		Expr: `COUNT(*)`,
		Dims: []string{"rail", "product_id", "price_id", "cancel_type"}},
	{Name: "avg_membership_duration_days", Class: ClassRatio, Unit: "days", Num: "ended_membership_days", Den: "ended_subscriptions",
		Description: "average started->ended lifetime (days) of subs that ENDED in the bucket; survivors not counted (censoring)",
		Formula:     "SUM(lifetime) / COUNT(subscriptions ended in the bucket)",
		Dims:        []string{"rail", "product_id", "price_id", "cancel_type"}},
	{Name: "subscriptions_at_period_start", Class: ClassSnapshot, Family: FamSubsSnapshot, Unit: "count", Internal: true,
		SnapshotAtPeriodStart: true,
		Expr:                  `COUNT(s.id)`,
		Dims:                  []string{"currency", "rail", "psp", "product_id", "price_id", "billing_cycle", "status"}},
	{Name: "churn_rate", Class: ClassRatio, Unit: "ratio", Num: "cancellations", Den: "subscriptions_at_period_start",
		Description: "cancellations in the bucket / subscriptions existing at bucket start; currency groups by price currency",
		Formula:     "cancellations / subscriptions(at period start)",
		Dims:        []string{"currency", "rail", "product_id", "price_id"}},
	// --- attempts (#1116): every authorization a PSP answered --------------------
	{Name: "attempts", Class: ClassAdditive, Family: FamAttempts, Unit: "count",
		Description: "authorizations a PSP answered: card verifications, sales, rebills and retries",
		Formula:     "COUNT(payment attempts)",
		Expr:        `COUNT(*)`,
		Dims:        attemptDims},
	{Name: "approved_attempts", Class: ClassAdditive, Family: FamAttempts, Unit: "count",
		Description: "approved authorizations",
		Formula:     "COUNT(attempts with category='approved')",
		Expr:        `COUNT(*) FILTER (WHERE a.category = 'approved')`,
		Dims:        attemptDims},
	{Name: "failed_attempts", Class: ClassAdditive, Family: FamAttempts, Unit: "count",
		Description: "refused or failed authorizations, by the one decline classifier's category and reason",
		Formula:     "COUNT(attempts with category<>'approved')",
		Expr:        `COUNT(*) FILTER (WHERE a.category <> 'approved')`,
		Dims:        attemptDims},
	{Name: "attempt_failure_rate", Class: ClassRatio, Unit: "ratio", Num: "failed_attempts", Den: "attempts",
		Description: "failed / all authorizations; the new-card decline rate is kind in (verify, initial) with card_entry=new",
		Formula:     "failed_attempts / attempts",
		Dims:        attemptDims},
	// --- checkouts (#1116): one buyer's attempts on one target -------------------
	{Name: "checkouts", Class: ClassAdditive, Family: FamCheckouts, Unit: "count",
		Description: "checkouts (a buyer's attempts on one purchase or card save) started in the bucket",
		Formula:     "COUNT(checkouts)",
		Expr:        `COUNT(*)`,
		Dims:        checkoutDims},
	{Name: "settled_checkouts", Class: ClassAdditive, Family: FamCheckouts, Unit: "count", Internal: true,
		Expr: `COUNT(*) FILTER (WHERE ` + checkoutSettled + `)`,
		Dims: checkoutDims},
	{Name: "failed_checkouts", Class: ClassAdditive, Family: FamCheckouts, Unit: "count",
		Description: "checkouts that never reached an approval of their target and went quiet for an hour",
		Formula:     "COUNT(checkouts without approval, idle > 1h)",
		Expr:        `COUNT(*) FILTER (WHERE NOT ck.approved AND ` + checkoutSettled + `)`,
		Dims:        checkoutDims},
	{Name: "checkout_failure_rate", Class: ClassRatio, Unit: "ratio", Num: "failed_checkouts", Den: "settled_checkouts",
		Description: "failed / settled checkouts: the new-card failure rate per buyer rather than per attempt",
		Formula:     "failed_checkouts / (approved + failed checkouts)",
		Dims:        checkoutDims},
	{Name: "checkout_attempts", Class: ClassAdditive, Family: FamCheckouts, Unit: "count", Internal: true,
		Expr: `COALESCE(SUM(ck.attempts), 0)::bigint`,
		Dims: checkoutDims},
	{Name: "attempts_per_checkout", Class: ClassRatio, Unit: "ratio", Num: "checkout_attempts", Den: "checkouts",
		Description: "average authorizations per checkout",
		Formula:     "SUM(attempts per checkout) / checkouts",
		Dims:        checkoutDims},
	{Name: "troubled_checkouts", Class: ClassAdditive, Family: FamCheckouts, Unit: "count", Internal: true,
		Expr: `COUNT(*) FILTER (WHERE ck.had_failure AND ` + checkoutSettled + `)`,
		Dims: checkoutDims},
	{Name: "recovered_checkouts", Class: ClassAdditive, Family: FamCheckouts, Unit: "count", Internal: true,
		Expr: `COUNT(*) FILTER (WHERE ck.had_failure AND ck.approved)`,
		Dims: checkoutDims},
	{Name: "checkout_recovery_rate", Class: ClassRatio, Unit: "ratio", Num: "recovered_checkouts", Den: "troubled_checkouts",
		Description: "checkouts with a failure that still ended approved / settled checkouts with a failure",
		Formula:     "recovered checkouts / settled checkouts with a failure",
		Dims:        checkoutDims},
	// --- rebill cycles (#1116): cohort by the period that came due ---------------
	{Name: "rebills_due", Class: ClassAdditive, Family: FamRebillCycles, Unit: "count",
		Description: "rebill cycles: paid periods that came due in the bucket and were attempted or recorded missed",
		Formula:     "COUNT(rebill cycles)",
		Expr:        `COUNT(*)`,
		Dims:        cycleDims},
	{Name: "rebills_open", Class: ClassAdditive, Family: FamRebillCycles, Unit: "count",
		Description: "cycles neither collected nor closed (subscription ended, a later period paid, or the dunning window passed)",
		Formula:     "COUNT(open cycles)",
		Expr:        `COUNT(*) FILTER (WHERE NOT ` + cycleClosed + `)`,
		Dims:        cycleDims},
	{Name: "rebills_attempted", Class: ClassAdditive, Family: FamRebillCycles, Unit: "count",
		Description: "cycles with a first outcome: attempted, or recorded missed (the rebill failure rate's denominator)",
		Formula:     "COUNT(cycles attempted or missed)",
		Expr:        `COUNT(*) FILTER (WHERE cy.missed_at IS NOT NULL OR cy.first_category IS NOT NULL)`,
		Dims:        cycleDims},
	{Name: "rebill_first_failures", Class: ClassAdditive, Family: FamRebillCycles, Unit: "count",
		Description: "cycles whose rebill failed: the first attempt declined or errored, or no attempt happened (missed)",
		Formula:     "COUNT(cycles whose first_outcome is declined, error or missed)",
		Expr:        `COUNT(*) FILTER (WHERE cy.first_failed)`,
		Dims:        cycleDims},
	{Name: "rebill_first_failure_rate", Class: ClassRatio, Unit: "ratio", Num: "rebill_first_failures", Den: "rebills_attempted",
		Description: "failed first rebills / cycles with a first outcome",
		Formula:     "rebill_first_failures / (cycles attempted or missed)",
		Dims:        cycleDims},
	{Name: "rebills_missed", Class: ClassAdditive, Family: FamRebillCycles, Unit: "count",
		Description: "cycles whose rebill never happened by its owner's deadline, by miss_reason",
		Formula:     "COUNT(missed cycles)",
		Expr:        `COUNT(*) FILTER (WHERE cy.missed_at IS NOT NULL)`,
		Dims:        cycleDims},
	{Name: "rebill_missed_rate", Class: ClassRatio, Unit: "ratio", Num: "rebills_missed", Den: "rebills_due",
		Description: "missed / due rebills",
		Formula:     "rebills_missed / rebills_due",
		Dims:        cycleDims},
	{Name: "rebills_closed", Class: ClassAdditive, Family: FamRebillCycles, Unit: "count", Internal: true,
		Expr: `COUNT(*) FILTER (WHERE ` + cycleClosed + `)`,
		Dims: cycleDims},
	{Name: "rebills_collected", Class: ClassAdditive, Family: FamRebillCycles, Unit: "count", Internal: true,
		Expr: `COUNT(*) FILTER (WHERE cy.won_at IS NOT NULL)`,
		Dims: cycleDims},
	{Name: "rebills_lost", Class: ClassAdditive, Family: FamRebillCycles, Unit: "count", Internal: true,
		Expr: `COUNT(*) FILTER (WHERE ` + cycleLost + `)`,
		Dims: cycleDims},
	{Name: "closed_first_failures", Class: ClassAdditive, Family: FamRebillCycles, Unit: "count", Internal: true,
		Expr: `COUNT(*) FILTER (WHERE cy.first_failed AND ` + cycleClosed + `)`,
		Dims: cycleDims},
	{Name: "dunning_recovered", Class: ClassAdditive, Family: FamRebillCycles, Unit: "count",
		Description: "cycles whose first rebill failed and that were collected; group by recovery_attempt or days_to_recover for the recovery curve",
		Formula:     "COUNT(cycles with a failed first outcome that were collected)",
		Expr:        `COUNT(*) FILTER (WHERE cy.first_failed AND cy.won_at IS NOT NULL)`,
		Dims:        cycleDims},
	{Name: "dunning_recovery_rate", Class: ClassRatio, Unit: "ratio", Num: "dunning_recovered", Den: "closed_first_failures",
		Description: "collected / closed cycles whose first rebill failed",
		Formula:     "dunning_recovered / closed cycles with a failed first outcome",
		Dims:        cycleDims},
	{Name: "rebill_collection_rate", Class: ClassRatio, Unit: "ratio", Num: "rebills_collected", Den: "rebills_closed",
		Description: "collected / closed cycles",
		Formula:     "collected cycles / closed cycles",
		Dims:        cycleDims},
	{Name: "rebill_loss_rate", Class: ClassRatio, Unit: "ratio", Num: "rebills_lost", Den: "rebills_closed",
		Description: "closed cycles never collected / closed cycles",
		Formula:     "lost cycles / closed cycles",
		Dims:        cycleDims},
	// --- NMI history (#1120): NMI's own authorizations, by month ------------------
	{Name: "nmi_history_authorizations", Class: ClassAdditive, Family: FamNMIHistory, Unit: "count",
		Description: "authorizations NMI answered, read daily from its transaction history: card verifications, one-off sales and NMI-scheduled rebills, whoever sent them; dated by month",
		Formula:     "SUM(authorizations in NMI's history)",
		Expr:        `COALESCE(SUM(h.authorizations), 0)::bigint`,
		Dims:        nmiHistDims},
	{Name: "nmi_history_approved", Class: ClassAdditive, Family: FamNMIHistory, Unit: "count",
		Description: "authorizations NMI approved",
		Formula:     "SUM(authorizations with category='approved')",
		Expr:        `COALESCE(SUM(h.authorizations) FILTER (WHERE h.category = 'approved'), 0)::bigint`,
		Dims:        nmiHistDims},
	{Name: "nmi_history_refused", Class: ClassAdditive, Family: FamNMIHistory, Unit: "count",
		Description: "authorizations NMI refused, by the one decline classifier's category and reason",
		Formula:     "SUM(authorizations with category<>'approved')",
		Expr:        `COALESCE(SUM(h.authorizations) FILTER (WHERE h.category <> 'approved'), 0)::bigint`,
		Dims:        nmiHistDims},
	{Name: "nmi_history_refusal_rate", Class: ClassRatio, Unit: "ratio", Num: "nmi_history_refused", Den: "nmi_history_authorizations",
		Description: "refused / all authorizations in NMI's history; group by nmi_kind and time (month) for the decline report",
		Formula:     "nmi_history_refused / nmi_history_authorizations",
		Dims:        nmiHistDims},
	// --- grants / usage-credits ---------------------------------------------------
	{Name: "credits_sold", Class: ClassAdditive, Family: FamGrants, Money: true, Unit: "money",
		Description: "prepaid credit lots purchased (cash-in, native currency units); NOT recognized revenue until consumed",
		Formula:     "SUM(grant lot amounts) over purchased credit grants",
		Expr:        `COALESCE(SUM(g.amount), 0)::bigint`,
		Dims:        []string{"currency", "product_id", "customer"}},
	{Name: "credit_topups", Class: ClassAdditive, Family: FamGrants, Unit: "count", Internal: true,
		Expr: `COUNT(*)`,
		Dims: []string{"currency", "product_id", "customer"}},
	{Name: "repeat_topups", Class: ClassAdditive, Family: FamGrants, Unit: "count", Internal: true,
		Expr: `COALESCE(SUM(CASE WHEN EXISTS (
			SELECT 1 FROM billing.grants g2
			WHERE g2.merchant_id = g.merchant_id AND g2.customer_id = g.customer_id
			  AND g2.kind = 'credit' AND g2.event = 'grant' AND g2.source_type = 'purchase'
			  AND g2.created_at < g.created_at
		) THEN 1 ELSE 0 END), 0)::bigint`,
		Dims: []string{"currency", "product_id", "customer"}},
	{Name: "repeat_topup_rate", Class: ClassRatio, Unit: "ratio", Num: "repeat_topups", Den: "credit_topups",
		Description: "share of credit purchases in the bucket made by customers with an earlier credit purchase",
		Formula:     "repeat top-ups / all top-ups",
		Dims:        []string{"currency", "product_id"}},
	{Name: "usage_revenue", Class: ClassAdditive, Family: FamUsage, Money: true, Unit: "money",
		Description: "consumed (recognized) usage spend in native currency units, from usage events; cash-in is credits_sold",
		Formula:     "SUM(amount) of usage events",
		Expr:        `COALESCE(SUM(ue.amount), 0)::bigint`,
		Dims:        []string{"currency", "customer", "sku", "rate_card"}},
	{Name: "usage_units", Class: ClassAdditive, Family: FamUsage, Unit: "count",
		Description: "count of metered usage events (raw volume; per-dimension token counts live host-side)",
		Formula:     "COUNT(usage events)",
		Expr:        `COUNT(*)`,
		Dims:        []string{"currency", "customer", "sku", "rate_card"}},
	{Name: "active_customers", Class: ClassDistinct, Family: FamUsage, Unit: "count",
		Description: "distinct customers with any usage in the bucket (the API platform's WAU/MAU)",
		Formula:     "COUNT(DISTINCT customers) over usage events",
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
		Dims:        []string{"denial_reason", "customer"}},
	// --- snapshots ------------------------------------------------------------------
	{Name: "subscriptions", Class: ClassSnapshot, Family: FamSubsSnapshot, Unit: "count",
		Description: "subscriptions existing at t (interval reconstruction); filter/group by status for active/past_due/pending views",
		Formula:     "COUNT(subscriptions started at or before t and not ended)",
		Expr:        `COUNT(s.id)`,
		Dims:        []string{"currency", "rail", "psp", "product_id", "price_id", "billing_cycle", "status"}},
	{Name: "mrr", Class: ClassSnapshot, Family: FamSubsSnapshot, Money: true, Unit: "money",
		Description: "monthly-normalized recurring price of auto-renew subs existing at t; group by status to split healthy vs in-dunning",
		Formula:     "SUM(monthly-normalized price) over auto-renewing subscriptions at t",
		Expr:        `COALESCE(SUM(` + monthlyNormExpr + `) FILTER (WHERE pr.auto_renew), 0)::bigint`,
		Dims:        []string{"currency", "rail", "psp", "product_id", "price_id", "billing_cycle", "status"}},
	{Name: "billable_subscriptions", Class: ClassSnapshot, Family: FamSubsSnapshot, Unit: "count",
		Description: "subs at t projected to keep billing: auto-renew price, non-terminal status, not canceled/scheduled for deletion",
		Formula:     "COUNT(auto-renewing subscriptions at t that are pending, active, past_due or unknown, not canceled and not scheduled for deletion)",
		Expr:        `COUNT(s.id) FILTER (WHERE pr.auto_renew AND s.status IN ('pending','active','past_due','unknown') AND s.canceled_at IS NULL AND s.deletion_scheduled_at IS NULL)`,
		Dims:        []string{"currency", "rail", "psp", "product_id", "price_id", "billing_cycle"}},
	{Name: "entitled_customers", Class: ClassSnapshot, Family: FamEntitlSnapshot, Unit: "count",
		Description: "distinct customers holding a live entitlement at t (includes timed/comped access, not just subscribers)",
		Formula:     "COUNT(DISTINCT customers) with an entitlement at t",
		Expr:        `COUNT(DISTINCT e.customer_id)`,
		Dims:        []string{"entitlement"}},
	{Name: "outstanding_credit_liability", Class: ClassSnapshot, Family: FamBalance, Money: true, Unit: "money",
		Description: "unconsumed prepaid credit (deferred revenue) at t: net customer_balance across the ledger",
		Formula:     "SUM(customer balances) at t",
		Expr: `COALESCE(SUM(
			CASE WHEN ca.account_type = 'customer_balance' THEN lt.amount ELSE 0 END
			- CASE WHEN da.account_type = 'customer_balance' THEN lt.amount ELSE 0 END), 0)::bigint`,
		Dims: []string{"currency"}},
	{Name: "outstanding_owed", Class: ClassSnapshot, Family: FamBalance, Money: true, Unit: "money",
		Description: "arrears accounts receivable at t: accrued-but-unpaid usage debt",
		Formula:     "SUM(arrears owed) at t",
		Expr: `COALESCE(SUM(
			CASE WHEN da.account_type = 'arrears_liability' THEN lt.amount ELSE 0 END
			- CASE WHEN ca.account_type = 'arrears_liability' THEN lt.amount ELSE 0 END), 0)::bigint`,
		Dims: []string{"currency"}},
	{Name: "customers_at_depletion_risk", Class: ClassSnapshot, Family: FamDepletion, Unit: "count",
		Description: "customers whose prepaid balance at t covers <= 7 days of their trailing-7d burn (in any currency)",
		Formula:     "COUNT(customers with balance / (7d burn / 7) <= 7 days)",
		Dims:        []string{}},
	// --- webhook health (#786) --------------------------------------------------------
	{Name: "webhook_silence_age_seconds", Class: ClassSnapshot, Family: FamWebhookHealth, Unit: "seconds",
		Description: "seconds since the last signature-VERIFIED inbound webhook per PSP at t (since tracking began when none was ever accepted); rejects never advance it",
		Formula:     "t - the last accepted webhook (or the PSP's creation), per PSP",
		Expr:        `MAX(GREATEST(EXTRACT(EPOCH FROM (edge.bucket - COALESCE(wh.last_accepted_at, wh.created_at))), 0))::float8`,
		Dims:        []string{"rail", "psp"}},
	{Name: "webhook_rejects", Class: ClassAdditive, Family: FamWebhookDaily, Unit: "count",
		Description: "inbound webhooks that FAILED verification (bad signature / disallowed source), UTC-day buckets — a spike means webhooks arrive but the signing secret is wrong",
		Formula:     "SUM(daily rejected counters)",
		Expr:        `COALESCE(SUM(whd.rejected), 0)::bigint`,
		Dims:        []string{"rail", "psp"}},
	{Name: "webhook_drift_events", Class: ClassAdditive, Family: FamWebhookDaily, Unit: "count",
		Description: "provider-state corrections that arrived by PULL with no webhook accepted since the previous pull (partial webhook breakage), UTC-day buckets",
		Formula:     "SUM(daily drift counters)",
		Expr:        `COALESCE(SUM(whd.drift), 0)::bigint`,
		Dims:        []string{"rail", "psp"}},
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
	"token_type exists from #796 instrumentation onward: NULL/legacy rows read 'unknown' and are excluded from token_type-filtered analyses; attempt_kind exists from instrumentation onward; older and imported rows read 'unknown'. Declines are measured on attempts, not payments: Stripe- and CCBill-owned renewals are recorded as their providers report them (#1111b)",
	"attempts, checkouts and rebill cycles begin at their instrumentation (#1110, #1111): earlier history lives in payments and the provider. Checkouts and cycles are dated by their first attempt and due date; a checkout quiet for an hour, and a cycle past its 15-day dunning window, count as settled as of the range end",
	"nmi_history measures are NMI's own transaction history, read daily per NMI PSP and kept 25 months: they include what OpenRails sent and what came before it. Rows are dated by their month's first instant, so use month-aligned ranges. one_off_sale cannot separate initial sales from retries of declined rebills",
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

// declineReasonValues are the reason dimension's values.
func declineReasonValues() []string {
	var out []string
	for _, r := range billing.DeclineReasons() {
		out = append(out, string(r))
	}
	slices.Sort(out)
	return out
}
