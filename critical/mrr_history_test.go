//go:build integration

package critical_test

import (
	"context"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"testing"
	"testing/fstest"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/migrate"
	postgresmigrations "github.com/open-rails/openrails/internal/migrate/postgres"
	"github.com/open-rails/openrails/internal/nmimock"
	"github.com/open-rails/openrails/internal/sqlschema"
)

// An import brings the merchant's history: what it lands adds MRR from each
// subscription's start to its end, at their own dates, on the customer's one
// timeline; nothing is booked at the import.
func TestMRRHistory(t *testing.T) {
	day := 24 * time.Hour
	at := func(moves []mrrMove) []string {
		out := make([]string, len(moves))
		for i, m := range moves {
			out[i] = mrrKind(m.Currency, m.Kind, m.Delta) + " " + m.OccurredAt.UTC().Format(time.RFC3339)
		}
		return out
	}
	stamp := func(t time.Time) string { return t.UTC().Format(time.RFC3339) }

	t.Run("OR-J42-S38/an-imported-subscription-is-new-at-its-start-and-churns-at-its-end", func(t *testing.T) {
		iw := bootImportWorld(t, "ccbill")
		price := ccbillMemberPrice(t, iw.alpha)
		now := iw.clock.Now().UTC().Truncate(time.Second)
		t0 := now.Add(-730 * day)
		canceled, end, liveStart, paid := t0.Add(350*day), t0.Add(365*day), t0.Add(180*day), now.Add(5*day)
		ended, live := billing.CustomerID(uuid.New()), billing.CustomerID(uuid.New())
		res, err := iw.alpha.ImportBilling(t.Context(), billing.DeclaredBilling{AsOf: now, DefaultPSP: billing.PSPRef{Key: "ccbill"},
			Customers: []billing.DeclaredCustomer{{Customer: ended}, {Customer: live}},
			Subscriptions: []billing.DeclaredSubscription{
				{SourceID: "ended", Customer: ended, Price: price, Rail: "ccbill", RailSubscriptionID: "910000000000000001", StartedAt: t0, PaidThrough: &end,
					Cancel: billing.CancelEvidence{Kind: "user_canceled", At: canceled}},
				{SourceID: "live", Customer: live, Price: price, Rail: "ccbill", RailSubscriptionID: "910000000000000002", StartedAt: liveStart, PaidThrough: &paid,
					AcceptedSchedule: &billing.DeclaredProviderSchedule{SourceReference: "live", Amount: 10_000_000, Currency: "USD", IntervalHours: 720, FirstDueAt: paid}},
			}})
		require.NoError(t, err)
		require.ElementsMatch(t, []string{"ended", "live"}, res.Imported, "%v", res.Reasons)

		moves := mrrMoves(t, iw.world, iw.alpha, ended)
		require.Equal(t, []string{mrrKind("USD", "new", ccbillMemberMRR) + " " + stamp(t0), mrrKind("USD", "churn", -ccbillMemberMRR) + " " + stamp(end)}, at(moves),
			"new when it started two years ago, churned at the end of what it paid for")
		require.Equal(t, []int{1, -1}, []int{moves[0].Logo, moves[1].Logo})
		require.NotNil(t, moves[1].Voluntary)
		require.True(t, *moves[1].Voluntary, "the customer canceled")
		require.Equal(t, "canceled", moves[1].Cause)
		require.False(t, moves[0].Scheduled || moves[1].Scheduled)
		require.Equal(t, []string{mrrKind("USD", "new", ccbillMemberMRR) + " " + stamp(liveStart)}, at(mrrMoves(t, iw.world, iw.alpha, live)),
			"a live subscription is new when it started; nothing at the import")
		var cohorts []time.Time
		rows, err := iw.pool.Query(t.Context(), "SELECT DISTINCT cohort_at FROM "+pgx.Identifier{iw.schema, "mrr_movements"}.Sanitize()+
			" WHERE merchant_id = $1 AND customer_id = ANY($2) ORDER BY 1", iw.alpha.MerchantID().UUID(), []uuid.UUID{ended.UUID(), live.UUID()})
		require.NoError(t, err)
		for rows.Next() {
			var c time.Time
			require.NoError(t, rows.Scan(&c))
			cohorts = append(cohorts, c.UTC())
		}
		require.NoError(t, rows.Err())
		require.Equal(t, []time.Time{t0, liveStart}, cohorts, "each customer's cohort is its first start")
		require.Equal(t, [3]int64{ccbillMemberMRR, 1, 1}, mrrTotals(t, iw.world, iw.alpha, "USD", settledBy(iw.world)))
		require.Equal(t, [3]int64{2 * ccbillMemberMRR, 2, 2}, mrrTotals(t, iw.world, iw.alpha, "USD", t0.Add(200*day)), "a year and a half ago both added MRR")
	})

	t.Run("OR-J42-S39/a-customer-imported-from-two-rails-in-two-passes-reads-as-one-timeline", func(t *testing.T) {
		iw := bootImportWorld(t, "nmi", "ccbill")
		iw.nmi.AddPlan(nmimock.Plan{ID: "gold", Name: "Gold", Amount: "20.00", Days: 30})
		_, err := iw.alpha.CreateProduct(t.Context(), billing.CreateProductParams{Key: "gold", DisplayName: "Gold", Entitlements: []string{"member:gold"}})
		require.NoError(t, err)
		gold, err := iw.alpha.CreateProviderPrice(t.Context(), billing.CreateProviderPriceParams{ProductKey: "gold", Key: "nmi-gold", PSP: "nmi", PlanID: "gold"})
		require.NoError(t, err)
		member := ccbillMemberPrice(t, iw.alpha)
		now := iw.clock.Now().UTC().Truncate(time.Second)
		t0 := now.Add(-600 * day)
		customer := billing.CustomerID(uuid.New())
		stint := func(source, rail, ref string, price billing.PriceID, from, to time.Duration) billing.DeclaredSubscription {
			start, end := t0.Add(from), t0.Add(to)
			return billing.DeclaredSubscription{SourceID: source, Customer: customer, Price: price, Rail: billing.Rail(rail), RailSubscriptionID: ref, StartedAt: start,
				PaidThrough: &end, Cancel: billing.CancelEvidence{Kind: "user_canceled", At: end}}
		}
		pass := func(psp string, sub billing.DeclaredSubscription) {
			t.Helper()
			res, err := iw.alpha.ImportBilling(t.Context(), billing.DeclaredBilling{AsOf: now, DefaultPSP: billing.PSPRef{Key: psp},
				Customers: []billing.DeclaredCustomer{{Customer: customer}}, Subscriptions: []billing.DeclaredSubscription{sub}})
			require.NoError(t, err)
			require.Equal(t, []string{sub.SourceID}, res.Imported, "%v", res.Reasons)
		}
		pass("nmi", stint("nmi-gold", "nmi", "4100000001", gold.ID, 0, 300*day))
		require.Equal(t, []string{mrrKind("USD", "new", 20_000_000) + " " + stamp(t0), mrrKind("USD", "churn", -20_000_000) + " " + stamp(t0.Add(300*day))},
			at(mrrMoves(t, iw.world, iw.alpha, customer)))

		// The other rail's membership overlaps it: its end is no longer churn.
		pass("ccbill", stint("ccbill-member", "ccbill", "910000000000000003", member, 100*day, 400*day))
		moves := mrrMoves(t, iw.world, iw.alpha, customer)
		require.Equal(t, []string{
			mrrKind("USD", "new", 20_000_000) + " " + stamp(t0),
			mrrKind("USD", "expansion", ccbillMemberMRR) + " " + stamp(t0.Add(100*day)),
			mrrKind("USD", "contraction", -20_000_000) + " " + stamp(t0.Add(300*day)),
			mrrKind("USD", "churn", -ccbillMemberMRR) + " " + stamp(t0.Add(400*day)),
		}, at(moves), "one timeline across both rails")
		require.Equal(t, []int64{0, 20_000_000, 30_000_000, ccbillMemberMRR}, []int64{moves[0].Before, moves[1].Before, moves[2].Before, moves[3].Before})
		require.Equal(t, []int{1, 0, 0, -1}, []int{moves[0].Logo, moves[1].Logo, moves[2].Logo, moves[3].Logo})
		require.Equal(t, []int{1, 1, 1, 1}, []int{moves[0].Lines, moves[1].Lines, moves[2].Lines, moves[3].Lines})
	})

	t.Run("OR-J42-S40/churn-nrr-and-cohort-retention-over-imported-months-equal-the-same-history-booked-live", func(t *testing.T) {
		iw := bootImportWorld(t, "ccbill")
		now := iw.clock.Now().UTC().Truncate(time.Second)
		t0 := now.Add(-700 * day).Truncate(day).Add(10 * time.Hour)
		type stint struct {
			customer int
			product  string
			from, to time.Duration // to 0: still live
			kind     string        // user_canceled or provider_terminated
		}
		history := []stint{
			{1, "a", 0, 200 * day, "user_canceled"},
			{2, "a", 30 * day, 0, ""},
			{3, "a", 60 * day, 150 * day, "provider_terminated"},
			{3, "a", 300 * day, 500 * day, "user_canceled"},
			{4, "a", 90 * day, 0, ""},
			{4, "b", 240 * day, 400 * day, "user_canceled"},
			{5, "b", 400 * day, 600 * day, "provider_terminated"},
			{6, "a", 450 * day, 0, ""},
			{7, "b", 33 * day, 640 * day, "user_canceled"},
		}
		amounts := map[string]int64{"a": 10_000_000, "b": 25_000_000}

		// Alpha imports it.
		alphaPrices := map[string]billing.PriceID{}
		for _, key := range []string{"a", "b"} {
			_, err := iw.alpha.CreateProduct(t.Context(), billing.CreateProductParams{Key: "plan-" + key, DisplayName: key, Entitlements: []string{"plan:" + key}})
			require.NoError(t, err)
			p, err := iw.alpha.CreateProviderPrice(t.Context(), billing.CreateProviderPriceParams{ProductKey: "plan-" + key, Key: "ccbill-" + key, PSP: "ccbill",
				FlexID: "critical-flex-" + key, Currency: "USD", UnitAmount: ptr(amounts[key]), BillingIntervalHours: ptr(720)})
			require.NoError(t, err)
			alphaPrices[key] = p.ID
		}
		alphaCustomers := map[int]billing.CustomerID{}
		book := billing.DeclaredBilling{AsOf: now, DefaultPSP: billing.PSPRef{Key: "ccbill"}}
		for i, s := range history {
			if _, ok := alphaCustomers[s.customer]; !ok {
				alphaCustomers[s.customer] = billing.CustomerID(uuid.New())
				book.Customers = append(book.Customers, billing.DeclaredCustomer{Customer: alphaCustomers[s.customer]})
			}
			sub := billing.DeclaredSubscription{SourceID: fmt.Sprintf("stint-%d", i), Customer: alphaCustomers[s.customer], Price: alphaPrices[s.product], Rail: "ccbill",
				RailSubscriptionID: fmt.Sprintf("92%016d", i+1), StartedAt: t0.Add(s.from)}
			if s.to == 0 {
				paid := now.Add(10 * day)
				sub.PaidThrough = &paid
				sub.AcceptedSchedule = &billing.DeclaredProviderSchedule{SourceReference: sub.SourceID, Amount: amounts[s.product], Currency: "USD", IntervalHours: 720, FirstDueAt: paid}
			} else {
				end := t0.Add(s.to)
				sub.PaidThrough, sub.Cancel = &end, billing.CancelEvidence{Kind: s.kind, At: end}
			}
			book.Subscriptions = append(book.Subscriptions, sub)
		}
		res, err := iw.alpha.ImportBilling(t.Context(), book)
		require.NoError(t, err)
		require.Len(t, res.Imported, len(history), "%v", res.Reasons)

		// Beta books the same history live: each change one transaction as it
		// happens, through the schema's own triggers.
		table := func(name string) string { return pgx.Identifier{iw.schema, name}.Sanitize() }
		beta := iw.beta.MerchantID().UUID()
		var psp uuid.UUID
		require.NoError(t, iw.pool.QueryRow(t.Context(), "INSERT INTO "+table("psps")+" (merchant_id, key, rail, environment, account_id) VALUES ($1, 'live', 'stripe', 'test', $2) RETURNING id",
			beta, "acct_"+uuid.NewString()[:8]).Scan(&psp))
		betaPrices, betaProducts := map[string]uuid.UUID{}, map[string]uuid.UUID{}
		for _, key := range []string{"a", "b"} {
			product, err := iw.beta.CreateProduct(t.Context(), billing.CreateProductParams{Key: "plan-" + key, DisplayName: key, Entitlements: []string{"plan:" + key}})
			require.NoError(t, err)
			_, err = iw.beta.CreatePrice(t.Context(), billing.CreatePriceParams{ProductID: product.ID, Key: "plan-" + key, UnitAmount: amounts[key], Currency: "USD", BillingIntervalHours: ptr(720)})
			require.NoError(t, err)
			betaProducts[key] = product.ID.UUID()
			var id uuid.UUID
			require.NoError(t, iw.pool.QueryRow(t.Context(), "SELECT id FROM "+table("prices")+" WHERE merchant_id = $1 AND product_id = $2", beta, product.ID.UUID()).Scan(&id))
			betaPrices[key] = id
		}
		database, err := producerDB(iw.pool, iw.schema)
		require.NoError(t, err)
		ctx := merchant.WithID(t.Context(), iw.beta.MerchantID())
		write := func(sql string, args ...any) {
			t.Helper()
			require.NoError(t, database.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
				_, err := tx.Exec(ctx, sql, args...)
				return err
			}), sql)
		}
		betaCustomers, cards := map[int]uuid.UUID{}, map[int]uuid.UUID{}
		type event struct {
			at     time.Time
			stint  int
			ending bool
		}
		var events []event
		for i, s := range history {
			if _, ok := betaCustomers[s.customer]; !ok {
				betaCustomers[s.customer], cards[s.customer] = uuid.New(), uuid.New()
				start := t0.Add(s.from)
				write("INSERT INTO billing.customers (id, merchant_id, created_at, last_seen_at) VALUES ($1, $2, $3, $3)", betaCustomers[s.customer], beta, start)
				write(`INSERT INTO billing.payment_methods (id, merchant_id, customer_id, rail, psp_id, custodian, card_brand, card_last4, card_exp_month, card_exp_year,
					charge_via, status, created_at, updated_at) VALUES ($1, $2, $3, 'stripe', $4, 'psp', 'visa', '4242', 12, 2040, 'pan_proxy', 'active', $5, $5)`,
					cards[s.customer], beta, betaCustomers[s.customer], psp, start)
			}
			events = append(events, event{at: t0.Add(s.from), stint: i})
			if s.to != 0 {
				events = append(events, event{at: t0.Add(s.to), stint: i, ending: true})
			}
		}
		for i := range events {
			for j := i + 1; j < len(events); j++ {
				if events[j].at.Before(events[i].at) {
					events[i], events[j] = events[j], events[i]
				}
			}
		}
		subs := map[int]uuid.UUID{}
		for _, e := range events {
			s := history[e.stint]
			if !e.ending {
				subs[e.stint] = uuid.New()
				write(`INSERT INTO billing.subscriptions (id, merchant_id, customer_id, price_id, product_id, status, rail, collection_policy, payment_method_id,
					current_period_starts_at, current_period_ends_at, started_at, psp_id, created_at, updated_at)
					VALUES ($1, $2, $3, $4, $5, 'active', 'stripe', 'engine', $6, $7, $8, $7, $9, $7, $7)`,
					subs[e.stint], beta, betaCustomers[s.customer], betaPrices[s.product], betaProducts[s.product], cards[s.customer], e.at, e.at.Add(30*day), psp)
				continue
			}
			cancelType := "user"
			if s.kind == "provider_terminated" {
				cancelType = "provider"
			}
			write(`UPDATE billing.subscriptions SET status = 'canceled', cancel_type = $3, canceled_at = $4, ended_at = $4, lifecycle_rev = lifecycle_rev + 1, updated_at = $4
				WHERE merchant_id = $1 AND id = $2`, beta, subs[e.stint], cancelType, e.at)
		}

		// Every measure of the months, and every cohort, read the same.
		months := &billing.MetricsRange{From: t0.Format(time.DateOnly), To: now.Format(time.DateOnly)}
		for _, q := range []billing.QueryMetricsParams{
			{Measures: []string{"mrr", "subscribers", "subscriber_churn_rate", "nrr", "grr"}, By: []string{"time"}, Grain: "month", Range: months},
			{Measures: []string{"mrr_movements", "subscriber_movements"}, By: []string{"time", "movement"}, Grain: "month", Range: months},
			{Measures: []string{"subscriber_churn_rate"}, By: []string{"time", "churn_type"}, Grain: "month", Range: months},
			{Measures: []string{"cohort_retention"}, By: []string{"cohort", "months_since_start", "basis"}, Range: months},
		} {
			imported, booked := metricTable(t, iw.alpha, q), metricTable(t, iw.beta, q)
			require.NotEmpty(t, imported)
			require.Equal(t, booked, imported, "%v by %v", q.Measures, q.By)
		}
	})

	t.Run("OR-J42-S43/an-ended-subscription-stops-adding-mrr-at-its-paid-through-end", func(t *testing.T) {
		iw := bootImportWorld(t, "ccbill")
		price := ccbillMemberPrice(t, iw.alpha)
		now := iw.clock.Now().UTC().Truncate(time.Second)
		t0 := now.Add(-2400 * day)
		cleanup := now.Add(-290 * day)
		// Lapsed years before a cleanup marked it canceled; charged back
		// before the end of what it paid for; paid for nothing, canceled later.
		lapsed, disputed, unpaid := billing.CustomerID(uuid.New()), billing.CustomerID(uuid.New()), billing.CustomerID(uuid.New())
		lapsedEnd, disputedStart := t0.Add(30*day), t0.Add(400*day)
		disputedAt, disputedEnd := disputedStart.Add(10*day), disputedStart.Add(30*day)
		res, err := iw.alpha.ImportBilling(t.Context(), billing.DeclaredBilling{AsOf: now, DefaultPSP: billing.PSPRef{Key: "ccbill"},
			Customers: []billing.DeclaredCustomer{{Customer: lapsed}, {Customer: disputed}, {Customer: unpaid}},
			Subscriptions: []billing.DeclaredSubscription{
				{SourceID: "lapsed", Customer: lapsed, Price: price, Rail: "ccbill", RailSubscriptionID: "910000000000000431", StartedAt: t0, PaidThrough: &lapsedEnd,
					Cancel: billing.CancelEvidence{Kind: "user_canceled", At: cleanup}},
				{SourceID: "disputed", Customer: disputed, Price: price, Rail: "ccbill", RailSubscriptionID: "910000000000000432", StartedAt: disputedStart, PaidThrough: &disputedEnd,
					Cancel: billing.CancelEvidence{Kind: "chargeback", At: disputedAt}},
				{SourceID: "unpaid", Customer: unpaid, Price: price, Rail: "ccbill", RailSubscriptionID: "910000000000000433", StartedAt: t0, PaidThrough: &t0,
					Cancel: billing.CancelEvidence{Kind: "user_canceled", At: cleanup}},
			}})
		require.NoError(t, err)
		require.ElementsMatch(t, []string{"lapsed", "disputed", "unpaid"}, res.Imported, "%v", res.Reasons)

		moves := mrrMoves(t, iw.world, iw.alpha, lapsed)
		require.Equal(t, []string{mrrKind("USD", "new", ccbillMemberMRR) + " " + stamp(t0), mrrKind("USD", "churn", -ccbillMemberMRR) + " " + stamp(lapsedEnd)}, at(moves),
			"a cancel recorded after its service lapsed churns where its service ended")
		require.True(t, *moves[1].Voluntary, "the customer canceled")
		moves = mrrMoves(t, iw.world, iw.alpha, disputed)
		require.Equal(t, []string{mrrKind("USD", "new", ccbillMemberMRR) + " " + stamp(disputedStart), mrrKind("USD", "churn", -ccbillMemberMRR) + " " + stamp(disputedAt)}, at(moves),
			"a chargeback ends it at once")
		require.False(t, *moves[1].Voluntary)
		require.Equal(t, "chargeback", moves[1].Cause)
		require.Empty(t, mrrMoves(t, iw.world, iw.alpha, unpaid), "paid through no later than its start: it added nothing")
	})
}

// An upgrade turns each opening an import wrote into its subscriptions'
// history; live movements after it stand.
func TestMRRHistoryUpgrade(t *testing.T) {
	t.Run("OR-J42-S41/an-upgrade-turns-openings-into-history-and-keeps-live-movements", func(t *testing.T) {
		ctx := t.Context()
		pool, schema, logger := beforeImportHistory(t)
		table := func(name string) string { return pgx.Identifier{schema, name}.Sanitize() }
		exec := func(sql string, args ...any) {
			t.Helper()
			_, err := pool.Exec(ctx, sql, args...)
			require.NoError(t, err, sql)
		}
		m, psp, product, price := uuid.New(), uuid.New(), uuid.New(), uuid.New()
		exec("INSERT INTO "+table("merchants")+" (id, slug, status) VALUES ($1, 'upgrade-1423', 'active')", m)
		exec("INSERT INTO "+table("psps")+" (id, merchant_id, key, rail, environment, account_id) VALUES ($1, $2, 'ccbill', 'ccbill', 'test', '999999-0000')", psp, m)
		exec("INSERT INTO "+table("products")+" (id, merchant_id, key, display_name, tier_rank, archived) VALUES ($1, $2, 'member', 'Member', 0, false)", product, m)
		exec("INSERT INTO "+table("prices")+" (id, merchant_id, product_id, key, amount, currency, archived, billing_interval_hours) VALUES ($1, $2, $3, 'member', 10000000, 'USD', false, 720)",
			price, m, product)
		date := func(month time.Month, day int) time.Time { return time.Date(2025, month, day, 10, 0, 0, 0, time.UTC) }
		live, ended, lapsed := uuid.New(), uuid.New(), uuid.New()
		liveSub, endedSub, lapsedSub := uuid.New(), uuid.New(), uuid.New()
		for _, c := range []uuid.UUID{live, ended, lapsed} {
			exec("INSERT INTO "+table("customers")+" (id, merchant_id, created_at, last_seen_at) VALUES ($1, $2, $3, $3)", c, m, date(1, 1).AddDate(-6, 0, 0))
		}
		insert := "INSERT INTO " + table("subscriptions") + ` (id, merchant_id, customer_id, product_id, price_id, status, rail, collection_policy, rail_subscription_id, psp_id,
			started_at, current_period_starts_at, current_period_ends_at, canceled_at, ended_at, cancel_type, cancel_feedback, created_at, updated_at)
			VALUES ($1, $2, $3, $4, $5, $6, 'ccbill', 'provider', $7, $8, $9, $10, $11, $12, $13, $14, $15, $9, $16)`
		// The import as the build before wrote it: a live membership opened at
		// the import, and one already ended, which opened nothing.
		asOf := date(9, 1)
		tx, err := pool.Begin(ctx)
		require.NoError(t, err)
		_, err = tx.Exec(ctx, "SELECT set_config('openrails.mrr_cause', 'import', true)")
		require.NoError(t, err)
		_, err = tx.Exec(ctx, insert, liveSub, m, live, product, price, "active", "930000000000000001", psp, date(2, 10), date(8, 20), date(9, 19), nil, nil, nil, nil, asOf)
		require.NoError(t, err)
		_, err = tx.Exec(ctx, insert, endedSub, m, ended, product, price, "canceled", "930000000000000002", psp, date(1, 15), date(4, 15), date(5, 15), date(5, 15), date(5, 15),
			"user", "imported: declared user_canceled", date(5, 15))
		require.NoError(t, err)
		// Paid through March 2019; a cleanup marked it canceled in February 2025.
		lapsedStart, lapsedEnd, cleanup := date(2, 1).AddDate(-6, 0, 0), date(3, 1).AddDate(-6, 0, 0), date(2, 6)
		_, err = tx.Exec(ctx, insert, lapsedSub, m, lapsed, product, price, "canceled", "930000000000000003", psp, lapsedStart, lapsedStart, lapsedEnd, cleanup, cleanup,
			"user", "imported: declared user_canceled", cleanup)
		require.NoError(t, err)
		require.NoError(t, tx.Commit(ctx))
		// Then live: the customer cancels.
		exec("UPDATE "+table("subscriptions")+` SET status = 'canceled', cancel_type = 'user', canceled_at = $3, ended_at = $3, lifecycle_rev = lifecycle_rev + 1, updated_at = $3
			WHERE merchant_id = $1 AND id = $2`, m, liveSub, date(10, 1))
		kinds := func(customer uuid.UUID) []string {
			t.Helper()
			rows, err := pool.Query(ctx, "SELECT kind, delta, occurred_at FROM "+table("mrr_movements")+" WHERE merchant_id = $1 AND customer_id = $2 ORDER BY occurred_at, id", m, customer)
			require.NoError(t, err)
			defer rows.Close()
			var out []string
			for rows.Next() {
				var kind string
				var delta int64
				var when time.Time
				require.NoError(t, rows.Scan(&kind, &delta, &when))
				out = append(out, mrrKind("USD", kind, delta)+" "+when.UTC().Format(time.RFC3339))
			}
			require.NoError(t, rows.Err())
			return out
		}
		stamp := func(t time.Time) string { return t.UTC().Format(time.RFC3339) }
		require.Equal(t, []string{mrrKind("USD", "opening", 10_000_000) + " " + stamp(asOf), mrrKind("USD", "churn", -10_000_000) + " " + stamp(date(10, 1))}, kinds(live),
			"before: an opening at the import")
		require.Empty(t, kinds(ended), "before: the ended membership had no history")

		require.NoError(t, migrate.Apply(ctx, pool, migrate.Options{Schema: schema, Logger: logger}), "an upgrade over openings")

		require.Equal(t, []string{mrrKind("USD", "new", 10_000_000) + " " + stamp(date(2, 10)), mrrKind("USD", "churn", -10_000_000) + " " + stamp(date(10, 1))}, kinds(live),
			"the opening is its history from its start; the live churn stands")
		require.Equal(t, []string{mrrKind("USD", "new", 10_000_000) + " " + stamp(date(1, 15)), mrrKind("USD", "churn", -10_000_000) + " " + stamp(date(5, 15))}, kinds(ended),
			"the ended membership gains its history")
		require.Equal(t, []string{mrrKind("USD", "new", 10_000_000) + " " + stamp(lapsedStart), mrrKind("USD", "churn", -10_000_000) + " " + stamp(lapsedEnd)}, kinds(lapsed),
			"a cancel recorded after its service lapsed churns where its service ended")
		var openings, importCauses int
		require.NoError(t, pool.QueryRow(ctx, "SELECT count(*) FILTER (WHERE kind = 'opening'), count(*) FILTER (WHERE cause = 'import') FROM "+table("mrr_movements")+" WHERE merchant_id = $1", m).
			Scan(&openings, &importCauses))
		require.Zero(t, openings+importCauses)
		_, err = pool.Exec(ctx, "DELETE FROM "+table("mrr_movements")+" WHERE merchant_id = $1", m)
		require.Error(t, err, "history stays immutable")
	})

	t.Run("OR-J42-S42/an-upgrade-turns-thousands-of-openings-into-history-without-a-lock-per-customer", func(t *testing.T) {
		ctx := t.Context()
		pool, schema, logger := beforeImportHistory(t)
		table := func(name string) string { return pgx.Identifier{schema, name}.Sanitize() }
		exec := func(sql string, args ...any) {
			t.Helper()
			_, err := pool.Exec(ctx, sql, args...)
			require.NoError(t, err, sql)
		}
		m, psp, product, price := uuid.New(), uuid.New(), uuid.New(), uuid.New()
		exec("INSERT INTO "+table("merchants")+" (id, slug, status) VALUES ($1, 'upgrade-1423-books', 'active')", m)
		exec("INSERT INTO "+table("psps")+" (id, merchant_id, key, rail, environment, account_id) VALUES ($1, $2, 'ccbill', 'ccbill', 'test', '999999-0000')", psp, m)
		exec("INSERT INTO "+table("products")+" (id, merchant_id, key, display_name, tier_rank, archived) VALUES ($1, $2, 'member', 'Member', 0, false)", product, m)
		exec("INSERT INTO "+table("prices")+" (id, merchant_id, product_id, key, amount, currency, archived, billing_interval_hours) VALUES ($1, $2, $3, 'member', 10000000, 'USD', false, 720)",
			price, m, product)
		// More customers than PostgreSQL's default lock table holds (64 per
		// connection, 100 connections), landed as the build before did: books
		// of a thousand, each opening its live memberships at the import.
		// Every seventh had already ended and opened nothing.
		const customers, book = 7000, 1000
		start, asOf := time.Date(2024, 1, 1, 10, 0, 0, 0, time.UTC), time.Date(2025, 9, 1, 10, 0, 0, 0, time.UTC)
		for first := 0; first < customers; first += book {
			tx, err := pool.Begin(ctx)
			require.NoError(t, err)
			for _, q := range []struct {
				sql  string
				args []any
			}{
				{"SELECT set_config('openrails.mrr_cause', 'import', true)", nil},
				{"INSERT INTO " + table("customers") + ` (id, merchant_id, created_at, last_seen_at)
					SELECT md5('customer' || g)::uuid, $1, $2, $2 FROM generate_series($3::int, $4::int) g`, []any{m, start, first, first + book - 1}},
				{"INSERT INTO " + table("subscriptions") + ` (id, merchant_id, customer_id, product_id, price_id, status, rail, collection_policy, rail_subscription_id, psp_id,
					started_at, current_period_starts_at, current_period_ends_at, canceled_at, ended_at, cancel_type, cancel_feedback, created_at, updated_at)
					SELECT md5('subscription' || g)::uuid, $1, md5('customer' || g)::uuid, $2, $3, CASE WHEN x.ended THEN 'canceled' ELSE 'active' END,
					       'ccbill', 'provider', '93' || lpad(g::text, 16, '0'), $4, x.started,
					       CASE WHEN x.ended THEN x.ends - interval '30 days' ELSE $5::timestamptz - interval '12 days' END,
					       CASE WHEN x.ended THEN x.ends ELSE $5::timestamptz + interval '18 days' END,
					       CASE WHEN x.ended THEN x.ends END, CASE WHEN x.ended THEN x.ends END, CASE WHEN x.ended THEN 'user' END,
					       CASE WHEN x.ended THEN 'imported: declared user_canceled' END, x.started, CASE WHEN x.ended THEN x.ends ELSE $5::timestamptz END
					  FROM generate_series($6::int, $7::int) g
					 CROSS JOIN LATERAL (SELECT g % 7 = 0 AS ended, $8::timestamptz + make_interval(days => g % 500) AS started,
					                            $8::timestamptz + make_interval(days => g % 500 + 60) AS ends) x`,
					[]any{m, product, price, psp, asOf, first, first + book - 1, start}},
			} {
				_, err := tx.Exec(ctx, q.sql, q.args...)
				require.NoError(t, err, q.sql)
			}
			require.NoError(t, tx.Commit(ctx))
		}
		var openings int
		require.NoError(t, pool.QueryRow(ctx, "SELECT count(*) FROM "+table("mrr_movements")+" WHERE merchant_id = $1 AND kind = 'opening'", m).Scan(&openings))
		require.Equal(t, customers-customers/7, openings, "before: an opening per live membership")

		// The conversion, where the locks it holds can be counted: a lock per
		// customer exhausts the lock table of a large merchant's upgrade.
		content, err := fs.ReadFile(postgresmigrations.FS, "0016_import_history.up.sql")
		require.NoError(t, err)
		upgrade, err := sqlschema.Rewrite(string(content), schema)
		require.NoError(t, err)
		tx, err := pool.Begin(ctx)
		require.NoError(t, err)
		_, err = tx.Exec(ctx, upgrade)
		require.NoError(t, err)
		var locks int
		require.NoError(t, tx.QueryRow(ctx, "SELECT count(*) FROM pg_locks WHERE pid = pg_backend_pid() AND locktype = 'advisory'").Scan(&locks))
		require.NoError(t, tx.Rollback(ctx))
		require.LessOrEqual(t, locks, 1, "the conversion of %d customers holds no lock per customer", customers)

		began := time.Now()
		require.NoError(t, migrate.Apply(ctx, pool, migrate.Options{Schema: schema, Logger: logger}), "an upgrade over thousands of openings")
		t.Logf("the upgrade converted %d customers in %s", customers, time.Since(began).Round(time.Millisecond))
		var news, churns, total, imported int
		var mrr int64
		require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FILTER (WHERE v.kind = 'new' AND v.occurred_at = s.started_at),
			       count(*) FILTER (WHERE v.kind = 'churn' AND v.occurred_at = s.ended_at), count(*), count(*) FILTER (WHERE v.imported),
			       COALESCE(sum(v.delta) FILTER (WHERE v.occurred_at <= now()), 0)
			  FROM `+table("mrr_movements")+` v JOIN `+table("subscriptions")+` s ON s.merchant_id = v.merchant_id AND s.customer_id = v.customer_id
			 WHERE v.merchant_id = $1`, m).Scan(&news, &churns, &total, &imported, &mrr))
		require.Equal(t, customers, news, "each customer is new at its membership's start")
		require.Equal(t, customers/7, churns, "each ended membership churns at its end")
		require.Equal(t, customers+customers/7, total)
		require.Equal(t, total, imported)
		require.Equal(t, int64(customers-customers/7)*10_000_000, mrr, "the live memberships' MRR stands")
	})
}

// beforeImportHistory builds a schema at the migration before imports wrote
// history, for an upgrade to run over.
func beforeImportHistory(t *testing.T) (*pgxpool.Pool, string, *slog.Logger) {
	t.Helper()
	ctx := t.Context()
	dsn := os.Getenv("OPENRAILS_CRITICAL_DSN")
	require.NotEmpty(t, dsn, "OPENRAILS_CRITICAL_DSN must name a disposable PostgreSQL database; critical scenarios never skip")
	pool, err := pgxpool.New(ctx, dsn)
	require.NoError(t, err)
	schema := criticalSchema()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_, err := pool.Exec(ctx, "DROP SCHEMA IF EXISTS "+pgx.Identifier{schema}.Sanitize()+" CASCADE")
		require.NoError(t, err)
		pool.Close()
	})
	logger := slog.New(slog.DiscardHandler)
	earlier := fstest.MapFS{}
	names, err := fs.Glob(postgresmigrations.FS, "*.up.sql")
	require.NoError(t, err)
	for _, name := range names {
		if name >= "0016_" {
			continue
		}
		data, err := fs.ReadFile(postgresmigrations.FS, name)
		require.NoError(t, err)
		earlier[name] = &fstest.MapFile{Data: data}
	}
	require.NoError(t, migrate.Apply(ctx, pool, migrate.Options{Schema: schema, Chain: earlier, Logger: logger}))
	return pool, schema, logger
}
