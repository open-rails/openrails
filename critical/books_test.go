//go:build integration

package critical_test

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/catalog"
	"github.com/open-rails/openrails/internal/archivewire"
	"github.com/open-rails/openrails/internal/currency"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/merchantarchive/contract"
	"github.com/open-rails/openrails/internal/migrate"
	postgresmigrations "github.com/open-rails/openrails/internal/migrate/postgres"
	"github.com/open-rails/openrails/internal/modules/merchantconfig"
	"github.com/open-rails/openrails/internal/modules/money/books"
	"github.com/open-rails/openrails/internal/modules/money/ledger"
)

func TestBooks(t *testing.T) {
	t.Run("OR-J40-S01/every-account-type-has-one-book-and-class-and-the-general-ledger-holds-money", func(t *testing.T) {
		w := newWorld(t)
		mid := w.alpha.MerchantID().UUID()
		customer := billing.CustomerID(uuid.New())
		fundBooks(t, w, customer, "USD", 1000)
		c, psp := customer.UUID(), fixturePSP(t, w)

		// Every account type opens through the chart, in its book and class,
		// with its sign rules, scoped as the chart says.
		require.NoError(t, booksTx(t, w, func(ctx context.Context, q *gen.Queries) error {
			for _, a := range books.Chart() {
				var cust, p *uuid.UUID
				switch a.Scope {
				case books.Customer:
					cust = &c
				case books.PSP:
					p = &psp
				}
				if _, _, err := books.EnsureAccount(ctx, q, mid, a.Type, "USD", cust, p); err != nil {
					return err
				}
			}
			return nil
		}))
		type stored struct {
			book, class      string
			dmnec, cmned     bool
			customer, hasPSP bool
		}
		rows, err := w.pool.Query(t.Context(), "SELECT account_type, book, class, debits_must_not_exceed_credits, credits_must_not_exceed_debits, customer_id IS NOT NULL, psp_id IS NOT NULL FROM "+booksTable(w, "ledger_accounts")+" WHERE merchant_id=$1 AND currency='USD'", mid)
		require.NoError(t, err)
		accounts := map[string]stored{}
		for rows.Next() {
			var typ string
			var s stored
			require.NoError(t, rows.Scan(&typ, &s.book, &s.class, &s.dmnec, &s.cmned, &s.customer, &s.hasPSP))
			accounts[typ] = s
		}
		require.NoError(t, rows.Err())
		require.Len(t, accounts, len(books.Chart()))
		for _, a := range books.Chart() {
			require.Equal(t, stored{
				book: string(a.Book), class: string(a.Class),
				dmnec: a.DebitsMustNotExceedCredits, cmned: a.CreditsMustNotExceedDebits,
				customer: a.Scope == books.Customer, hasPSP: a.Scope == books.PSP,
			}, accounts[string(a.Type)], a.Type)
		}

		// The schema admits no other pairing of type, book, class or scope, no
		// general-ledger sign rule but the chart's and no general-ledger
		// currency outside the registry.
		insert := "INSERT INTO " + booksTable(w, "ledger_accounts") + " (merchant_id, book, class, account_type, customer_id, currency, debits_must_not_exceed_credits, credits_must_not_exceed_debits) VALUES ($1, $2, $3, $4, $5, $6, $7, false)"
		for _, bad := range []struct {
			book, class, typ string
			customer         *uuid.UUID
			currency         string
			floor            bool
			constraint       string
		}{
			{"general", "revenue", "units_redeemed", nil, "EUR", false, "ledger_accounts_type_check"},
			{"units", "issued", "units_redeemed", nil, "EUR", false, "ledger_accounts_type_check"},
			{"general", "asset", "customer_balance", nil, "EUR", false, "ledger_accounts_type_check"},
			{"units", "retired", "units_redeemed", &c, "EUR", false, "ledger_accounts_scope_check"},
			{"general", "asset", "cash_clearing", nil, "EUR", false, "ledger_accounts_scope_check"},
			{"general", "liability", "unapplied_cash", nil, "EUR", false, "ledger_accounts_general_sign_check"},
			{"general", "revenue", "usage_revenue", nil, "EUR", true, "ledger_accounts_general_sign_check"},
			{"general", "revenue", "usage_revenue", nil, "GOLD", false, "ledger_accounts_general_currency_check"},
		} {
			_, err := w.pool.Exec(t.Context(), insert, mid, bad.book, bad.class, bad.typ, bad.customer, bad.currency, bad.floor)
			require.ErrorContains(t, err, bad.constraint, bad)
		}
		// The general ledger's currencies are the registry's, in code and in
		// the schema.
		var currencies string
		require.NoError(t, w.pool.QueryRow(t.Context(), "SELECT pg_get_constraintdef(oid) FROM pg_constraint WHERE conname='ledger_accounts_general_currency_check' AND conrelid=$1::regclass", booksTable(w, "ledger_accounts")).Scan(&currencies))
		var registry, declared []string
		for _, u := range currency.List() {
			registry = append(registry, u.Code)
		}
		for _, m := range regexp.MustCompile(`'(\w+)'::text`).FindAllStringSubmatch(currencies, -1) {
			if m[1] != "units" {
				declared = append(declared, m[1])
			}
		}
		require.ElementsMatch(t, registry, declared, currencies)

		// Each transfer type belongs to one book: the units book's are the
		// ledger's, the general ledger's the chart's.
		var def string
		require.NoError(t, w.pool.QueryRow(t.Context(), "SELECT pg_get_constraintdef(oid) FROM pg_constraint WHERE conname='ledger_transfers_type_check' AND conrelid=$1::regclass", booksTable(w, "ledger_transfers")).Scan(&def))
		vocab := map[string][]string{}
		for _, m := range regexp.MustCompile(`book = '(\w+)'::text\) AND \(transfer_type = ANY \(ARRAY\[([^\]]*)\]`).FindAllStringSubmatch(def, -1) {
			for _, typ := range regexp.MustCompile(`'(\w+)'::text`).FindAllStringSubmatch(m[2], -1) {
				vocab[m[1]] = append(vocab[m[1]], typ[1])
			}
		}
		var units, general []string
		for _, typ := range ledger.AllTransferTypes {
			units = append(units, string(typ))
		}
		for _, typ := range books.GeneralTransferTypes {
			general = append(general, string(typ))
		}
		require.ElementsMatch(t, units, vocab["units"], def)
		require.ElementsMatch(t, general, vocab["general"], def)
		for _, typ := range units {
			require.NotContains(t, general, typ)
		}

		// The units book's writer opens no general-ledger account.
		require.ErrorContains(t, booksTx(t, w, func(ctx context.Context, q *gen.Queries) error {
			_, err := ledger.New(q, mid).EnsureSystemAccount(ctx, books.UsageRevenue, "USD")
			return err
		}), "not a units-book account")

		// The general ledger holds registry currencies only: a merchant's own
		// unit, an unknown code and a code not as the registry spells it are
		// refused before anything is written.
		accrual := func(currency, key string) books.Posting {
			return books.Posting{
				Debit: books.Ref{Type: books.UsageReceivable, Customer: &c}, Credit: books.Ref{Type: books.UsageRevenue},
				Amount: 2_500_000, Currency: currency, Type: books.UsageAccrued,
				Coord:       books.Coord{Operation: "usage_accrual", Source: "critical", SourceID: key},
				EffectiveAt: w.clock.Now(), Customer: &c,
			}
		}
		for _, currency := range []string{"GOLD", "XAU", "usd"} {
			err := booksTx(t, w, func(ctx context.Context, q *gen.Queries) error {
				_, _, err := books.New(q, mid).Post(ctx, accrual(currency, "refused"))
				return err
			})
			require.ErrorIs(t, err, books.ErrNotMoney, currency)
		}
		var posted, replay gen.BillingLedgerTransfer
		for i, into := range []*gen.BillingLedgerTransfer{&posted, &replay} {
			require.NoError(t, booksTx(t, w, func(ctx context.Context, q *gen.Queries) error {
				tr, applied, err := books.New(q, mid).Post(ctx, accrual("USD", "accepted"))
				require.Equal(t, i == 0, applied)
				*into = tr
				return err
			}))
		}
		require.Equal(t, posted.ID, replay.ID, "a replayed posting lands once")
		// A coordinate that posted one transfer refuses a different one, and a
		// posting for an order names its customer.
		order := uuid.New()
		for _, bad := range []struct {
			change func(*books.Posting)
			err    error
		}{
			{func(p *books.Posting) { p.Amount++ }, books.ErrCoordinateReused},
			{func(p *books.Posting) { p.EffectiveAt = p.EffectiveAt.Add(time.Minute) }, books.ErrCoordinateReused},
			{func(p *books.Posting) { p.Order = &order }, books.ErrCoordinateReused},
		} {
			err := booksTx(t, w, func(ctx context.Context, q *gen.Queries) error {
				p := accrual("USD", "accepted")
				bad.change(&p)
				_, _, err := books.New(q, mid).Post(ctx, p)
				return err
			})
			require.ErrorIs(t, err, bad.err)
		}
		require.ErrorContains(t, booksTx(t, w, func(ctx context.Context, q *gen.Queries) error {
			_, _, err := books.New(q, mid).Post(ctx, books.Posting{
				Debit: books.Ref{Type: books.RevenueAdjustments}, Credit: books.Ref{Type: books.DeferredRevenue},
				Amount: 100, Currency: "USD", Type: books.RevenueAdjusted, EffectiveAt: w.clock.Now(), Order: &order,
				Coord: books.Coord{Operation: "revenue_adjustment", Source: "critical", SourceID: "unattributed"},
			})
			return err
		}), "names its customer")
		require.Equal(t, "general", posted.Book)
		require.True(t, posted.EffectiveAt.Equal(w.clock.Now().Truncate(time.Microsecond)), "a posting takes effect at its own instant")
		var count int
		require.NoError(t, w.pool.QueryRow(t.Context(), "SELECT count(*) FROM "+booksTable(w, "ledger_transfers")+" WHERE merchant_id=$1 AND book='general'", mid).Scan(&count))
		require.Equal(t, 1, count)

		// The units book's readers never see a general-ledger transfer.
		page, err := w.alpha.ListBalanceTransactions(t.Context(), customer, billing.BalanceTransactionListParams{Currency: "USD"})
		require.NoError(t, err)
		require.Len(t, page.Items, 1)
		require.NotEqual(t, posted.ID, page.Items[0].ID.UUID())
		byID, err := w.alpha.ListBalanceTransactions(t.Context(), customer, billing.BalanceTransactionListParams{IDs: []billing.BalanceTransactionID{billing.BalanceTransactionID(posted.ID)}})
		require.NoError(t, err)
		require.Empty(t, byID.Items)
		balance(t, w.alpha, customer, 1000, 0, 1000)
		require.Contains(t, hostCLI(t, w, "ledger-audit", "--merchant", "id:"+w.alpha.MerchantID().String()), "Ledger integrity OK")
	})

	t.Run("OR-J40-S02/a-transfer-across-books-currencies-or-customers-is-refused", func(t *testing.T) {
		w := newWorld(t)
		mid := w.alpha.MerchantID().UUID()
		alice, bob := billing.CustomerID(uuid.New()), billing.CustomerID(uuid.New())
		fundBooks(t, w, alice, "USD", 1000)
		fundBooks(t, w, alice, "EUR", 1000)
		fundBooks(t, w, bob, "USD", 1000)
		a, b := alice.UUID(), bob.UUID()
		account := func(typ books.AccountType, currency string, customer *uuid.UUID) uuid.UUID {
			var id uuid.UUID
			require.NoError(t, booksTx(t, w, func(ctx context.Context, q *gen.Queries) error {
				var err error
				id, _, err = books.EnsureAccount(ctx, q, mid, typ, currency, customer, nil)
				return err
			}))
			return id
		}
		aliceUSD, aliceEUR, bobUSD := account(books.CustomerUnits, "USD", &a), account(books.CustomerUnits, "EUR", &a), account(books.CustomerUnits, "USD", &b)
		sold, receivable, revenue := account(books.UnitsSold, "USD", nil), account(books.UsageReceivable, "USD", &a), account(books.UsageRevenue, "USD", nil)
		before := booksSnapshot(t, w)

		// The database refuses each, whichever writer inserts it.
		insert := "INSERT INTO " + booksTable(w, "ledger_transfers") + " (merchant_id, book, debit_account_id, credit_account_id, amount, currency, transfer_type, allow_debit_negative_up_to, operation, source, source_id, customer_id, effective_at) VALUES ($1, $2, $3, $4, 100, $5, $6, 0, 'critical', 'critical', $7, $8, now())"
		for _, bad := range []struct {
			name, book    string
			debit, credit uuid.UUID
			currency, typ string
			customer      *uuid.UUID
			refusal       string
		}{
			{"units-to-general", "units", aliceUSD, revenue, "USD", "credit_spend", &a, "ledger_cross_book"},
			{"general-to-units", "general", receivable, sold, "USD", "usage_accrued", &a, "ledger_cross_book"},
			{"dollars-to-euros", "units", aliceUSD, aliceEUR, "USD", "credit_spend", &a, "ledger_cross_currency"},
			{"another-customers-balance", "units", bobUSD, sold, "USD", "credit_spend", &a, "ledger_customer_mismatch"},
			{"an-unattributed-balance", "units", aliceUSD, sold, "USD", "credit_spend", nil, "ledger_customer_mismatch"},
			{"another-books-type", "units", sold, aliceUSD, "USD", "cash_received", &a, "ledger_transfers_type_check"},
		} {
			_, err := w.pool.Exec(t.Context(), insert, mid, bad.book, bad.debit, bad.credit, bad.currency, bad.typ, bad.name, bad.customer)
			require.ErrorContains(t, err, bad.refusal, bad.name)
		}

		// No floor relaxes a general-ledger sign rule.
		_, err := w.pool.Exec(t.Context(), strings.Replace(insert, "$6, 0,", "$6, 5,", 1), mid, "general", receivable, revenue, "USD", "usage_accrued", "floor", &a)
		require.ErrorContains(t, err, "ledger_transfers_general_floor_check")

		// The units book's writer reaches the same refusals.
		for _, bad := range []struct {
			transfer ledger.Transfer
			refusal  string
		}{
			{ledger.Transfer{Debit: aliceUSD, Credit: revenue, Customer: &a}, "ledger_cross_book"},
			{ledger.Transfer{Debit: bobUSD, Credit: sold, Customer: &a}, "ledger_customer_mismatch"},
			{ledger.Transfer{Debit: aliceUSD, Credit: aliceEUR, Customer: &a}, "ledger_cross_currency"},
		} {
			bad.transfer.Amount, bad.transfer.Currency, bad.transfer.Type = 100, "USD", ledger.CreditSpend
			bad.transfer.Coord = ledger.Coord{Operation: ledger.OpSpend, Source: "critical", SourceID: bad.refusal}
			err := booksTx(t, w, func(ctx context.Context, q *gen.Queries) error {
				_, err := ledger.New(q, mid).Apply(ctx, bad.transfer)
				return err
			})
			require.ErrorContains(t, err, bad.refusal)
		}

		// The general ledger's poster refuses a leg in another customer's
		// account or in the units book before it writes.
		for _, bad := range []books.Posting{
			{Debit: books.Ref{Type: books.UsageReceivable, Customer: &b}, Credit: books.Ref{Type: books.UsageRevenue}, Customer: &a},
			{Debit: books.Ref{Type: books.UsageReceivable, Customer: &a}, Credit: books.Ref{Type: books.UnitsRedeemed}, Customer: &a},
		} {
			bad.Amount, bad.Currency, bad.Type, bad.EffectiveAt = 100, "USD", books.UsageAccrued, w.clock.Now()
			bad.Coord = books.Coord{Operation: "usage_accrual", Source: "critical", SourceID: string(bad.Debit.Type) + string(bad.Credit.Type)}
			err := booksTx(t, w, func(ctx context.Context, q *gen.Queries) error {
				_, _, err := books.New(q, mid).Post(ctx, bad)
				return err
			})
			require.Error(t, err)
		}

		// Nothing moved.
		require.Equal(t, before, booksSnapshot(t, w))
		require.Contains(t, hostCLI(t, w, "ledger-audit", "--merchant", "id:"+w.alpha.MerchantID().String()), "Ledger integrity OK")
	})

	t.Run("OR-J40-S03/ledger-audit-catches-injected-counter-and-rollup-corruption", func(t *testing.T) {
		w := newWorld(t, func(w *world) {
			alpha := cfgMerchant(w.cfg, "alpha")
			alpha.Accounting = &billing.AccountingSettings{Timezone: "America/New_York"}
			setCfgMerchant(&w.cfg, "alpha", alpha)
		})
		mid := w.alpha.MerchantID().UUID()

		// accounting.timezone is an IANA zone; the merchant's dates its books.
		for _, bad := range []string{"Mars/Olympus", "Local", "../zoneinfo/UTC"} {
			_, err := merchantconfig.Normalize("Alpha", billing.MerchantSettings{Accounting: &billing.AccountingSettings{Timezone: bad}})
			require.ErrorContains(t, err, "accounting.timezone", bad)
		}
		state, err := w.alpha.GetMerchantConfiguration(t.Context())
		require.NoError(t, err)
		require.NotNil(t, state.Accounting)
		require.Equal(t, "America/New_York", state.Accounting.Timezone)
		settings, err := merchantconfig.Normalize(state.DisplayName, state.MerchantSettings)
		require.NoError(t, err)
		zone, err := time.LoadLocation(settings.Config.AccountingTimezone)
		require.NoError(t, err)

		customer := billing.CustomerID(uuid.New())
		fundBooks(t, w, customer, "USD", 1000)
		c := customer.UUID()
		// Half past eleven on the evening of 30 November in New York is
		// 04:30 UTC on 1 December: its bucket is UTC, and a reader grouping in
		// the merchant's zone counts it on 30 November.
		at := time.Date(2026, time.November, 30, 23, 30, 0, 0, zone)
		var posted gen.BillingLedgerTransfer
		require.NoError(t, booksTx(t, w, func(ctx context.Context, q *gen.Queries) error {
			var err error
			posted, _, err = books.New(q, mid).Post(ctx, books.Posting{
				Debit: books.Ref{Type: books.UsageReceivable, Customer: &c}, Credit: books.Ref{Type: books.UsageRevenue},
				Amount: 2_500_000, Currency: "USD", Type: books.UsageAccrued,
				Coord:       books.Coord{Operation: "usage_accrual", Source: "critical", SourceID: "evening"},
				EffectiveAt: at, Customer: &c,
			})
			return err
		}))
		require.True(t, posted.EffectiveAt.Equal(at))
		var units gen.BillingLedgerTransfer
		require.NoError(t, w.pool.QueryRow(t.Context(), "SELECT effective_at, created_at FROM "+booksTable(w, "ledger_transfers")+" WHERE merchant_id=$1 AND book='units'", mid).Scan(&units.EffectiveAt, &units.CreatedAt))
		require.True(t, units.CreatedAt.Equal(units.EffectiveAt), "a units-book transfer takes effect when it posts")

		// Each leg is counted in its account type's total and day.
		require.Equal(t, [2]int64{0, 2_500_000}, booksTotal(t, w, "general", "usage_receivable"))
		require.Equal(t, [2]int64{2_500_000, 0}, booksTotal(t, w, "general", "usage_revenue"))
		require.Equal(t, [2]int64{1000, 0}, booksTotal(t, w, "units", "customer_units"))
		var credits int64
		var bucket time.Time
		var day string
		require.NoError(t, w.pool.QueryRow(t.Context(), "SELECT credits, bucket_at, (bucket_at AT TIME ZONE $2)::date::text FROM "+booksTable(w, "ledger_daily")+" WHERE merchant_id=$1 AND book='general' AND account_type='usage_revenue' AND transfer_type='usage_accrued'", mid, state.Accounting.Timezone).Scan(&credits, &bucket, &day))
		require.Equal(t, int64(2_500_000), credits)
		require.Equal(t, "2026-12-01T04:30:00Z", bucket.UTC().Format(time.RFC3339))
		require.Equal(t, "2026-11-30", day)
		audit := "id:" + w.alpha.MerchantID().String()
		require.Contains(t, hostCLI(t, w, "ledger-audit", "--merchant", audit), "Ledger integrity OK")

		// A restore rebuilds the rollups from the transfers it carries and
		// proves them before it commits.
		target, client := archiveTarget(t, w)
		_, err = client.ImportBillingArchive(t.Context(), bytes.NewReader(archiveBytes(t, w.alpha)))
		require.NoError(t, err)
		for _, rollup := range []string{"ledger_type_totals", "ledger_daily"} {
			require.Equal(t, booksRollup(t, w, rollup, mid), booksRollup(t, target, rollup, mid), rollup)
		}
		require.Contains(t, hostCLI(t, target, "ledger-audit", "--merchant", audit), "Ledger integrity OK")

		// Corruption no trigger writes: each is caught, none by a periodic
		// worker.
		corrupt := func(table, guard, statement string) {
			tx, err := w.pool.Begin(t.Context())
			require.NoError(t, err)
			defer tx.Rollback(context.Background())
			_, err = tx.Exec(t.Context(), "ALTER TABLE "+booksTable(w, table)+" DISABLE TRIGGER "+guard)
			require.NoError(t, err)
			_, err = tx.Exec(t.Context(), strings.ReplaceAll(statement, "$table", booksTable(w, table)), mid)
			require.NoError(t, err)
			_, err = tx.Exec(t.Context(), "ALTER TABLE "+booksTable(w, table)+" ENABLE TRIGGER "+guard)
			require.NoError(t, err)
			require.NoError(t, tx.Commit(t.Context()))
		}
		corrupt("ledger_daily", "guard_ledger_daily", "UPDATE $table SET credits=credits+1 WHERE merchant_id=$1 AND account_type='usage_revenue'")
		output, err := hostCLIResult(t, w, "ledger-audit", "--merchant", audit)
		require.Error(t, err)
		require.Contains(t, output, "rollup drift: ledger_daily 2026-12-01T04:30:00Z usage_accrued general/usage_revenue")
		corrupt("ledger_type_totals", "guard_ledger_type_totals", "DELETE FROM $table WHERE merchant_id=$1 AND account_type='customer_units'")
		corrupt("ledger_accounts", "guard_ledger_account_facts", "UPDATE $table SET credits_posted=credits_posted+1 WHERE merchant_id=$1 AND account_type='usage_revenue'")
		output, err = hostCLIResult(t, w, "ledger-audit", "--merchant", audit)
		require.Error(t, err)
		require.Contains(t, output, "rollup drift: ledger_type_totals units/customer_units")
		require.Contains(t, output, "counter drift:")
		require.Contains(t, output, "conservation: merchant="+w.alpha.MerchantID().String()+" book=general")
		require.Equal(t, 3, strings.Count(output, "FAIL("), output)
		output, err = hostCLIResult(t, w, "ledger-audit", "--merchant", audit, "--format", "json")
		require.Error(t, err)
		require.Contains(t, output, `"rollup_drifts"`)
	})

	t.Run("OR-J40-S07/an-archive-written-before-the-books-restores-as-the-units-book", func(t *testing.T) {
		source := newWorld(t)
		customer := billing.CustomerID(uuid.New())
		fundBooks(t, source, customer, "USD", 1000)
		archive := beforeBooks(t, archiveBytes(t, source.alpha))
		target, client := archiveTarget(t, source)
		receipt, err := client.ImportBillingArchive(t.Context(), bytes.NewReader(archive))
		require.NoError(t, err)
		require.False(t, receipt.AlreadyImported)
		balance(t, client, customer, 1000, 0, 1000)
		mid := source.alpha.MerchantID().UUID()
		rows, err := target.pool.Query(t.Context(), "SELECT account_type || ' ' || book || ' ' || class FROM "+booksTable(target, "ledger_accounts")+" WHERE merchant_id=$1", mid)
		require.NoError(t, err)
		accounts, err := pgx.CollectRows(rows, pgx.RowTo[string])
		require.NoError(t, err)
		require.ElementsMatch(t, []string{"customer_units units held", "units_sold units issued"}, accounts)
		var moved int
		require.NoError(t, target.pool.QueryRow(t.Context(), "SELECT count(*) FROM "+booksTable(target, "ledger_transfers")+" WHERE merchant_id=$1 AND book='units' AND effective_at=created_at", mid).Scan(&moved))
		require.Equal(t, 1, moved, "a transfer is the units book's, effective when it posted")
		require.Equal(t, booksRollup(t, source, "ledger_type_totals", mid), booksRollup(t, target, "ledger_type_totals", mid))
		require.Contains(t, hostCLI(t, target, "ledger-audit", "--merchant", "id:"+source.alpha.MerchantID().String()), "Ledger integrity OK")
	})

	t.Run("OR-J40-S06/concurrent-top-ups-one-posting-unposted-usage-first-all-commit", func(t *testing.T) {
		w := newWorld(t)
		meterForCapture(t, w)
		// A top-up posts the customer's unposted usage, then the deposit:
		// the rollups both touch are kept at commit, after every account
		// lock, so neither waits on the other's rollups mid-transaction.
		for round := range 16 {
			used, plain := billing.CustomerID(uuid.New()), billing.CustomerID(uuid.New())
			_, err := w.alpha.CreateCreditGrants(t.Context(), []billing.CreateCreditGrantParams{{CustomerID: used, Currency: "USD", Amount: 1000, Source: "critical", SourceID: fmt.Sprintf("fund-%d", round)}})
			require.NoError(t, err)
			request := fmt.Sprintf("use-%d", round)
			admitted, err := w.alpha.Admit(t.Context(), []billing.AdmitParams{reservation(w, used, request, 400)})
			require.NoError(t, err)
			require.True(t, admitted[0].Allowed())
			spent, err := w.alpha.CaptureAdmissions(t.Context(), []billing.CaptureAdmissionParams{capture(request, 400)})
			require.NoError(t, err)
			require.NoError(t, spent[0].Err())
			var errs [2]error
			var group sync.WaitGroup
			start := make(chan struct{})
			for i, customer := range []billing.CustomerID{used, plain} {
				group.Go(func() {
					<-start
					_, errs[i] = w.alpha.CreateCreditGrants(t.Context(), []billing.CreateCreditGrantParams{{CustomerID: customer, Currency: "USD", Amount: 500, Source: "critical", SourceID: fmt.Sprintf("top-up-%d-%d", round, i)}})
				})
			}
			close(start)
			group.Wait()
			require.NoError(t, errs[0], "round %d", round)
			require.NoError(t, errs[1], "round %d", round)
			balance(t, w.alpha, used, 1100, 0, 1100)
			balance(t, w.alpha, plain, 500, 0, 500)
		}
		require.Contains(t, hostCLI(t, w, "ledger-audit", "--merchant", "id:"+w.alpha.MerchantID().String()), "Ledger integrity OK")
	})
}

// TestBooksMigration migrates a database that predates the books, with its
// ledger, to the books.
func TestBooksMigration(t *testing.T) {
	t.Run("OR-J40-S05/a-ledger-from-before-the-books-migrates-with-its-rollups", func(t *testing.T) {
		dsn := os.Getenv("OPENRAILS_CRITICAL_DSN")
		require.NotEmpty(t, dsn, "OPENRAILS_CRITICAL_DSN must name a disposable PostgreSQL database; critical scenarios never skip")
		pool, err := pgxpool.New(t.Context(), dsn)
		require.NoError(t, err)
		t.Cleanup(pool.Close)
		schema := "critical_" + strings.ReplaceAll(uuid.NewString(), "-", "")
		// Every migration before the books', whatever their numbers.
		names, err := fs.Glob(postgresmigrations.FS, "*.up.sql")
		require.NoError(t, err)
		slices.SortFunc(names, func(a, b string) int { return cmp.Compare(migrationNumber(t, a), migrationNumber(t, b)) })
		at := slices.IndexFunc(names, func(name string) bool { return strings.HasSuffix(name, "_books.up.sql") })
		require.Positive(t, at)
		before := fstest.MapFS{}
		for _, name := range names[:at] {
			body, err := fs.ReadFile(postgresmigrations.FS, name)
			require.NoError(t, err)
			before[name] = &fstest.MapFile{Data: body}
		}
		require.NoError(t, migrate.Apply(t.Context(), pool, migrate.Options{Schema: schema, Chain: before, Logger: slog.New(slog.DiscardHandler)}))
		table := func(name string) string { return pgx.Identifier{schema, name}.Sanitize() }

		// The ledger as it stood: a customer's deposit, a spend, and usage
		// owed beyond the balance, posted by the triggers of the time.
		var mid uuid.UUID
		require.NoError(t, pool.QueryRow(t.Context(), "INSERT INTO "+table("merchants")+" (slug, status) VALUES ('alpha', 'active') RETURNING id").Scan(&mid))
		customer := uuid.New()
		_, err = pool.Exec(t.Context(), "INSERT INTO "+table("customers")+" (merchant_id, id) VALUES ($1, $2)", mid, customer)
		require.NoError(t, err)
		accounts := map[string]uuid.UUID{}
		for _, a := range []struct {
			typ      string
			customer *uuid.UUID
			floor    bool
		}{{"customer_balance", &customer, true}, {"arrears_liability", &customer, false}, {"processor_clearing", nil, false}, {"platform_revenue", nil, false}} {
			var id uuid.UUID
			require.NoError(t, pool.QueryRow(t.Context(), "INSERT INTO "+table("ledger_accounts")+" (merchant_id, customer_id, account_type, currency, debits_must_not_exceed_credits, credits_must_not_exceed_debits) VALUES ($1, $2, $3, 'USD', $4, false) RETURNING id", mid, a.customer, a.typ, a.floor).Scan(&id))
			accounts[a.typ] = id
		}
		for i, tr := range []struct {
			debit, credit, typ string
			amount             int64
		}{{"processor_clearing", "customer_balance", "deposit", 1000}, {"customer_balance", "platform_revenue", "credit_spend", 400}, {"arrears_liability", "platform_revenue", "owed_accrual", 250}} {
			_, err := pool.Exec(t.Context(), "INSERT INTO "+table("ledger_transfers")+" (merchant_id, debit_account_id, credit_account_id, amount, currency, transfer_type, allow_debit_negative_up_to, operation, source, source_id, customer_id) VALUES ($1, $2, $3, $4, 'USD', $5, 0, 'critical', 'critical', $6, $7)",
				mid, accounts[tr.debit], accounts[tr.credit], tr.amount, tr.typ, fmt.Sprint(i), customer)
			require.NoError(t, err)
		}

		// The engine starts on it and migrates it to the books.
		w := newWorld(t, func(w *world) {
			w.schema = schema
			w.cfg.Database = openrails.DatabaseConfig{Schema: schema, RiverSchema: schema}
		})
		require.Equal(t, mid, w.alpha.MerchantID().UUID())
		balance(t, w.alpha, billing.CustomerID(customer), 600, 0, 600)
		rows, err := w.pool.Query(t.Context(), "SELECT account_type || ' ' || book || ' ' || class FROM "+table("ledger_accounts")+" WHERE merchant_id=$1", mid)
		require.NoError(t, err)
		renamed, err := pgx.CollectRows(rows, pgx.RowTo[string])
		require.NoError(t, err)
		require.ElementsMatch(t, []string{"customer_units units held", "customer_owed units owed", "units_sold units issued", "units_redeemed units retired"}, renamed)
		require.Equal(t, [2]int64{1000, 400}, booksTotal(t, w, "units", "customer_units"))
		require.Equal(t, [2]int64{650, 0}, booksTotal(t, w, "units", "units_redeemed"))
		require.Equal(t, [2]int64{0, 250}, booksTotal(t, w, "units", "customer_owed"))
		require.Contains(t, hostCLI(t, w, "ledger-audit", "--merchant", "id:"+w.alpha.MerchantID().String()), "Ledger integrity OK")
		require.NotEmpty(t, archiveBytes(t, w.alpha), "the export's ledger check accepts the migrated books")
	})
}

// booksTx runs fn in one of alpha's transactions, over the engine's queries.
func booksTx(t *testing.T, w *world, fn func(ctx context.Context, q *gen.Queries) error) error {
	t.Helper()
	database, err := db.NewWithPGXPool(slog.Default(), w.pool, w.schema)
	require.NoError(t, err)
	ctx := merchant.WithID(t.Context(), w.alpha.MerchantID())
	return database.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		return fn(ctx, database.NewWithPgxTx(tx).Gen(ctx))
	})
}

func booksTable(w *world, table string) string {
	return pgx.Identifier{w.schema, table}.Sanitize()
}

// fundBooks gives the customer a units-book balance through the public API.
func fundBooks(t *testing.T, w *world, customer billing.CustomerID, currency catalog.Currency, amount int64) {
	t.Helper()
	_, err := w.alpha.CreateCreditGrants(t.Context(), []billing.CreateCreditGrantParams{{CustomerID: customer, Currency: currency, Amount: amount, Source: "critical", SourceID: "books-" + string(currency)}})
	require.NoError(t, err)
}

// fixturePSP is a PSP row for a cash-clearing account; no charge reaches it.
func fixturePSP(t *testing.T, w *world) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	require.NoError(t, w.pool.QueryRow(t.Context(), "INSERT INTO "+booksTable(w, "psps")+" (merchant_id, key, rail, environment, account_id) VALUES ($1, 'books', 'nmi', 'test', $2) RETURNING id", w.alpha.MerchantID().UUID(), uuid.NewString()).Scan(&id))
	return id
}

// booksSnapshot is alpha's transfers and counters, to show nothing moved.
func booksSnapshot(t *testing.T, w *world) [3]int64 {
	t.Helper()
	var s [3]int64
	require.NoError(t, w.pool.QueryRow(t.Context(), "SELECT (SELECT count(*) FROM "+booksTable(w, "ledger_transfers")+" WHERE merchant_id=$1), COALESCE(sum(credits_posted), 0), COALESCE(sum(debits_posted), 0) FROM "+booksTable(w, "ledger_accounts")+" WHERE merchant_id=$1", w.alpha.MerchantID().UUID()).Scan(&s[0], &s[1], &s[2]))
	return s
}

// booksTotal is one account type's credits and debits in ledger_type_totals.
func booksTotal(t *testing.T, w *world, book, accountType string) [2]int64 {
	t.Helper()
	var total [2]int64
	err := w.pool.QueryRow(t.Context(), "SELECT credits_posted, debits_posted FROM "+booksTable(w, "ledger_type_totals")+" WHERE merchant_id=$1 AND book=$2 AND currency='USD' AND account_type=$3", w.alpha.MerchantID().UUID(), book, accountType).Scan(&total[0], &total[1])
	if errors.Is(err, pgx.ErrNoRows) {
		return total
	}
	require.NoError(t, err)
	return total
}

// booksRollup is one merchant's rows of a rollup as text, in one order.
func booksRollup(t *testing.T, w *world, table string, merchant uuid.UUID) []string {
	t.Helper()
	rows, err := w.pool.Query(t.Context(), "SELECT r::text FROM "+booksTable(w, table)+" r WHERE merchant_id=$1", merchant)
	require.NoError(t, err)
	out, err := pgx.CollectRows(rows, pgx.RowTo[string])
	require.NoError(t, err)
	slices.Sort(out)
	return out
}

// beforeBooks rewrites an archive into the shape exports had before the
// books: accounts by their former names with no book, class or PSP;
// transfers with no book, effective_at, order line or product.
func beforeBooks(t *testing.T, archive []byte) []byte {
	t.Helper()
	former := map[string]string{}
	for _, a := range books.Chart() {
		if a.Book == books.Units {
			former[string(a.Type)] = string(a.Type)
		}
	}
	for old, now := range map[string]string{"customer_balance": "customer_units", "arrears_liability": "customer_owed", "processor_clearing": "units_sold",
		"promotional_funding": "units_granted", "platform_revenue": "units_redeemed", "expired_credits": "units_expired", "revoked_credits": "units_revoked",
		"credit_refund_clearing": "units_refund_clearing", "credit_refund_loss": "units_refund_loss"} {
		former[now] = old
	}
	profile := func(name string) contract.Profile {
		i := slices.IndexFunc(contract.Profiles, func(p contract.Profile) bool { return p.Name == name })
		require.GreaterOrEqual(t, i, 0, name)
		return contract.Profiles[i]
	}
	accounts, transfers := profile("ledger_accounts"), profile("ledger_transfers")
	typeAt := slices.IndexFunc(accounts.Columns, func(c contract.Column) bool { return c.Name == "account_type" })
	var out bytes.Buffer
	var w *archivewire.Writer
	var table string
	_, err := archivewire.Read(bytes.NewReader(archive), func(h archivewire.Header) error {
		var err error
		w, err = archivewire.NewWriterWithTenant(&out, h.MerchantID, h.TenantID, h.CatalogRevision)
		return err
	}, func(r archivewire.Record) error {
		if r.Kind == "table" {
			table = r.Table
			return w.Table(table)
		}
		v := r.Values
		switch table {
		case "ledger_accounts":
			require.Len(t, v, len(accounts.Columns))
			v = slices.Clone(v[:len(v)-3])
			old := former[*v[typeAt]]
			require.NotEmpty(t, old, *v[typeAt])
			v[typeAt] = &old
		case "ledger_transfers":
			require.Len(t, v, len(transfers.Columns))
			v = v[:len(v)-4]
		}
		return w.Row(v)
	})
	require.NoError(t, err)
	require.NoError(t, w.Close())
	return out.Bytes()
}

func migrationNumber(t *testing.T, name string) int {
	t.Helper()
	n, err := strconv.Atoi(name[:strings.IndexByte(name, '_')])
	require.NoError(t, err, name)
	return n
}
