package metrics

import (
	"fmt"
	"slices"
	"strings"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/internal/modules/money/books"
)

// The books' measures read their rollups: a period's flows are its
// ledger_daily buckets (15-minute UTC buckets of effective_at, grouped into
// the accounting timezone's days), an account's balance at t its
// ledger_type_totals row less its flows from t.

// incomeAccounts are recognized_revenue's: revenue adds, contra revenue and
// bad debt subtract, as their balances sit.
var incomeAccounts = accountNames(
	books.SubscriptionRevenue, books.OneTimeRevenue, books.UsageRevenue, books.BreakageRevenue,
	books.Refunds, books.Chargebacks, books.RevenueAdjustments, books.PromotionalCredit, books.BadDebt,
)

// balanceAccounts are balance's, each on its normal side; customer_units is
// the units book's, per unit.
var balanceAccounts = accountNames(
	books.DeferredRevenue, books.DeferredRevenueCredit, books.UsageReceivable, books.AllowanceDoubtful, books.UnappliedCash,
	books.CustomerUnits,
)

// paymentAccounts are the revenue accounts a payment's bill funds, as cash
// and payments split by account; a fee funds none.
var paymentAccounts = []string{"", string(books.SubscriptionRevenue), string(books.OneTimeRevenue), string(books.UsageRevenue)}

// accountValues is the account dimension's vocabulary: every account a
// books or cash measure splits by.
var accountValues = func() []string {
	out := slices.Clone(incomeAccounts)
	for _, a := range balanceAccounts {
		if !slices.Contains(out, a) {
			out = append(out, a)
		}
	}
	return append([]string{""}, out...)
}()

// transferTypeValues are the general ledger's transfer types.
var transferTypeValues = func() []string {
	out := make([]string, 0, len(books.GeneralTransferTypes))
	for _, t := range books.GeneralTransferTypes {
		out = append(out, string(t))
	}
	return out
}()

func accountNames(types ...books.AccountType) []string {
	out := make([]string, len(types))
	for i, t := range types {
		if _, ok := books.Lookup(t); !ok {
			panic("metrics: " + string(t) + " is not in the books' chart")
		}
		out[i] = string(t)
	}
	return out
}

// normalSign is the SQL sign that puts an account type's balance (credits
// less debits) on its normal side.
func normalSign(column string, accounts []string) string {
	var debit []string
	for _, a := range accounts {
		if acct, _ := books.Lookup(books.AccountType(a)); acct.Class.Normal() == books.Debit {
			debit = append(debit, a)
		}
	}
	if len(debit) == 0 {
		return "1"
	}
	return "CASE WHEN " + column + " IN (" + quoted(debit) + ") THEN -1 ELSE 1 END"
}

// balanceKeys admits the balance accounts, each in its book.
func balanceKeys(alias string) string {
	var keys []string
	for _, a := range balanceAccounts {
		acct, _ := books.Lookup(books.AccountType(a))
		keys = append(keys, fmt.Sprintf("('%s', '%s')", acct.Book, a))
	}
	return "(" + alias + ".book, " + alias + ".account_type) IN (" + strings.Join(keys, ", ") + ")"
}

// paymentAccount is the revenue account a payment's bill funds: a usage
// invoice's or a bill of credit lines only is usage revenue (a lot's money is
// recognised as it is spent or lapses), a subscription's subscription
// revenue, any other one-time revenue. A processor fee funds none.
const paymentAccount = `CASE WHEN p.fee THEN ''
	WHEN EXISTS (SELECT 1 FROM billing.invoices ui WHERE ui.merchant_id = p.merchant_id AND ui.id = p.invoice_id AND ui.origin = 'usage')
		OR (SELECT bool_and(cl.credit_grant IS NOT NULL) FROM billing.invoice_lines cl WHERE cl.merchant_id = p.merchant_id AND cl.invoice_id = p.invoice_id)
		THEN 'usage_revenue'
	WHEN p.subscription_id IS NOT NULL THEN 'subscription_revenue'
	ELSE 'one_time_revenue' END`

// paymentsFrom is the payments family's rows: each payment and reversal, and
// each quarter hour's processor fees a PSP's cash clearing paid, signed as
// they left it (a fee given back adds).
const paymentsFrom = `(SELECT pm.merchant_id, pm.purchased_at, pm.amount, pm.currency, pm.status, pm.rail, pm.psp_id, pm.subscription_id,
		pm.invoice_id, pm.refunded_payment_id, pm.reversal_kind, pm.refund_reason, pm.card_brand, pm.token_type, pm.attempt_kind,
		pm.discount_code, false AS fee
	FROM billing.payments pm
	UNION ALL
	SELECT fl.merchant_id, fl.bucket_at, fl.debits - fl.credits, fl.currency, NULL, fp.rail, fl.psp_id, NULL, NULL, NULL, NULL, NULL,
		NULL, NULL, NULL, NULL, true
	FROM billing.ledger_daily fl
	LEFT JOIN billing.psps fp ON fp.merchant_id = fl.merchant_id AND fp.id = fl.psp_id
	WHERE fl.book = 'general' AND fl.account_type = 'cash_clearing' AND fl.transfer_type IN ('processor_fee', 'chargeback_fee')) p`

// compileLedgerBalance reads each account's balance at the plan's edges: its
// total now less its flows from the first edge (the base, a NULL bucket),
// then each bucket's flows, which foldBalance runs forward. Without a time
// grouping it is the total less the flows from the range's end.
func compileLedgerBalance(plan *Plan, merchantID uuid.UUID, fam Family, spec familySpec, leaves []*Measure) (stmt, error) {
	var args []any
	arg := func(v any) string { args = append(args, v); return fmt.Sprintf("$%d", len(args)) }
	merchant := arg(merchantID)

	dims := func(alias string) ([]string, []string, error) {
		var sel, where []string
		for _, d := range plan.Dims {
			expr, ok := spec.DimExprs[d]
			if !ok {
				return nil, nil, fmt.Errorf("metrics: family %s lacks dimension %s (registry bug)", fam, d)
			}
			sel = append(sel, strings.ReplaceAll(expr, "x.", alias+".")+" AS dim_"+d)
		}
		for _, d := range sortedFilterDims(plan.Filters) {
			expr, ok := spec.DimExprs[d]
			if !ok {
				return nil, nil, fmt.Errorf("metrics: family %s lacks filter dimension %s (registry bug)", fam, d)
			}
			where = append(where, fmt.Sprintf("%s = ANY(%s)", strings.ReplaceAll(expr, "x.", alias+"."), arg(plan.Filters[d])))
		}
		return sel, where, nil
	}
	values := func(alias, credits, debits string, negate bool) string {
		var sel []string
		for _, m := range leaves {
			sign := normalSign(alias+".account_type", balanceAccounts)
			if negate {
				sign = "-(" + sign + ")"
			}
			sel = append(sel, fmt.Sprintf("COALESCE(SUM((%s) * (%s.%s - %s.%s)), 0)::bigint AS m_%s", sign, alias, credits, alias, debits, m.Name))
		}
		return strings.Join(sel, ", ")
	}
	part := func(bucket, alias, table, credits, debits string, negate bool, extra ...string) (string, error) {
		sel, where, err := dims(alias)
		if err != nil {
			return "", err
		}
		cols := append([]string{bucket + " AS bucket"}, sel...)
		cols = append(cols, values(alias, credits, debits, negate))
		scope := strings.ReplaceAll(merchantCols[fam], "x.", alias+".") + " = " + merchant
		where = append([]string{scope, balanceKeys(alias)}, append(extra, where...)...)
		group := make([]string, 0, len(sel)+1)
		for i := range len(sel) + 1 {
			group = append(group, fmt.Sprint(i+1))
		}
		return "SELECT " + strings.Join(cols, ", ") + "\nFROM " + table + " " + alias +
			"\nWHERE " + strings.Join(where, "\n  AND ") + "\nGROUP BY " + strings.Join(group, ", "), nil
	}

	edge := plan.To
	if plan.HasTime {
		edge = plan.Buckets[0]
	}
	from := arg(edge)
	totals, err := part("NULL::timestamptz", "t", "billing.ledger_type_totals", "credits_posted", "debits_posted", false)
	if err != nil {
		return stmt{}, err
	}
	later, err := part("NULL::timestamptz", "d", "billing.ledger_daily", "credits", "debits", true, "d.bucket_at >= "+from)
	if err != nil {
		return stmt{}, err
	}
	parts := []string{totals, later}
	if plan.HasTime {
		last := arg(plan.Buckets[len(plan.Buckets)-1])
		bucket := fmt.Sprintf("date_trunc(%s, d.bucket_at, %s)", arg(plan.Grain), arg(plan.zone().String()))
		flows, err := part(bucket, "d", "billing.ledger_daily", "credits", "debits", false, "d.bucket_at >= "+from, "d.bucket_at < "+last)
		if err != nil {
			return stmt{}, err
		}
		parts = append(parts, flows)
	}
	sql := strings.Join(parts, "\nUNION ALL\n")
	if !plan.HasTime {
		// One row per combination: the totals less the later flows.
		var outer []string
		for _, d := range plan.Dims {
			outer = append(outer, "dim_"+d)
		}
		cols := slices.Clone(outer)
		for _, m := range leaves {
			cols = append(cols, "SUM(m_"+m.Name+")::bigint AS m_"+m.Name)
		}
		sql = "SELECT " + strings.Join(cols, ", ") + "\nFROM (" + sql + ") b"
		if len(outer) > 0 {
			sql += "\nGROUP BY " + strings.Join(outer, ", ")
		}
		return stmt{family: fam, sql: sql, args: args, leaves: leaves}, nil
	}
	return stmt{family: fam, sql: sql, args: args, leaves: leaves, hasTime: true, balance: true}, nil
}
