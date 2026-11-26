//go:build integration

package critical_test

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/catalog"
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/integrations/ccbill"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/reconcile"
	"github.com/open-rails/openrails/internal/stripemock"
	"github.com/stretchr/testify/require"
)

// Movements have no route yet: the scenarios read the tables reports read.

type mrrMove struct {
	Currency, Kind, Cause string
	Delta, Before, After  int64
	Logo                  int
	Voluntary             *bool
	OccurredAt            time.Time
	Lines                 int
}

// mrrMoves are the customer's MRR movements, oldest first.
func mrrMoves(t *testing.T, w *world, client *openrails.Client, customer billing.CustomerID) []mrrMove {
	t.Helper()
	rows, err := w.pool.Query(t.Context(), `SELECT m.currency, m.kind, m.cause, m.delta, m.mrr_before, m.mrr_after, m.logo_delta, m.voluntary, m.occurred_at,
		(SELECT count(*) FROM `+pgx.Identifier{w.schema, "mrr_movement_lines"}.Sanitize()+` l WHERE l.merchant_id = m.merchant_id AND l.movement_id = m.id)
		FROM `+pgx.Identifier{w.schema, "mrr_movements"}.Sanitize()+` m WHERE m.merchant_id = $1 AND m.customer_id = $2 ORDER BY m.occurred_at, m.id`,
		client.MerchantID().UUID(), customer.UUID())
	require.NoError(t, err)
	defer rows.Close()
	var out []mrrMove
	for rows.Next() {
		var m mrrMove
		require.NoError(t, rows.Scan(&m.Currency, &m.Kind, &m.Cause, &m.Delta, &m.Before, &m.After, &m.Logo, &m.Voluntary, &m.OccurredAt, &m.Lines))
		out = append(out, m)
	}
	require.NoError(t, rows.Err())
	return out
}

// mrrKinds is each movement's currency, kind and change.
func mrrKinds(moves []mrrMove) []string {
	out := make([]string, len(moves))
	for i, m := range moves {
		out[i] = mrrKind(m.Currency, m.Kind, m.Delta)
	}
	return out
}

func mrrKind(currency, kind string, delta int64) string {
	return currency + " " + kind + " " + strconv.FormatInt(delta, 10)
}

// mrrEntries are the subscription's scheduled MRR changes, each kind and
// status, oldest first.
func mrrEntries(t *testing.T, w *world, client *openrails.Client, sub billing.SubscriptionID) []string {
	t.Helper()
	rows, err := w.pool.Query(t.Context(), "SELECT kind || ' ' || status FROM "+pgx.Identifier{w.schema, "mrr_scheduled"}.Sanitize()+
		" WHERE merchant_id = $1 AND subscription_id = $2 ORDER BY scheduled_at, id", client.MerchantID().UUID(), sub.UUID())
	require.NoError(t, err)
	out, err := pgx.CollectRows(rows, pgx.RowTo[string])
	require.NoError(t, err)
	return out
}

// mrrFlows sums the merchant's entries of each measure in currency, over
// every bucket.
func mrrFlows(t *testing.T, w *world, client *openrails.Client, currency string) map[string]int64 {
	t.Helper()
	rows, err := w.pool.Query(t.Context(), "SELECT measure, sum(entries)::bigint FROM "+pgx.Identifier{w.schema, "mrr_flows"}.Sanitize()+
		" WHERE merchant_id = $1 AND currency = $2 GROUP BY measure", client.MerchantID().UUID(), currency)
	require.NoError(t, err)
	defer rows.Close()
	out := map[string]int64{}
	for rows.Next() {
		var measure string
		var entries int64
		require.NoError(t, rows.Scan(&measure, &entries))
		out[measure] = entries
	}
	require.NoError(t, rows.Err())
	return out
}

// mrrScheduledFlows counts the merchant's scheduled MRR entries in currency
// by kind and event, as reports derive them: added by scheduled_at, then
// reversed, effected or superseded by resolved_at.
func mrrScheduledFlows(t *testing.T, w *world, client *openrails.Client, currency string) map[string]int64 {
	t.Helper()
	table := pgx.Identifier{w.schema, "mrr_scheduled"}.Sanitize()
	rows, err := w.pool.Query(t.Context(), `SELECT 'scheduled_' || kind || '_added', count(*) FROM `+table+` WHERE merchant_id = $1 AND currency = $2 GROUP BY kind
		UNION ALL SELECT 'scheduled_' || kind || '_' || status, count(*) FROM `+table+` WHERE merchant_id = $1 AND currency = $2 AND resolved_at IS NOT NULL GROUP BY kind, status`,
		client.MerchantID().UUID(), currency)
	require.NoError(t, err)
	defer rows.Close()
	out := map[string]int64{}
	for rows.Next() {
		var measure string
		var entries int64
		require.NoError(t, rows.Scan(&measure, &entries))
		out[measure] = entries
	}
	require.NoError(t, rows.Err())
	return out
}

// mrrTotals is the merchant's MRR now in currency, its customers and
// subscribers.
func mrrTotals(t *testing.T, w *world, client *openrails.Client, currency string) [3]int64 {
	t.Helper()
	var out [3]int64
	require.NoError(t, w.pool.QueryRow(t.Context(), "SELECT mrr, customers, subscribers FROM "+pgx.Identifier{w.schema, "mrr_totals"}.Sanitize()+
		" WHERE merchant_id = $1 AND currency = $2", client.MerchantID().UUID(), currency).Scan(&out[0], &out[1], &out[2]))
	return out
}

// mrrOf is the subscription's MRR as the merchant routes serve it.
func mrrOf(t *testing.T, client *openrails.Client, id billing.SubscriptionID) billing.SubscriptionMRR {
	t.Helper()
	sub, err := client.GetSubscription(t.Context(), id)
	require.NoError(t, err)
	require.NotNil(t, sub.MRR, "a subscription with a price has MRR state")
	return *sub.MRR
}

// mrrPlan is a product of its own, sold at price.
func mrrPlan(t *testing.T, client *openrails.Client, key string, price billing.CreatePriceParams) billing.PriceID {
	t.Helper()
	product, err := client.CreateProduct(t.Context(), billing.CreateProductParams{Key: key, DisplayName: key, Entitlements: []string{key + ":paid"}})
	require.NoError(t, err)
	price.ProductID, price.Key = product.ID, key
	created, err := client.CreatePrice(t.Context(), price)
	require.NoError(t, err)
	return created.ID
}

// buyMRRPlan is alice buying price with a new card; it answers her
// subscription.
func buyMRRPlan(t *testing.T, a *authorityWorld, provider *providerLedger, price billing.PriceID, total int64, key string) *billing.Subscription {
	t.Helper()
	token := provider.core.NewPaymentMethod(stripemock.Card{Brand: "visa", Last4: "4242"})
	input := billing.CreateMyInvoiceParams{Lines: []billing.InvoiceLineParams{{PriceID: price}}, ExpectedTotal: ptr(total), Reusable: ptr(true), Payment: &billing.InvoicePaymentParams{Token: token}}
	status, raw := a.request(t, http.MethodPost, "/v1/me/invoices", a.aliceToken, input, map[string]string{"Idempotency-Key": key})
	require.Equal(t, http.StatusCreated, status, string(raw))
	var invoice billing.Invoice
	require.NoError(t, json.Unmarshal(raw, &invoice))
	require.Equal(t, billing.InvoiceComplete, invoice.Status, string(raw))
	require.NotNil(t, invoice.Lines[0].SubscriptionID)
	sub, err := a.alpha.GetSubscription(t.Context(), *invoice.Lines[0].SubscriptionID)
	require.NoError(t, err)
	return sub
}

// customerCancel is alice canceling sub at the end of its period.
func customerCancel(t *testing.T, a *authorityWorld, sub billing.SubscriptionID) {
	t.Helper()
	status, raw := a.request(t, http.MethodPost, "/v1/me/subscriptions/"+sub.String()+"/cancel", a.aliceToken, map[string]any{"reason": "Customer stops renewing"})
	require.Equal(t, http.StatusOK, status, string(raw))
}

// ccbillBook is n legacy CCBill members on price, one customer each, paid
// five days past asOf; from keeps their ids apart from another book's.
func ccbillBook(price billing.PriceID, asOf time.Time, from, n int) billing.DeclaredBilling {
	book := billing.DeclaredBilling{AsOf: asOf, DefaultPSP: billing.PSPRef{Key: "ccbill"}}
	for i := from; i < from+n; i++ {
		customer := billing.CustomerID(uuid.New())
		// No issuer's range starts with 9 or 0: no id reads as a card number.
		sub, sale := legacyCCBillMember(customer, price, fmt.Sprintf("9%017d", i), fmt.Sprintf("08%017d", i), asOf.Add(5*24*time.Hour))
		book.Customers = append(book.Customers, billing.DeclaredCustomer{Customer: customer})
		book.Subscriptions = append(book.Subscriptions, sub)
		book.Transactions = append(book.Transactions, sale)
	}
	return book
}

// ghostCCBillMembership writes the customer a CCBill membership, opened as
// an import opens one, that a legacy book left behind without its access: no
// grant came of it, so a prune may remove it (a subscription that granted
// access is surfaced, never pruned). It is canceled at its paid end.
func ghostCCBillMembership(t *testing.T, w *world, customer billing.CustomerID, psp uuid.UUID, ref string, start, paid, canceled time.Time) {
	t.Helper()
	var product, price uuid.UUID
	require.NoError(t, w.pool.QueryRow(t.Context(), "SELECT product_id, id FROM "+pgx.Identifier{w.schema, "prices"}.Sanitize()+" WHERE merchant_id = $1",
		w.alpha.MerchantID().UUID()).Scan(&product, &price))
	database, err := db.NewWithPGXPool(slog.Default(), w.pool, w.schema)
	require.NoError(t, err)
	require.NoError(t, database.MerchantTx(merchant.WithID(t.Context(), w.alpha.MerchantID()), func(ctx context.Context, tx pgx.Tx) error {
		if err := db.SetMRRCause(ctx, gen.New(tx), db.MRRCauseImport); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, "INSERT INTO billing.customers (merchant_id, id) VALUES ($1, $2) ON CONFLICT DO NOTHING", w.alpha.MerchantID().UUID(), customer.UUID()); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, "INSERT INTO billing.subscriptions (merchant_id, customer_id, product_id, price_id, status, rail, collection_policy, rail_subscription_id, psp_id,"+
			" started_at, current_period_starts_at, current_period_ends_at, canceled_at, ended_at, cancel_type, created_at, updated_at)"+
			" VALUES ($1, $2, $3, $4, 'canceled', 'ccbill', 'provider', $5, $6, $7, $7, $8, $9, $8, 'user', $10, $10)",
			w.alpha.MerchantID().UUID(), customer.UUID(), product, price, ref, psp, start, paid, canceled, w.clock.Now())
		return err
	}))
}

// subscriptionMRRReads is the rows and index entries reads of
// subscription_mrr returned: since the statistics began, or in q's
// transaction (xact).
func subscriptionMRRReads(t *testing.T, w *world, q interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}, xact bool) int64 {
	t.Helper()
	fn := "pg_stat_get_tuples_returned"
	if xact {
		fn = "pg_stat_get_xact_tuples_returned"
	}
	var n int64
	require.NoError(t, q.QueryRow(t.Context(), `SELECT (`+fn+`(c.oid) + COALESCE((SELECT sum(`+fn+`(i.indexrelid)) FROM pg_index i WHERE i.indrelid = c.oid), 0))::bigint
		FROM pg_class c WHERE c.oid = $1::regclass`, pgx.Identifier{w.schema, "subscription_mrr"}.Sanitize()).Scan(&n))
	return n
}

// subscriptionMRRScans is, for subscription_mrr and each of its indexes,
// its scans and the rows or entries they returned, as subscriptionMRRReads
// counts: where a write's reads went.
func subscriptionMRRScans(t *testing.T, w *world, q interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}, xact bool) string {
	t.Helper()
	scans, returned := "pg_stat_get_numscans", "pg_stat_get_tuples_returned"
	if xact {
		scans, returned = "pg_stat_get_xact_numscans", "pg_stat_get_xact_tuples_returned"
	}
	var out string
	require.NoError(t, q.QueryRow(t.Context(), `SELECT string_agg(r.name || ' ' || `+scans+`(r.oid) || ' scans ' || `+returned+`(r.oid) || ' read', '; ' ORDER BY r.name)
		FROM (SELECT c.oid, 'table' AS name FROM pg_class c WHERE c.oid = $1::regclass
		      UNION ALL SELECT i.indexrelid, i.indexrelid::regclass::text FROM pg_index i WHERE i.indrelid = $1::regclass) r`,
		pgx.Identifier{w.schema, "subscription_mrr"}.Sanitize()).Scan(&out))
	return out
}

// Monthly MRR of the fixture prices. A named cadence divides exactly (720
// hours by one, 8760 by twelve), a week counts 52 over 12, and any other
// window 730.485 hours a month: 10.00 a day is 304.36875.
const (
	dailyTenMRR    = 304_368_750
	dailyFiveMRR   = 152_184_375
	dailyTwentyMRR = 608_737_500
	// 7.00 a week, rounded once.
	weeklyMRR = 30_333_333
	// The CCBill fixtures bill 10.00 and 20.00 every 720 hours, monthly.
	ccbillMemberMRR = 10_000_000
	ccbillPlusMRR   = 20_000_000
)

func TestMRR(t *testing.T) {
	t.Run("OR-J42-S01/new-expansion-contraction-churn-reactivation-once-normalised-per-currency", func(t *testing.T) {
		a, provider := newPaymentWorld(t, func(w *world) { w.clock.Advance(-30 * 24 * time.Hour) })
		alice := customerOf(a.alice)
		daily := mrrPlan(t, a.alpha, "mrr-daily", billing.CreatePriceParams{UnitAmount: 10_000_000, Currency: "USD", BillingIntervalHours: ptr(24), AccessDurationHours: ptr(24)})
		annual := mrrPlan(t, a.alpha, "mrr-annual", billing.CreatePriceParams{UnitAmount: 120_000_000, Currency: "USD", BillingIntervalHours: ptr(8760), AccessDurationHours: ptr(8760)})
		quarterly := mrrPlan(t, a.alpha, "mrr-quarterly", billing.CreatePriceParams{UnitAmount: 30_000_000, Currency: "EUR", BillingIntervalMonths: ptr(3)})
		weekly := mrrPlan(t, a.alpha, "mrr-weekly", billing.CreatePriceParams{UnitAmount: 7_000_000, Currency: "USD", BillingIntervalHours: ptr(168), AccessDurationHours: ptr(168)})

		// A day's price counts 730.485 hours a month.
		first := buyMRRPlan(t, a, provider, daily, 10_000_000, "mrr-daily")
		require.Equal(t, billing.SubscriptionMRR{Amount: dailyTenMRR, Currency: "USD"}, mrrOf(t, a.alpha, first.ID))
		// A yearly price, 8760 hours, counts a twelfth.
		yearly := buyMRRPlan(t, a, provider, annual, 120_000_000, "mrr-annual")
		require.Equal(t, billing.SubscriptionMRR{Amount: 10_000_000, Currency: "USD"}, mrrOf(t, a.alpha, yearly.ID))
		// The merchant ends the year now: the customer keeps MRR, less.
		_, err := a.alpha.CancelSubscription(t.Context(), yearly.ID, billing.CancelSubscriptionParams{Reason: "Merchant ends the yearly plan", RevokeAccess: true})
		require.NoError(t, err)
		require.Equal(t, billing.SubscriptionMRR{Amount: 0, Currency: "USD"}, mrrOf(t, a.alpha, yearly.ID))
		// And then the day plan: none left.
		_, err = a.alpha.CancelSubscription(t.Context(), first.ID, billing.CancelSubscriptionParams{Reason: "Merchant ends the day plan", RevokeAccess: true})
		require.NoError(t, err)
		// Buying again is coming back.
		again := buyMRRPlan(t, a, provider, daily, 10_000_000, "mrr-daily-again")
		require.NotEqual(t, first.ID, again.ID)
		// A quarter of calendar months counts a third, in its own currency.
		euro := buyMRRPlan(t, a, provider, quarterly, 30_000_000, "mrr-quarterly")
		require.Equal(t, billing.SubscriptionMRR{Amount: 10_000_000, Currency: "EUR"}, mrrOf(t, a.alpha, euro.ID))
		// A week counts 52 of them over 12 months.
		week := buyMRRPlan(t, a, provider, weekly, 7_000_000, "mrr-weekly")
		require.Equal(t, billing.SubscriptionMRR{Amount: weeklyMRR, Currency: "USD"}, mrrOf(t, a.alpha, week.ID))

		moves := mrrMoves(t, a.world, a.alpha, alice)
		require.Equal(t, []string{
			mrrKind("USD", "new", dailyTenMRR),
			mrrKind("USD", "expansion", 10_000_000),
			mrrKind("USD", "contraction", -10_000_000),
			mrrKind("USD", "churn", -dailyTenMRR),
			mrrKind("USD", "reactivation", dailyTenMRR),
			mrrKind("EUR", "new", 10_000_000),
			mrrKind("USD", "expansion", weeklyMRR),
		}, mrrKinds(moves))
		for i, logo := range []int{1, 0, 0, -1, 1, 1, 0} {
			require.Equal(t, logo, moves[i].Logo, "%s", moves[i].Kind)
			require.Equal(t, 1, moves[i].Lines, "%s: one subscription moved", moves[i].Kind)
		}
		require.Equal(t, []string{"subscribed", "subscribed", "merchant_canceled", "merchant_canceled", "subscribed", "subscribed", "subscribed"},
			[]string{moves[0].Cause, moves[1].Cause, moves[2].Cause, moves[3].Cause, moves[4].Cause, moves[5].Cause, moves[6].Cause})
		require.Equal(t, int64(dailyTenMRR+10_000_000), moves[1].After)
		require.NotNil(t, moves[3].Voluntary)
		require.True(t, *moves[3].Voluntary, "a merchant's cancel is voluntary churn")
		for _, m := range []mrrMove{moves[0], moves[1], moves[2], moves[4], moves[5], moves[6]} {
			require.Nil(t, m.Voluntary, "%s", m.Kind)
		}

		require.Equal(t, [3]int64{dailyTenMRR + weeklyMRR, 1, 2}, mrrTotals(t, a.world, a.alpha, "USD"))
		require.Equal(t, [3]int64{10_000_000, 1, 1}, mrrTotals(t, a.world, a.alpha, "EUR"))
		require.Equal(t, map[string]int64{"new": 1, "expansion": 2, "contraction": 1, "churn": 1, "churn_voluntary": 1, "reactivation": 1},
			mrrFlows(t, a.world, a.alpha, "USD"), "each movement is counted once")
		require.Equal(t, map[string]int64{"new": 1}, mrrFlows(t, a.world, a.alpha, "EUR"))
	})

	t.Run("OR-J42-S02/cancel-at-period-end-is-scheduled-churn-a-resume-reverses-it-the-end-churns", func(t *testing.T) {
		a, _, sub := purchasedMembership(t)
		alice := customerOf(a.alice)
		end := *sub.CurrentPeriodEndsAt
		customerCancel(t, a, sub.ID)

		mrr := mrrOf(t, a.alpha, sub.ID)
		require.EqualValues(t, dailyTenMRR, mrr.Amount, "a canceled subscription adds its MRR until its end")
		require.NotNil(t, mrr.Scheduled)
		require.Equal(t, billing.ScheduledMRRChurn, mrr.Scheduled.Kind)
		require.EqualValues(t, -dailyTenMRR, mrr.Scheduled.Amount)
		require.True(t, mrr.Scheduled.EffectiveAt.Equal(end), "effective at %s", mrr.Scheduled.EffectiveAt)
		require.Equal(t, []string{mrrKind("USD", "new", dailyTenMRR)}, mrrKinds(mrrMoves(t, a.world, a.alpha, alice)), "scheduled churn moves no MRR")
		// The scheduled-churn list is the subscriptions scheduled by a date.
		listed, err := a.alpha.ListSubscriptions(t.Context(), billing.SubscriptionListParams{ScheduledChangeBefore: end})
		require.NoError(t, err)
		require.Len(t, listed.Items, 1)
		require.Equal(t, sub.ID, listed.Items[0].ID)
		require.NotNil(t, listed.Items[0].MRR)
		require.NotNil(t, listed.Items[0].MRR.Scheduled)
		earlier, err := a.alpha.ListSubscriptions(t.Context(), billing.SubscriptionListParams{ScheduledChangeBefore: end.Add(-time.Microsecond)})
		require.NoError(t, err)
		require.Empty(t, earlier.Items)

		resumed, err := a.alpha.ResumeSubscription(t.Context(), sub.ID)
		require.NoError(t, err)
		require.NotNil(t, resumed.MRR)
		require.Equal(t, billing.SubscriptionMRR{Amount: dailyTenMRR, Currency: "USD"}, *resumed.MRR, "a resume reverses the scheduled churn")
		require.Equal(t, []string{"churn reversed"}, mrrEntries(t, a.world, a.alpha, sub.ID))
		require.Len(t, mrrMoves(t, a.world, a.alpha, alice), 1)

		customerCancel(t, a, sub.ID)
		require.Equal(t, []string{"churn reversed", "churn scheduled"}, mrrEntries(t, a.world, a.alpha, sub.ID))
		a.clock.Advance(end.Sub(a.clock.Now()))
		fleet := startFleet(t, a.world)
		runRegistered(t, fleet, "openrails.billing_pass", 2)
		require.Eventually(t, func() bool { return len(mrrMoves(t, a.world, a.alpha, alice)) == 2 }, 20*time.Second, 20*time.Millisecond, "the end churns")
		moves := mrrMoves(t, a.world, a.alpha, alice)
		require.Equal(t, []string{mrrKind("USD", "new", dailyTenMRR), mrrKind("USD", "churn", -dailyTenMRR)}, mrrKinds(moves))
		require.True(t, moves[1].OccurredAt.Equal(end), "the churn is dated at the end, %s", moves[1].OccurredAt)
		require.Equal(t, "canceled", moves[1].Cause)
		require.Equal(t, -1, moves[1].Logo)
		require.NotNil(t, moves[1].Voluntary)
		require.True(t, *moves[1].Voluntary)
		require.Equal(t, []string{"churn reversed", "churn effected"}, mrrEntries(t, a.world, a.alpha, sub.ID))
		require.Equal(t, billing.SubscriptionMRR{Amount: 0, Currency: "USD"}, mrrOf(t, a.alpha, sub.ID))
		require.Equal(t, map[string]int64{"scheduled_churn_added": 2, "scheduled_churn_reversed": 1, "scheduled_churn_effected": 1},
			mrrScheduledFlows(t, a.world, a.alpha, "USD"))
		require.EqualValues(t, 1, mrrFlows(t, a.world, a.alpha, "USD")["churn_voluntary"])
		require.Equal(t, [3]int64{0, 0, 0}, mrrTotals(t, a.world, a.alpha, "USD"))
	})

	t.Run("OR-J42-S03/scheduled-downgrade-is-scheduled-contraction-applied-at-renewal-a-canceled-change-reverses-it", func(t *testing.T) {
		a, _, sub := purchasedMembership(t)
		alice := customerOf(a.alice)
		_, err := a.alpha.UpdateProduct(t.Context(), sub.ProductID, billing.UpdateProductParams{TierGroup: catalog.Value("membership-level"), TierRank: catalog.Value(2)})
		require.NoError(t, err)
		lower, err := a.alpha.CreateProduct(t.Context(), billing.CreateProductParams{Key: "lower-membership", DisplayName: "Lower membership", TierGroup: ptr("membership-level"), TierRank: 1, Entitlements: []string{"membership:lower"}})
		require.NoError(t, err)
		smaller, err := a.alpha.CreatePrice(t.Context(), billing.CreatePriceParams{ProductID: lower.ID, Key: "lower-day", UnitAmount: 5_000_000, Currency: "USD", BillingIntervalHours: ptr(24), AccessDurationHours: ptr(24)})
		require.NoError(t, err)
		due := *sub.CurrentPeriodEndsAt
		downgrade := billing.ChangeSubscriptionParams{PriceID: &smaller.ID, Reason: "Customer asked for the smaller plan"}
		changed, err := a.alpha.ChangeSubscription(t.Context(), "mrr-downgrade", sub.ID, downgrade)
		require.NoError(t, err)
		require.Equal(t, "period_end", changed.Effective)
		mrr := mrrOf(t, a.alpha, sub.ID)
		require.EqualValues(t, dailyTenMRR, mrr.Amount, "a scheduled downgrade moves nothing yet")
		require.NotNil(t, mrr.Scheduled)
		require.Equal(t, billing.ScheduledMRRContraction, mrr.Scheduled.Kind)
		require.EqualValues(t, dailyFiveMRR-dailyTenMRR, mrr.Scheduled.Amount)
		require.True(t, mrr.Scheduled.EffectiveAt.Equal(due), "the renewal it applies at, %s", mrr.Scheduled.EffectiveAt)

		// Changing back to the current plan cancels the change.
		_, err = a.alpha.ChangeSubscription(t.Context(), "mrr-keep", sub.ID, billing.ChangeSubscriptionParams{PriceID: &sub.PriceID, Reason: "Customer kept the plan"})
		require.NoError(t, err)
		current, err := a.alpha.GetSubscription(t.Context(), sub.ID)
		require.NoError(t, err)
		require.Nil(t, current.ScheduledChange)
		require.NotNil(t, current.MRR)
		require.Nil(t, current.MRR.Scheduled, "a canceled change reverses its contraction")
		require.Equal(t, []string{"contraction reversed"}, mrrEntries(t, a.world, a.alpha, sub.ID))

		_, err = a.alpha.ChangeSubscription(t.Context(), "mrr-downgrade-again", sub.ID, downgrade)
		require.NoError(t, err)
		require.Equal(t, []string{"contraction reversed", "contraction scheduled"}, mrrEntries(t, a.world, a.alpha, sub.ID))
		a.clock.Advance(due.Sub(a.clock.Now()))
		fleet := startFleet(t, a.world)
		runRegistered(t, fleet, "openrails.billing_pass", 2)
		require.Eventually(t, func() bool {
			current, err := a.alpha.GetSubscription(t.Context(), sub.ID)
			return err == nil && current.PriceID == smaller.ID && current.CurrentPeriodStartsAt.Equal(due)
		}, 20*time.Second, 20*time.Millisecond, "the renewal bills the smaller plan")
		require.Equal(t, billing.SubscriptionMRR{Amount: dailyFiveMRR, Currency: "USD"}, mrrOf(t, a.alpha, sub.ID))
		moves := mrrMoves(t, a.world, a.alpha, alice)
		require.Equal(t, []string{mrrKind("USD", "new", dailyTenMRR), mrrKind("USD", "contraction", dailyFiveMRR-dailyTenMRR)}, mrrKinds(moves))
		require.Equal(t, "price_change", moves[1].Cause)
		require.Equal(t, []string{"contraction reversed", "contraction effected"}, mrrEntries(t, a.world, a.alpha, sub.ID))
		require.Equal(t, map[string]int64{"scheduled_contraction_added": 2, "scheduled_contraction_reversed": 1, "scheduled_contraction_effected": 1},
			mrrScheduledFlows(t, a.world, a.alpha, "USD"))
	})

	t.Run("OR-J42-S04/stripe-cancel-at-period-end-is-scheduled-churn-a-portal-resume-reverses-it", func(t *testing.T) {
		tw := newTrailWorld(t, "stripe")
		tw.stripe.AddProduct("prod_basic", "basic")
		tw.stripe.AddPrice(stripemock.Price{ID: "price_basic", Product: "prod_basic", Amount: 900, Interval: "month"})
		tw.mapStripe(t, tw.stripeProduct(t, "basic", "member:basic"), "stripe-basic", "price_basic")
		start := tw.clock.Now().Add(-24 * time.Hour)
		customer := tw.stripe.AddCustomer(tw.alice.Email, "Alice", nil)
		remote := tw.stripe.AddSubscription(customer, "pm_alice", "price_basic", 900, start, start.AddDate(0, 1, 0))
		tw.stripe.TagSubscription(remote, "user_id", tw.alice.ID, start)
		tw.invoicePaid(t, remote)
		completedOnce(t, tw.fleet, kindConverge)
		alice := customerOf(tw.alice)
		held := tw.subscriptions(t, alice)
		require.Len(t, held, 1)
		sub := held[0]
		require.Equal(t, billing.SubscriptionMRR{Amount: 9_000_000, Currency: "USD"}, mrrOf(t, tw.alpha, sub.ID))

		tw.send(t, "customer.subscription.updated", tw.stripe.PortalCancelAtPeriodEnd(remote, true))
		completedOnce(t, tw.fleet, kindConverge)
		canceled, err := tw.alpha.GetSubscription(t.Context(), sub.ID)
		require.NoError(t, err)
		require.Equal(t, billing.SubscriptionCanceled, canceled.Status, "the portal cancel is the customer's period-end cancel")
		require.NotNil(t, canceled.MRR)
		require.EqualValues(t, 9_000_000, canceled.MRR.Amount)
		require.NotNil(t, canceled.MRR.Scheduled)
		require.Equal(t, billing.ScheduledMRRChurn, canceled.MRR.Scheduled.Kind)
		require.EqualValues(t, -9_000_000, canceled.MRR.Scheduled.Amount)
		require.True(t, canceled.MRR.Scheduled.EffectiveAt.Equal(*sub.CurrentPeriodEndsAt))

		tw.send(t, "customer.subscription.updated", tw.stripe.PortalCancelAtPeriodEnd(remote, false))
		completedOnce(t, tw.fleet, kindConverge)
		resumed, err := tw.alpha.GetSubscription(t.Context(), sub.ID)
		require.NoError(t, err)
		require.Equal(t, billing.SubscriptionActive, resumed.Status)
		require.NotNil(t, resumed.MRR)
		require.Equal(t, billing.SubscriptionMRR{Amount: 9_000_000, Currency: "USD"}, *resumed.MRR)
		require.Equal(t, []string{"churn reversed"}, mrrEntries(t, tw.world, tw.alpha, sub.ID))
		require.Equal(t, []string{mrrKind("USD", "new", 9_000_000)}, mrrKinds(mrrMoves(t, tw.world, tw.alpha, alice)), "neither moves MRR")
	})

	t.Run("OR-J42-S05/ccbill-upgrade-replacing-a-subscription-is-one-expansion-an-import-opens", func(t *testing.T) {
		a := ccbillWebhookWorld(t)
		price := ccbillMemberPrice(t, a.alpha)
		upper, err := a.alpha.CreateProduct(t.Context(), billing.CreateProductParams{Key: "ccbill-plus", DisplayName: "CCBill plus", Entitlements: []string{"ccbill:plus"}})
		require.NoError(t, err)
		plus, err := a.alpha.CreateProviderPrice(t.Context(), billing.CreateProviderPriceParams{ProductID: upper.ID, Key: "ccbill-plus-monthly", PSP: "ccbill", FlexID: "critical-plus-flex",
			Currency: "USD", UnitAmount: ptr(int64(20_000_000)), BillingIntervalHours: ptr(720)})
		require.NoError(t, err)
		bob := customerOf(a.bob)
		now := a.clock.Now().UTC().Truncate(time.Second)
		paid := now.Add(5 * 24 * time.Hour)
		legacy, sale := legacyCCBillMember(bob, price, "223344556677889900", "0812233445566778802", paid)
		importLegacyCCBill(t, a.alpha, now)(legacy, sale)
		sub := onlySubscription(t, a.alpha, bob)
		require.Equal(t, billing.SubscriptionMRR{Amount: ccbillMemberMRR, Currency: "USD"}, mrrOf(t, a.alpha, sub.ID))
		moves := mrrMoves(t, a.world, a.alpha, bob)
		require.Equal(t, []string{mrrKind("USD", "opening", ccbillMemberMRR)}, mrrKinds(moves), "an import is not new business")
		require.Equal(t, "import", moves[0].Cause)
		require.Equal(t, 1, moves[0].Logo)
		started := legacy.StartedAt.UTC()
		var cohort time.Time
		require.NoError(t, a.pool.QueryRow(t.Context(), "SELECT first_active_at FROM "+pgx.Identifier{a.schema, "customer_mrr"}.Sanitize()+
			" WHERE merchant_id = $1 AND customer_id = $2 AND currency = 'USD'", a.alpha.MerchantID().UUID(), bob.UUID()).Scan(&cohort))
		require.True(t, cohort.Equal(started), "the adopted cohort is when it started, %s", cohort)

		// A minute later CCBill replaces its subscription with the upgrade's;
		// OpenRails keeps the row and moves it onto the new price in one
		// transaction. (The import's row is dated by the database's clock.)
		a.clock.Advance(time.Minute)
		next := a.clock.Now().UTC().Add(720 * time.Hour)
		status, body := ccbillNotice(t, a, "UpgradeSuccess", map[string]any{"originalSubscriptionId": "0223344556677889900", "subscriptionId": "0667788990011223344",
			"transactionId": "0812233445566778810", "flexId": "critical-plus-flex", "billedInitialPrice": "20.00", "billedRecurringPrice": "20.00", "recurringPeriod": "30",
			"billedCurrencyCode": "840", "nextRenewalDate": next.Format(time.DateOnly), "cardType": "AMEX", "last4": "0005"})
		require.Equal(t, http.StatusOK, status, body)
		upgraded := onlySubscription(t, a.alpha, bob)
		require.Equal(t, plus.ID, upgraded.PriceID)
		require.Equal(t, billing.SubscriptionMRR{Amount: ccbillPlusMRR, Currency: "USD"}, mrrOf(t, a.alpha, upgraded.ID))
		moves = mrrMoves(t, a.world, a.alpha, bob)
		require.Equal(t, []string{mrrKind("USD", "opening", ccbillMemberMRR), mrrKind("USD", "expansion", ccbillPlusMRR-ccbillMemberMRR)}, mrrKinds(moves), "one expansion: no churn, no new")
		require.Equal(t, "price_change", moves[1].Cause)
		require.Equal(t, 0, moves[1].Logo)
		require.Equal(t, 1, moves[1].Lines)
	})

	t.Run("OR-J42-S06/mrr-at-a-past-date-equals-the-movements-and-every-report-read-has-an-index", func(t *testing.T) {
		zone, err := time.LoadLocation("America/New_York")
		require.NoError(t, err)
		// New York leaves daylight time at 2:00 on 2 November 2025: that day
		// has 25 hours, 04:00 to 05:00 UTC the next day.
		bought := time.Date(2025, time.November, 1, 22, 0, 0, 0, time.UTC)
		a, provider := newPaymentWorld(t, func(w *world) {
			w.clock.Advance(bought.Sub(w.clock.Now()))
			m := cfgMerchant(w.cfg, "alpha")
			m.Accounting = &billing.AccountingSettings{Timezone: "America/New_York"}
			setCfgMerchant(&w.cfg, "alpha", m)
		})
		settings, err := a.alphaRemote.GetMerchantConfiguration(t.Context())
		require.NoError(t, err)
		require.NotNil(t, settings.Accounting)
		accounting := settings.Accounting.Timezone
		require.Equal(t, "America/New_York", accounting)
		alice := customerOf(a.alice)
		small := mrrPlan(t, a.alpha, "mrr-year-small", billing.CreatePriceParams{UnitAmount: 120_000_000, Currency: "USD", BillingIntervalHours: ptr(8760), AccessDurationHours: ptr(8760)})
		large := mrrPlan(t, a.alpha, "mrr-year-large", billing.CreatePriceParams{UnitAmount: 240_000_000, Currency: "USD", BillingIntervalHours: ptr(8760), AccessDurationHours: ptr(8760)})

		// Saturday 18:00 EDT, Sunday 23:30 EST and Monday 00:15 EST: UTC
		// days would put the second and third on one day.
		first := buyMRRPlan(t, a, provider, small, 120_000_000, "mrr-year-small")
		a.clock.Advance(time.Date(2025, time.November, 3, 4, 30, 0, 0, time.UTC).Sub(a.clock.Now()))
		buyMRRPlan(t, a, provider, large, 240_000_000, "mrr-year-large")
		a.clock.Advance(45 * time.Minute)
		_, err = a.alpha.CancelSubscription(t.Context(), first.ID, billing.CancelSubscriptionParams{Reason: "Merchant ends the small plan", RevokeAccess: true})
		require.NoError(t, err)
		require.Equal(t, []string{mrrKind("USD", "new", 10_000_000), mrrKind("USD", "expansion", 20_000_000), mrrKind("USD", "contraction", -10_000_000)},
			mrrKinds(mrrMoves(t, a.world, a.alpha, alice)))

		flows := pgx.Identifier{a.schema, "mrr_flows"}.Sanitize()
		kinds := "('new', 'expansion', 'contraction', 'churn', 'reactivation', 'opening', 'correction')"
		// Each local day's movements, from the buckets grouped in a zone.
		byDay := `SELECT d.day || ' ' || d.amount::text FROM (SELECT to_char(f.bucket_at AT TIME ZONE $3, 'YYYY-MM-DD') AS day, sum(f.amount) AS amount
			FROM ` + flows + ` f WHERE f.merchant_id = $1 AND f.currency = $2 AND f.measure IN ` + kinds + ` GROUP BY 1) d ORDER BY d.day`
		days := func(zone string) []string {
			t.Helper()
			rows, err := a.pool.Query(t.Context(), byDay, a.alpha.MerchantID().UUID(), "USD", zone)
			require.NoError(t, err)
			out, err := pgx.CollectRows(rows, pgx.RowTo[string])
			require.NoError(t, err)
			return out
		}
		require.Equal(t, []string{"2025-11-01 10000000", "2025-11-02 20000000", "2025-11-03 -10000000"}, days(accounting), "New York days, the 25-hour one included")
		require.Equal(t, []string{"2025-11-01 10000000", "2025-11-03 10000000"}, days("UTC"), "the same buckets in UTC days")

		// MRR at a local day's start: now less the movements of the buckets
		// from it on, equal to the movements before it.
		pastMRR := `SELECT t.mrr - COALESCE((SELECT sum(f.amount) FROM ` + flows + ` f
			WHERE f.merchant_id = t.merchant_id AND f.currency = t.currency AND f.bucket_at >= $3 AND f.measure IN ` + kinds + `), 0)::bigint
			FROM ` + pgx.Identifier{a.schema, "mrr_totals"}.Sanitize() + ` t WHERE t.merchant_id = $1 AND t.currency = $2`
		movements := pgx.Identifier{a.schema, "mrr_movements"}.Sanitize()
		for _, sample := range []struct {
			day  int
			want int64
		}{{1, 0}, {2, 10_000_000}, {3, 30_000_000}, {4, 20_000_000}} {
			start := time.Date(2025, time.November, sample.day, 0, 0, 0, 0, zone)
			var fromBuckets, fromMovements int64
			require.NoError(t, a.pool.QueryRow(t.Context(), pastMRR, a.alpha.MerchantID().UUID(), "USD", start).Scan(&fromBuckets))
			require.NoError(t, a.pool.QueryRow(t.Context(), "SELECT COALESCE(sum(delta), 0)::bigint FROM "+movements+
				" WHERE merchant_id = $1 AND currency = $2 AND occurred_at < $3", a.alpha.MerchantID().UUID(), "USD", start).Scan(&fromMovements))
			require.Equal(t, sample.want, fromBuckets, "MRR at the start of New York's %d November", sample.day)
			require.Equal(t, fromMovements, fromBuckets, "MRR at the start of New York's %d November equals its movements", sample.day)
		}

		// With sequential scans off each read still plans on its index: proof
		// that one serves it, not the plan a large table would get.
		scheduled := pgx.Identifier{a.schema, "mrr_scheduled"}.Sanitize()
		added := `SELECT to_char(e.scheduled_at AT TIME ZONE $3, 'YYYY-MM-DD'), e.kind, count(*), sum(e.mrr_delta) FROM ` + scheduled + ` e
			WHERE e.merchant_id = $1 AND e.currency = $2 AND e.scheduled_at >= $4 AND e.scheduled_at < $5 GROUP BY 1, 2`
		resolved := `SELECT to_char(e.resolved_at AT TIME ZONE $3, 'YYYY-MM-DD'), e.kind, e.status, count(*), sum(e.mrr_delta) FROM ` + scheduled + ` e
			WHERE e.merchant_id = $1 AND e.currency = $2 AND e.resolved_at >= $4 AND e.resolved_at < $5 GROUP BY 1, 2, 3`
		atRisk := `SELECT e.currency, e.kind, sum(e.mrr_delta) FROM ` + scheduled + ` e
			WHERE e.merchant_id = $1 AND e.status = 'scheduled' AND e.effective_at <= $2 GROUP BY 1, 2`
		month := []any{a.alpha.MerchantID().UUID(), "USD", accounting, time.Date(2025, time.November, 1, 0, 0, 0, 0, zone), time.Date(2025, time.December, 1, 0, 0, 0, 0, zone)}
		due := `SELECT m.next_change_at, m.merchant_id, m.subscription_id FROM ` + pgx.Identifier{a.schema, "subscription_mrr"}.Sanitize() + ` m
			WHERE m.next_change_at <= $1 AND ($2::timestamptz IS NULL OR (m.next_change_at, m.merchant_id, m.subscription_id) > ($2::timestamptz, $3::uuid, $4::uuid))
			ORDER BY m.next_change_at, m.merchant_id, m.subscription_id LIMIT 200`
		for _, read := range []struct {
			sql  string
			args []any
			uses string
		}{
			{byDay, []any{a.alpha.MerchantID().UUID(), "USD", accounting}, "mrr_flows_pkey"},
			{pastMRR, []any{a.alpha.MerchantID().UUID(), "USD", time.Date(2025, time.November, 3, 0, 0, 0, 0, zone)}, "mrr_flows_pkey"},
			{due, []any{a.clock.Now(), nil, nil, nil}, "subscription_mrr_next_change_at_idx"},
			{added, month, "mrr_scheduled_scheduled_at_idx"},
			{resolved, month, "mrr_scheduled_resolved_at_idx"},
			{atRisk, []any{a.alpha.MerchantID().UUID(), a.clock.Now().AddDate(0, 1, 0)}, "mrr_scheduled_effective_at_idx"},
		} {
			plan := func() []string {
				tx, err := a.pool.Begin(t.Context())
				require.NoError(t, err)
				defer func() { _ = tx.Rollback(context.Background()) }()
				_, err = tx.Exec(t.Context(), "SET LOCAL enable_seqscan = off")
				require.NoError(t, err)
				rows, err := tx.Query(t.Context(), "EXPLAIN "+read.sql, read.args...)
				require.NoError(t, err)
				plan, err := pgx.CollectRows(rows, pgx.RowTo[string])
				require.NoError(t, err)
				return plan
			}()
			text := strings.Join(plan, "\n")
			require.NotContains(t, text, "Seq Scan", text)
			require.Contains(t, text, read.uses, text)
			require.False(t, slices.ContainsFunc(plan, func(line string) bool { return strings.Contains(line, "mrr_movements") }), text)
		}
	})
	t.Run("OR-J42-S07/an-engine-upgrade-replacing-a-subscription-is-one-expansion", func(t *testing.T) {
		a, _, sub := purchasedMembership(t)
		alice := customerOf(a.alice)
		price := upgradePrice(t, a, sub)
		a.clock.Advance(12 * time.Hour)
		status, raw := a.request(t, http.MethodPost, "/v1/me/subscriptions/"+sub.ID.String()+"/change", a.aliceToken, map[string]any{"price_id": price.ID},
			map[string]string{"Idempotency-Key": "mrr-upgrade"})
		require.Equal(t, http.StatusOK, status, string(raw))
		var changed billing.SubscriptionChange
		require.NoError(t, json.Unmarshal(raw, &changed))
		require.Equal(t, "succeeded", changed.Status, string(raw))
		require.NotNil(t, changed.SubscriptionID)
		require.NotEqual(t, sub.ID, *changed.SubscriptionID, "the engine replaces the subscription")

		require.Equal(t, billing.SubscriptionMRR{Amount: 0, Currency: "USD"}, mrrOf(t, a.alpha, sub.ID), "the replaced subscription leaves at once")
		require.Equal(t, billing.SubscriptionMRR{Amount: dailyTwentyMRR, Currency: "USD"}, mrrOf(t, a.alpha, *changed.SubscriptionID))
		moves := mrrMoves(t, a.world, a.alpha, alice)
		require.Equal(t, []string{mrrKind("USD", "new", dailyTenMRR), mrrKind("USD", "expansion", dailyTwentyMRR-dailyTenMRR)}, mrrKinds(moves),
			"one expansion: no churn, no new")
		require.Equal(t, "upgrade", moves[1].Cause, "the upgrade explains the movement")
		require.Equal(t, 0, moves[1].Logo)
		require.Equal(t, 2, moves[1].Lines, "both subscriptions moved")
		require.Equal(t, [3]int64{dailyTwentyMRR, 1, 1}, mrrTotals(t, a.world, a.alpha, "USD"))
		require.Equal(t, map[string]int64{"new": 1, "expansion": 1}, mrrFlows(t, a.world, a.alpha, "USD"))
	})

	t.Run("OR-J42-S08/a-prune-and-its-undo-are-corrections-the-pruned-scheduled-churn-is-superseded", func(t *testing.T) {
		var feed *httptest.Server
		var rebill time.Time
		w := newWorld(t, func(w *world) {
			rebill = w.clock.Now().Truncate(time.Second).Add(-10 * 24 * time.Hour)
			feed = dataLinkFeedFor(t, ccbillProvider, "0912233445566778801", rebill)
			declareCCBill(w, feed.URL)
		})
		price := ccbillMemberPrice(t, w.alpha)
		kept, gone := billing.CustomerID(uuid.New()), billing.CustomerID(uuid.New())
		start, paid := rebill.Add(-720*time.Hour), rebill.Add(720*time.Hour)
		res, err := w.alpha.ImportBilling(t.Context(), billing.DeclaredBilling{AsOf: w.clock.Now(), DefaultPSP: billing.PSPRef{Key: "ccbill"},
			Customers: []billing.DeclaredCustomer{{Customer: kept}},
			Subscriptions: []billing.DeclaredSubscription{
				{SourceID: "kept", Customer: kept, Price: price, Rail: "ccbill", RailSubscriptionID: ccbillStored, StartedAt: start, PaidThrough: &paid,
					AcceptedSchedule: &billing.DeclaredProviderSchedule{SourceReference: ccbillStored, Amount: 10_000_000, Currency: "USD", IntervalHours: 720, FirstDueAt: paid}},
			},
			Transactions: []billing.DeclaredTransaction{
				{RailSubscriptionID: ccbillStored, TransactionID: "0812233445566778801", Type: "sale", Success: true, Amount: 10_000_000, Currency: "USD", OccurredAt: start},
				{RailSubscriptionID: ccbillStored, TransactionID: "0912233445566778801", Type: "sale", Success: true, Amount: 10_000_000, Currency: "USD", OccurredAt: rebill},
			}})
		require.NoError(t, err)
		require.Equal(t, []string{"kept"}, res.Imported, "%v", res.Reasons)
		psps, err := w.alpha.ListPSPs(t.Context(), billing.PSPListParams{})
		require.NoError(t, err)
		require.Len(t, psps.Items, 1)
		// Canceled at the end of its paid period, which is ahead.
		ghostCCBillMembership(t, w, gone, psps.Items[0].ID.UUID(), "223344556677889900", start, paid, rebill.Add(24*time.Hour))
		pruned := onlySubscription(t, w.alpha, gone)
		require.Equal(t, billing.SubscriptionCanceled, pruned.Status)
		require.Equal(t, []string{"churn scheduled"}, mrrEntries(t, w, w.alpha, pruned.ID))
		require.Equal(t, [3]int64{2 * ccbillMemberMRR, 2, 2}, mrrTotals(t, w, w.alpha, "USD"))

		// CCBill's roster lists only the kept member: a prune removes the other.
		database, err := db.NewWithPGXPool(slog.Default(), w.pool, w.schema)
		require.NoError(t, err)
		dl := ccbill.NewDataLinkClient(slog.Default(), &config.CCBillConfig{ClientAccNum: "945280", ClientSubAcc: "0001", Sandbox: true,
			DataLinkUsername: "critical-datalink", DataLinkPassword: "critical-datalink-password"})
		dl.BaseURL = feed.URL
		binding := reconcile.PSPBinding{ID: psps.Items[0].ID.UUID(), Rail: "ccbill", AccountID: "945280-0001"}
		one := 1
		var run reconcile.PruneResult
		// As openrails prune runs it.
		require.NoError(t, database.RunInMerchantConn(merchant.WithID(t.Context(), w.alpha.MerchantID()), func(ctx context.Context) error {
			run, err = reconcile.PrunePSPExcess(ctx, database, completeRoster{reconcile.NewCCBillFetcher(dl)}, reconcile.ProviderCCBill, binding,
				reconcile.PruneParams{Since: rebill.Add(-24 * time.Hour), Until: w.clock.Now(), Apply: true, ExpectedRows: &one, Actor: "critical"})
			return err
		}))
		require.Equal(t, []uuid.UUID{pruned.ID.UUID()}, run.SubscriptionIDs)
		moves := mrrMoves(t, w, w.alpha, gone)
		require.Equal(t, []string{mrrKind("USD", "opening", ccbillMemberMRR), mrrKind("USD", "correction", -ccbillMemberMRR)}, mrrKinds(moves), "a prune is no churn")
		require.Equal(t, "prune", moves[1].Cause)
		require.Equal(t, -1, moves[1].Logo)
		require.Nil(t, moves[1].Voluntary)
		require.Equal(t, []string{"churn superseded"}, mrrEntries(t, w, w.alpha, pruned.ID), "the churn it scheduled never happens")
		require.Equal(t, [3]int64{ccbillMemberMRR, 1, 1}, mrrTotals(t, w, w.alpha, "USD"))

		// Undoing the prune restores the row: a correction back, and its
		// churn scheduled again.
		undo := func(args ...string) string {
			return hostCLI(t, w, append([]string{"undo-run", "--merchant", "id:" + w.alpha.MerchantID().String(), "--run", run.RunID.String()}, args...)...)
		}
		rows := regexp.MustCompile(`--expect-rows (\d+)`).FindStringSubmatch(undo())
		require.Len(t, rows, 2)
		undo("--apply", "--expect-rows", rows[1])
		moves = mrrMoves(t, w, w.alpha, gone)
		require.Equal(t, []string{mrrKind("USD", "opening", ccbillMemberMRR), mrrKind("USD", "correction", -ccbillMemberMRR), mrrKind("USD", "correction", ccbillMemberMRR)},
			mrrKinds(moves))
		require.Equal(t, "undo", moves[2].Cause)
		require.Equal(t, 1, moves[2].Logo)
		require.Equal(t, []string{"churn superseded", "churn scheduled"}, mrrEntries(t, w, w.alpha, pruned.ID))
		require.Equal(t, [3]int64{2 * ccbillMemberMRR, 2, 2}, mrrTotals(t, w, w.alpha, "USD"))
		require.Equal(t, map[string]int64{"opening": 2, "correction": 2}, mrrFlows(t, w, w.alpha, "USD"))
	})

	t.Run("OR-J42-S09/a-bulk-import-settles-once-its-reads-growing-with-the-book", func(t *testing.T) {
		a := ccbillWebhookWorld(t)
		price := ccbillMemberPrice(t, a.alpha)
		// The book's request body stays under the 1 MiB cap: some 700 bytes a member.
		const members = 1200
		before, scans := subscriptionMRRReads(t, a.world, a.pool, false), subscriptionMRRScans(t, a.world, a.pool, false)
		res, err := a.alpha.ImportBilling(t.Context(), ccbillBook(price, a.clock.Now().UTC().Truncate(time.Second), 0, members))
		require.NoError(t, err)
		require.Len(t, res.Imported, members, "%v", res.Reasons)

		require.Equal(t, [3]int64{members * ccbillMemberMRR, members, members}, mrrTotals(t, a.world, a.alpha, "USD"))
		require.Equal(t, map[string]int64{"opening": members}, mrrFlows(t, a.world, a.alpha, "USD"), "one opening per customer")
		// The commit settles once: every later row event probes its own row.
		// The importing session reports its reads once idle.
		var reads int64
		require.Eventually(t, func() bool {
			reads = subscriptionMRRReads(t, a.world, a.pool, false) - before
			return reads >= members
		}, 70*time.Second, 250*time.Millisecond, "the import's reads reach the statistics")
		// Each of a row's writes queues an event that probes the row's
		// versions, and each customer's settle reads its lines a few times:
		// some tens of reads a member. Reading every earlier row for each
		// would be a thousand times as many.
		require.Less(t, reads, int64(100*members), "reads of subscription_mrr grow with the book, not its square: before %s; after %s",
			scans, subscriptionMRRScans(t, a.world, a.pool, false))
	})
}
