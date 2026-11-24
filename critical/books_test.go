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
		// with its sign rules, scoped as the chart says: a merchant-wide
		// general-ledger account by Open, before any business transaction.
		booksOpen(t, w, mid, "USD")
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

		// A balance read is a counter: a merchant-wide general-ledger account
		// keeps none, and reads none.
		require.NoError(t, booksTx(t, w, func(ctx context.Context, q *gen.Queries) error {
			revenue, err := q.GetLedgerAccount(ctx, gen.GetLedgerAccountParams{MerchantID: mid, AccountType: string(books.UsageRevenue), Currency: "USD"})
			if err != nil {
				return err
			}
			_, err = ledger.New(q, mid).Balance(ctx, revenue.ID)
			require.ErrorIs(t, err, pgx.ErrNoRows)
			return nil
		}))

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

		// Each transfer type belongs to one book, but usage_reversed to both:
		// the units book's are the ledger's, the general ledger's the chart's.
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
		var both []string
		for _, typ := range units {
			if slices.Contains(general, typ) {
				both = append(both, typ)
			}
		}
		require.Equal(t, []string{string(books.UsageReversed)}, both)

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

		// usage_reversed posts in each book at one coordinate. Its units leg
		// reads as a spend given back to its lot, or as usage owed taken back;
		// the lot counts it unspent; the units book's coordinate lookup reads
		// the units leg, though the general ledger's posted after it.
		var lot uuid.UUID
		require.NoError(t, w.pool.QueryRow(t.Context(), "SELECT id FROM "+booksTable(w, "credit_grants")+" WHERE merchant_id=$1 AND customer_id=$2", mid, c).Scan(&lot))
		reversal := books.Coord{Operation: "usage_reversal", Source: "critical", SourceID: "period"}
		require.NoError(t, booksTx(t, w, func(ctx context.Context, q *gen.Queries) error {
			l := ledger.New(q, mid)
			held, err := l.EnsureCustomerBalance(ctx, c, "USD")
			if err != nil {
				return err
			}
			owed, err := l.EnsureCustomerArrears(ctx, c, "USD")
			if err != nil {
				return err
			}
			redeemed, err := l.EnsureSystemAccount(ctx, ledger.UnitsRedeemed, "USD")
			if err != nil {
				return err
			}
			for _, tr := range []ledger.Transfer{
				{Debit: held, Credit: redeemed, Amount: 300, Type: ledger.CreditSpend, CreditGrantID: &lot, Coord: ledger.Coord{Operation: ledger.OpSpend, Source: "critical", SourceID: "spent"}},
				{Debit: redeemed, Credit: held, Amount: 100, Type: ledger.UsageReversed, CreditGrantID: &lot, Coord: ledger.Coord{Operation: "usage_reversal", Source: "critical", SourceID: "given-back"}},
				{Debit: owed, Credit: redeemed, Amount: 50, Type: ledger.OwedAccrual, Coord: ledger.Coord{Operation: "usage_accrual", Source: "critical", SourceID: "owed"}},
				{Debit: redeemed, Credit: owed, Amount: 20, Type: ledger.UsageReversed, Coord: reversal},
			} {
				tr.Currency, tr.Customer = "USD", &c
				if _, err := l.Apply(ctx, tr); err != nil {
					return err
				}
			}
			return nil
		}))
		require.NoError(t, booksTx(t, w, func(ctx context.Context, q *gen.Queries) error {
			_, applied, err := books.New(q, mid).Post(ctx, books.Posting{
				Debit: books.Ref{Type: books.RevenueAdjustments}, Credit: books.Ref{Type: books.UsageReceivable, Customer: &c},
				Amount: 20, Currency: "USD", Type: books.UsageReversed, Coord: reversal, EffectiveAt: w.clock.Now(), Customer: &c,
			})
			require.True(t, applied, "the general ledger's leg is not the units book's")
			return err
		}))
		require.NoError(t, booksTx(t, w, func(ctx context.Context, q *gen.Queries) error {
			tr, err := q.GetLedgerTransferByCoords(ctx, gen.GetLedgerTransferByCoordsParams{MerchantID: mid, CustomerID: c, Currency: "USD",
				TransferType: string(ledger.UsageReversed), Operation: string(reversal.Operation), Source: reversal.Source, SourceID: reversal.SourceID})
			require.Equal(t, "units", tr.Book)
			return err
		}))
		history, err := w.alpha.ListBalanceTransactions(t.Context(), customer, billing.BalanceTransactionListParams{Currency: "USD"})
		require.NoError(t, err)
		var moves []string
		for _, item := range history.Items {
			moves = append(moves, fmt.Sprintf("%s %d", item.Type, item.Amount))
		}
		require.ElementsMatch(t, []string{"deposit 1000", "spend -300", "spend 100", "owed_accrual 50", "owed_accrual -20"}, moves)
		grant, err := w.alpha.GetCreditGrant(t.Context(), billing.CreditGrantID(lot))
		require.NoError(t, err)
		require.Equal(t, [2]int64{200, 800}, [2]int64{grant.SpentAmount, grant.RemainingAmount})
		balance(t, w.alpha, customer, 800, 0, 800)
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
		booksOpen(t, w, mid, "USD")
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

		// A general-ledger transfer carries no floor.
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

		// The general ledger is opened outside business transactions, never
		// inside one.
		database, err := db.NewWithPGXPool(slog.Default(), w.pool, w.schema)
		require.NoError(t, err)
		require.ErrorIs(t, booksRawTx(t, w, func(ctx context.Context, _ pgx.Tx, _ *gen.Queries) error {
			return books.Open(ctx, database.DataPool(), mid, "EUR")
		}), db.ErrCallerTransaction)

		// Nothing moved.
		require.Equal(t, before, booksSnapshot(t, w))
		require.Contains(t, hostCLI(t, w, "ledger-audit", "--merchant", "id:"+w.alpha.MerchantID().String()), "Ledger integrity OK")

		// A merchant-wide account's sign rule never refuses a commit: unapplied
		// cash drawn beyond what was received commits, raises books.sign_breach
		// once and fails the audit. Resolved, a later breach reopens it.
		draw := func(key string) error {
			return booksTx(t, w, func(ctx context.Context, q *gen.Queries) error {
				_, _, err := books.New(q, mid).Post(ctx, books.Posting{
					Debit: books.Ref{Type: books.UnappliedCash}, Credit: books.Ref{Type: books.DeferredRevenue},
					Amount: 100, Currency: "USD", Type: books.CashApplied, EffectiveAt: w.clock.Now(),
					Coord: books.Coord{Operation: "cash_application", Source: "critical", SourceID: key},
				})
				return err
			})
		}
		breach := func() (status string, findings int) {
			require.NoError(t, w.pool.QueryRow(t.Context(), "SELECT max(status), count(*) FROM "+booksTable(w, "reconciliation_findings")+
				" WHERE merchant_id=$1 AND finding_type='books.sign_breach' AND subject_key='unapplied_cash/USD'", mid).Scan(&status, &findings))
			return status, findings
		}
		require.NoError(t, draw("unfunded"))
		status, findings := breach()
		require.Equal(t, "requires_review", status)
		require.Equal(t, 1, findings)
		output, err := hostCLIResult(t, w, "ledger-audit", "--merchant", "id:"+w.alpha.MerchantID().String())
		require.Error(t, err)
		require.Contains(t, output, "sign: general/unapplied_cash")
		_, err = w.pool.Exec(t.Context(), "UPDATE "+booksTable(w, "reconciliation_findings")+" SET status='fixed', resolution='admin_fixed', resolved_at=now()"+
			" WHERE merchant_id=$1 AND finding_type='books.sign_breach'", mid)
		require.NoError(t, err)
		require.NoError(t, draw("unfunded-again"))
		status, findings = breach()
		require.Equal(t, "requires_review", status, "a later breach reopens it")
		require.Equal(t, 1, findings)

		// A customer's general-ledger account is recorded, never refused,
		// too: usage receivable credited beyond what was accrued commits and
		// raises the finding for that customer.
		require.NoError(t, booksTx(t, w, func(ctx context.Context, q *gen.Queries) error {
			_, _, err := books.New(q, mid).Post(ctx, books.Posting{
				Debit: books.Ref{Type: books.RevenueAdjustments}, Credit: books.Ref{Type: books.UsageReceivable, Customer: &a},
				Amount: 100, Currency: "USD", Type: books.UsageReversed, EffectiveAt: w.clock.Now(), Customer: &a,
				Coord: books.Coord{Operation: "usage_reversal", Source: "critical", SourceID: "beyond-accrued"},
			})
			return err
		}))
		var customerBreach string
		require.NoError(t, w.pool.QueryRow(t.Context(), "SELECT status FROM "+booksTable(w, "reconciliation_findings")+
			" WHERE merchant_id=$1 AND finding_type='books.sign_breach' AND subject_key=$2", mid, "usage_receivable/USD/"+a.String()).Scan(&customerBreach))
		require.Equal(t, "requires_review", customerBreach)
		output, err = hostCLIResult(t, w, "ledger-audit", "--merchant", "id:"+w.alpha.MerchantID().String())
		require.Error(t, err)
		require.Contains(t, output, "sign: general/usage_receivable customer="+a.String())
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
		booksOpen(t, w, mid, "USD")
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
			if err != nil {
				return err
			}
			_, _, err = books.New(q, mid).Post(ctx, books.Posting{
				Debit: books.Ref{Type: books.ExternalReceipts}, Credit: books.Ref{Type: books.UnappliedCash},
				Amount: 1_000_000, Currency: "USD", Type: books.CashReceived, EffectiveAt: at,
				Coord: books.Coord{Operation: "cash_receipt", Source: "critical", SourceID: "remittance"},
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
		// A customer's account keeps counters; a merchant-wide general-ledger
		// account keeps none, its type total being its balance.
		require.Equal(t, [2]int64{0, 2_500_000}, booksCounters(t, w, "usage_receivable"))
		require.Equal(t, [2]int64{0, 0}, booksCounters(t, w, "usage_revenue"))
		var credits int64
		var bucket time.Time
		var day string
		require.NoError(t, w.pool.QueryRow(t.Context(), "SELECT credits, bucket_at, (bucket_at AT TIME ZONE $2)::date::text FROM "+booksTable(w, "ledger_daily")+" WHERE merchant_id=$1 AND book='general' AND account_type='usage_revenue' AND transfer_type='usage_accrued'", mid, state.Accounting.Timezone).Scan(&credits, &bucket, &day))
		require.Equal(t, int64(2_500_000), credits)
		require.Equal(t, "2026-12-01T04:30:00Z", bucket.UTC().Format(time.RFC3339))
		require.Equal(t, "2026-11-30", day)
		audit := "id:" + w.alpha.MerchantID().String()
		require.Contains(t, hostCLI(t, w, "ledger-audit", "--merchant", audit), "Ledger integrity OK")

		// A merchant-wide type total opposite its class's normal side, with no
		// sign rule to refuse it, is reported as a warning: the books stay OK.
		require.NoError(t, booksTx(t, w, func(ctx context.Context, q *gen.Queries) error {
			_, _, err := books.New(q, mid).Post(ctx, books.Posting{
				Debit: books.Ref{Type: books.OneTimeRevenue}, Credit: books.Ref{Type: books.ExternalReceipts},
				Amount: 100, Currency: "USD", Type: books.RevenueAdjusted, EffectiveAt: at,
				Coord: books.Coord{Operation: "revenue_adjustment", Source: "critical", SourceID: "debit-balance"},
			})
			return err
		}))
		output := hostCLI(t, w, "ledger-audit", "--merchant", audit)
		require.Contains(t, output, "Ledger integrity OK")
		require.Contains(t, output, "WARN(1)")
		require.Contains(t, output, "side: general/one_time_revenue")

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
		output, err = hostCLIResult(t, w, "ledger-audit", "--merchant", audit)
		require.Error(t, err)
		require.Contains(t, output, "rollup drift: ledger_daily 2026-12-01T04:30:00Z usage_accrued general/usage_revenue")
		corrupt("ledger_type_totals", "guard_ledger_type_totals", "DELETE FROM $table WHERE merchant_id=$1 AND account_type='customer_units'")
		corrupt("ledger_accounts", "guard_ledger_account_facts", "UPDATE $table SET debits_posted=debits_posted+1 WHERE merchant_id=$1 AND account_type='usage_receivable'")
		corrupt("ledger_accounts", "guard_ledger_account_facts", "UPDATE $table SET credits_posted=credits_posted+1 WHERE merchant_id=$1 AND account_type='usage_revenue'")
		corrupt("ledger_type_totals", "guard_ledger_type_totals", "UPDATE $table SET debits_posted=debits_posted+2000000 WHERE merchant_id=$1 AND account_type='unapplied_cash'")
		output, err = hostCLIResult(t, w, "ledger-audit", "--merchant", audit)
		require.Error(t, err)
		require.Contains(t, output, "rollup drift: ledger_type_totals units/customer_units")
		require.Contains(t, output, "rollup drift: ledger_type_totals general/unapplied_cash")
		require.Equal(t, 2, strings.Count(output, "counter drift:"), output)
		require.Contains(t, output, "conservation: merchant="+w.alpha.MerchantID().String()+" book=general")
		require.Contains(t, output, "sign: general/unapplied_cash balance=")
		require.Equal(t, 4, strings.Count(output, "FAIL("), output)
		output, err = hostCLIResult(t, w, "ledger-audit", "--merchant", audit, "--format", "json")
		require.Error(t, err)
		require.Contains(t, output, `"rollup_drifts"`)
		require.Contains(t, output, `"sign_breaches"`)
		require.Contains(t, output, `"side_warnings"`)
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
		require.ElementsMatch(t, []string{"customer_units units held", "units_granted units issued"}, accounts, "a gift's units are granted")
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
			w.clock.Advance(time.Second)
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

	t.Run("OR-J40-S08/a-ledger-transfer-and-an-mrr-change-in-either-order-both-commit", func(t *testing.T) {
		a := ccbillWebhookWorld(t)
		w := a.world
		mid := w.alpha.MerchantID().UUID()
		price := ccbillMemberPrice(t, a.alpha)
		res, err := a.alpha.ImportBilling(t.Context(), ccbillBook(price, a.clock.Now().UTC().Truncate(time.Second), 0, 2))
		require.NoError(t, err)
		require.Len(t, res.Imported, 2, "%v", res.Reasons)
		rows, err := w.pool.Query(t.Context(), "SELECT id, customer_id FROM "+booksTable(w, "subscriptions")+" WHERE merchant_id=$1 ORDER BY id", mid)
		require.NoError(t, err)
		type member struct{ Sub, Customer uuid.UUID }
		members, err := pgx.CollectRows(rows, pgx.RowToStructByPos[member])
		require.NoError(t, err)
		require.Len(t, members, 2)
		booksOpen(t, w, mid, "USD")
		seat := func(ctx context.Context, tx pgx.Tx, sub uuid.UUID) error {
			_, err := tx.Exec(ctx, "UPDATE "+booksTable(w, "subscriptions")+" SET quantity=COALESCE(quantity, 1)+1, updated_at=now() WHERE merchant_id=$1 AND id=$2", mid, sub)
			return err
		}
		require.NoError(t, booksTx(t, w, func(ctx context.Context, q *gen.Queries) error {
			for i, m := range members {
				if err := booksAccrue(ctx, q, w, mid, m.Customer, fmt.Sprintf("open-%d", i)); err != nil {
					return err
				}
			}
			return nil
		}))
		// The first transaction posts, then adds a seat. Held at its commit
		// inside the ledger step, it holds the totals both post to.
		release := booksHold(t, w, mid, "usage_revenue")
		first := booksCommit(t, w, func(ctx context.Context, tx pgx.Tx, q *gen.Queries) error {
			if err := booksAccrue(ctx, q, w, mid, members[0].Customer, "posted-first"); err != nil {
				return err
			}
			return seat(ctx, tx, members[0].Sub)
		})
		first.blocked(t, w, "the transaction that posted first")
		// The second adds a seat, then posts: its commit settles the ledger
		// before MRR too, so it waits on the first's totals holding nothing
		// the first needs.
		second := booksCommit(t, w, func(ctx context.Context, tx pgx.Tx, q *gen.Queries) error {
			if err := seat(ctx, tx, members[1].Sub); err != nil {
				return err
			}
			return booksAccrue(ctx, q, w, mid, members[1].Customer, "seated-first")
		})
		second.blocked(t, w, "the transaction that seated first")
		release()
		require.NoError(t, first.wait())
		require.NoError(t, second.wait())
		require.Equal(t, [3]int64{2 * 2 * ccbillMemberMRR, 2, 2}, mrrTotals(t, w, a.alpha, "USD"))
		require.Equal(t, [2]int64{4 * 100, 0}, booksTotal(t, w, "general", "usage_revenue"))
		require.Contains(t, hostCLI(t, w, "ledger-audit", "--merchant", "id:"+w.alpha.MerchantID().String()), "Ledger integrity OK")
	})

	t.Run("OR-J40-S09/two-checkouts-and-a-usage-collection-in-opposite-lock-orders-all-commit", func(t *testing.T) {
		w := newWorld(t)
		mid := w.alpha.MerchantID().UUID()
		customer := billing.CustomerID(uuid.New())
		fundBooks(t, w, customer, "USD", 1000)
		c, psp := customer.UUID(), fixturePSP(t, w)
		const rounds = 8
		post := func(ctx context.Context, q *gen.Queries, debit, credit books.Ref, typ books.TransferType, currency, key string, who *uuid.UUID) error {
			_, _, err := books.New(q, mid).Post(ctx, books.Posting{
				Debit: debit, Credit: credit, Amount: 100, Currency: currency, Type: typ, EffectiveAt: w.clock.Now(), Customer: who,
				Coord: books.Coord{Operation: books.Operation(typ), Source: "critical", SourceID: key},
			})
			return err
		}
		cash, unapplied := books.Ref{Type: books.CashClearing, PSP: &psp}, books.Ref{Type: books.UnappliedCash}
		deferred, receivable := books.Ref{Type: books.DeferredRevenue}, books.Ref{Type: books.UsageReceivable, Customer: &c}
		checkout := func(ctx context.Context, q *gen.Queries, currency, key string) error {
			if err := post(ctx, q, cash, unapplied, books.CashReceived, currency, key, nil); err != nil {
				return err
			}
			return post(ctx, q, unapplied, deferred, books.CashApplied, currency, key, nil)
		}

		// The first checkouts in a currency find its accounts open, opened
		// by the catalog apply that made it sellable (every PSP's cash
		// clearing included): neither waits on the other.
		document, err := catalog.ParseApplicationYAML([]byte(`schema_version: 1
products:
  books-eur:
    display_name: Books in euros
    prices:
      monthly: {amount: 20 EUR, billing_interval: 1 month}
`))
		require.NoError(t, err)
		_, err = w.alpha.ApplyCatalog(t.Context(), document, billing.ApplyCatalogParams{})
		require.NoError(t, err)
		var firsts [2]error
		var group sync.WaitGroup
		posted := [2]chan struct{}{make(chan struct{}), make(chan struct{})}
		for i := range posted {
			group.Go(func() {
				firsts[i] = booksRawTx(t, w, func(ctx context.Context, _ pgx.Tx, q *gen.Queries) error {
					if err := checkout(ctx, q, "EUR", fmt.Sprintf("first-%d", i)); err != nil {
						return err
					}
					close(posted[i])
					return booksAwait(posted[1-i], "the other first checkout")
				})
			})
		}
		group.Wait()
		require.NoError(t, firsts[0])
		require.NoError(t, firsts[1])

		// A posting that meets an account nothing opened still posts: it
		// creates the account in its own transaction and raises
		// books.account.opened_late, once per account.
		for i := range 2 {
			require.NoError(t, booksTx(t, w, func(ctx context.Context, q *gen.Queries) error {
				return checkout(ctx, q, "GBP", fmt.Sprintf("unopened-%d", i))
			}))
		}
		rows, err := w.pool.Query(t.Context(), "SELECT subject_key FROM "+booksTable(w, "reconciliation_findings")+" WHERE merchant_id=$1 AND finding_type=$2", mid, books.OpenedLateFindingType)
		require.NoError(t, err)
		late, err := pgx.CollectRows(rows, pgx.RowTo[string])
		require.NoError(t, err)
		require.ElementsMatch(t, []string{"cash_clearing/GBP/" + psp.String(), "unapplied_cash/GBP", "deferred_revenue/GBP"}, late)
		var gbp [2]int64
		require.NoError(t, w.pool.QueryRow(t.Context(), "SELECT credits_posted, debits_posted FROM "+booksTable(w, "ledger_type_totals")+" WHERE merchant_id=$1 AND book='general' AND currency='GBP' AND account_type='unapplied_cash'", mid).Scan(&gbp[0], &gbp[1]))
		require.Equal(t, [2]int64{200, 200}, gbp)

		// The customer owes every round's usage, and remittances cover every
		// collection's draw on unapplied cash.
		booksOpen(t, w, mid, "USD")
		require.NoError(t, booksTx(t, w, func(ctx context.Context, q *gen.Queries) error {
			if err := checkout(ctx, q, "USD", "open"); err != nil {
				return err
			}
			for round := range rounds {
				if err := post(ctx, q, receivable, books.Ref{Type: books.UsageRevenue}, books.UsageAccrued, "USD", fmt.Sprintf("usage-%d", round), &c); err != nil {
					return err
				}
				if err := post(ctx, q, books.Ref{Type: books.ExternalReceipts}, unapplied, books.CashReceived, "USD", fmt.Sprintf("remittance-%d", round), nil); err != nil {
					return err
				}
			}
			return nil
		}))
		// The collection locks the customer and then draws unapplied cash;
		// one checkout posts cash and then locks the customer, the other only
		// posts. A merchant-wide account keeps no counters, so no checkout
		// holds unapplied cash until it commits: neither checkout waits on
		// the other, and the collection waits on neither.
		for round := range rounds {
			var errs [3]error
			var group sync.WaitGroup
			locked, posted := make(chan struct{}), [2]chan struct{}{make(chan struct{}), make(chan struct{})}
			group.Go(func() {
				errs[0] = booksRawTx(t, w, func(ctx context.Context, _ pgx.Tx, q *gen.Queries) error {
					if _, err := q.LockCustomerForSpend(ctx, gen.LockCustomerForSpendParams{ID: c, MerchantID: mid}); err != nil {
						return err
					}
					close(locked)
					for i, step := range posted {
						if err := booksAwait(step, fmt.Sprintf("checkout %d's cash", i)); err != nil {
							return err
						}
					}
					return post(ctx, q, unapplied, receivable, books.UsageCollected, "USD", fmt.Sprintf("collect-%d", round), &c)
				})
			})
			for i := range posted {
				group.Go(func() {
					errs[1+i] = booksRawTx(t, w, func(ctx context.Context, _ pgx.Tx, q *gen.Queries) error {
						if err := booksAwait(locked, "the collection's customer lock"); err != nil {
							return err
						}
						if err := checkout(ctx, q, "USD", fmt.Sprintf("checkout-%d-%d", round, i)); err != nil {
							return err
						}
						close(posted[i])
						if i == 0 {
							_, err := q.LockCustomerForSpend(ctx, gen.LockCustomerForSpendParams{ID: c, MerchantID: mid})
							return err
						}
						return nil
					})
				})
			}
			group.Wait()
			for i, err := range errs {
				require.NoError(t, err, "round %d, transaction %d", round, i)
			}
		}
		require.Equal(t, [2]int64{100 * (1 + 3*rounds), 100 * (1 + 3*rounds)}, booksTotal(t, w, "general", "unapplied_cash"))
		require.Equal(t, [2]int64{100 * rounds, 100 * rounds}, booksTotal(t, w, "general", "usage_receivable"))
		require.Equal(t, [2]int64{100 * rounds, 100 * rounds}, booksCounters(t, w, "usage_receivable"))
		require.Equal(t, [2]int64{0, 0}, booksCounters(t, w, "unapplied_cash"))
		require.Contains(t, hostCLI(t, w, "ledger-audit", "--merchant", "id:"+w.alpha.MerchantID().String()), "Ledger integrity OK")
	})

	t.Run("OR-J40-S23/an-event-queued-before-or-after-a-ledger-transfer-both-commit", func(t *testing.T) {
		w := newWorld(t)
		mid := w.alpha.MerchantID().UUID()
		booksOpen(t, w, mid, "USD")
		first, second := billing.CustomerID(uuid.New()), billing.CustomerID(uuid.New())
		fundBooks(t, w, first, "USD", 1000)
		fundBooks(t, w, second, "USD", 1000)
		a, b := first.UUID(), second.UUID()
		notify := func(ctx context.Context, q *gen.Queries, customer uuid.UUID) error {
			return q.CreateNotificationIfAbsent(ctx, gen.CreateNotificationIfAbsentParams{ID: uuid.New(), MerchantID: mid, CustomerID: customer, EventType: "order_due"})
		}
		// Every row both commits lock exists, the merchant's stream included.
		require.NoError(t, booksTx(t, w, func(ctx context.Context, q *gen.Queries) error {
			if err := booksAccrue(ctx, q, w, mid, a, "open-a"); err != nil {
				return err
			}
			if err := booksAccrue(ctx, q, w, mid, b, "open-b"); err != nil {
				return err
			}
			return notify(ctx, q, a)
		}))
		var events int
		require.NoError(t, w.pool.QueryRow(t.Context(), "SELECT count(*) FROM "+booksTable(w, "eventlog_events")+" WHERE merchant_id=$1", mid).Scan(&events))
		// The first transaction posts, then queues an event (a credit pack
		// bought): held at its commit inside the ledger step.
		release := booksHold(t, w, mid, "usage_revenue")
		posted := booksCommit(t, w, func(ctx context.Context, _ pgx.Tx, q *gen.Queries) error {
			if err := booksAccrue(ctx, q, w, mid, a, "posted-first"); err != nil {
				return err
			}
			return notify(ctx, q, a)
		})
		posted.blocked(t, w, "the transaction that posted first")
		// The second queues an event, then posts (a usage order made due):
		// its commit takes the ledger step before the event streams, so it
		// waits on the first's totals holding no stream.
		queued := booksCommit(t, w, func(ctx context.Context, _ pgx.Tx, q *gen.Queries) error {
			if err := notify(ctx, q, b); err != nil {
				return err
			}
			return booksAccrue(ctx, q, w, mid, b, "queued-first")
		})
		queued.blocked(t, w, "the transaction that queued first")
		release()
		require.NoError(t, posted.wait())
		require.NoError(t, queued.wait())
		var after int
		require.NoError(t, w.pool.QueryRow(t.Context(), "SELECT count(*) FROM "+booksTable(w, "eventlog_events")+" WHERE merchant_id=$1", mid).Scan(&after))
		require.Equal(t, events+2, after, "both events are numbered")
		require.Equal(t, [2]int64{4 * 100, 0}, booksTotal(t, w, "general", "usage_revenue"))
		require.Contains(t, hostCLI(t, w, "ledger-audit", "--merchant", "id:"+w.alpha.MerchantID().String()), "Ledger integrity OK")

		// Ledger work queued once the event streams have flushed would lock out
		// of order: it is refused. (Settling early stands in for a later
		// deferred trigger that posts.)
		err := booksRawTx(t, w, func(ctx context.Context, tx pgx.Tx, q *gen.Queries) error {
			if err := booksAccrue(ctx, q, w, mid, a, "settled-early"); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, "SET CONSTRAINTS "+booksTable(w, "ledger_rollups_settle")+" IMMEDIATE"); err != nil {
				return err
			}
			return booksAccrue(ctx, q, w, mid, a, "after-the-flush")
		})
		require.ErrorContains(t, err, "ledger or MRR work queued after the event log flushed")
	})

	t.Run("OR-J40-S24/one-transaction-posting-for-two-merchants-in-either-order-both-commit", func(t *testing.T) {
		w := newWorld(t)
		merchants := []uuid.UUID{w.alpha.MerchantID().UUID(), w.beta.MerchantID().UUID()}
		slices.SortFunc(merchants, func(x, y uuid.UUID) int { return bytes.Compare(x[:], y[:]) })
		low, high := merchants[0], merchants[1]
		receive := func(ctx context.Context, q *gen.Queries, merchant uuid.UUID, key string) error {
			_, _, err := books.New(q, merchant).Post(ctx, books.Posting{
				Debit: books.Ref{Type: books.ExternalReceipts}, Credit: books.Ref{Type: books.UnappliedCash},
				Amount: 100, Currency: "USD", Type: books.CashReceived, EffectiveAt: w.clock.Now(),
				Coord: books.Coord{Operation: "cash_receipt", Source: "critical", SourceID: key},
			})
			return err
		}
		for _, m := range merchants {
			booksOpen(t, w, m, "USD")
		}
		require.NoError(t, booksTx(t, w, func(ctx context.Context, q *gen.Queries) error {
			if err := receive(ctx, q, low, "open"); err != nil {
				return err
			}
			return receive(ctx, q, high, "open")
		}))
		// The first transaction posts for the lower merchant, then the
		// higher: held at its commit inside the lower merchant's ledger step.
		release := booksHold(t, w, low, "unapplied_cash")
		lowFirst := booksCommit(t, w, func(ctx context.Context, _ pgx.Tx, q *gen.Queries) error {
			if err := receive(ctx, q, low, "low-first"); err != nil {
				return err
			}
			return receive(ctx, q, high, "low-first")
		})
		lowFirst.blocked(t, w, "the transaction that posted for the lower merchant first")
		// The second posts for the higher merchant first; its commit settles
		// the lower merchant first all the same, so it holds nothing the
		// first needs.
		highFirst := booksCommit(t, w, func(ctx context.Context, _ pgx.Tx, q *gen.Queries) error {
			if err := receive(ctx, q, high, "high-first"); err != nil {
				return err
			}
			return receive(ctx, q, low, "high-first")
		})
		highFirst.blocked(t, w, "the transaction that posted for the higher merchant first")
		release()
		require.NoError(t, lowFirst.wait())
		require.NoError(t, highFirst.wait())
		for _, m := range merchants {
			var credits int64
			require.NoError(t, w.pool.QueryRow(t.Context(), "SELECT credits_posted FROM "+booksTable(w, "ledger_type_totals")+" WHERE merchant_id=$1 AND book='general' AND account_type='unapplied_cash'", m).Scan(&credits))
			require.Equal(t, int64(300), credits)
		}
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
	return booksRawTx(t, w, func(ctx context.Context, _ pgx.Tx, q *gen.Queries) error { return fn(ctx, q) })
}

// booksRawTx is booksTx with the transaction, for a statement no query names.
// It reports rather than fails, so a goroutine may run it.
func booksRawTx(t *testing.T, w *world, fn func(ctx context.Context, tx pgx.Tx, q *gen.Queries) error) error {
	database, err := db.NewWithPGXPool(slog.Default(), w.pool, w.schema)
	if err != nil {
		return err
	}
	ctx := merchant.WithID(t.Context(), w.alpha.MerchantID())
	return database.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		return fn(ctx, tx, database.NewWithPgxTx(tx).Gen(ctx))
	})
}

// booksAwait waits for a peer transaction's step; a step that never comes
// means a lock the scenario says nobody holds.
func booksAwait(step <-chan struct{}, what string) error {
	select {
	case <-step:
		return nil
	case <-time.After(15 * time.Second):
		return fmt.Errorf("waited 15s for %s", what)
	}
}

// booksOpen opens the merchant's general ledger in a currency, as the places
// a currency becomes sellable do.
func booksOpen(t *testing.T, w *world, merchant uuid.UUID, currency string) {
	t.Helper()
	database, err := db.NewWithPGXPool(slog.Default(), w.pool, w.schema)
	require.NoError(t, err)
	require.NoError(t, books.Open(t.Context(), database.DataPool(), merchant, currency))
}

// booksAccrue posts 100 of a customer's usage to the general ledger.
func booksAccrue(ctx context.Context, q *gen.Queries, w *world, merchant, customer uuid.UUID, key string) error {
	_, _, err := books.New(q, merchant).Post(ctx, books.Posting{
		Debit: books.Ref{Type: books.UsageReceivable, Customer: &customer}, Credit: books.Ref{Type: books.UsageRevenue},
		Amount: 100, Currency: "USD", Type: books.UsageAccrued, EffectiveAt: w.clock.Now(), Customer: &customer,
		Coord: books.Coord{Operation: "usage_accrual", Source: "critical", SourceID: key},
	})
	return err
}

// booksHold locks, from a session of its own, the merchant's general-ledger
// bucket of one account type, which the ledger step locks after its totals:
// a commit that reaches it waits there holding the totals. release ends the
// hold.
func booksHold(t *testing.T, w *world, merchant uuid.UUID, accountType string) (release func()) {
	t.Helper()
	tx, err := w.pool.Begin(t.Context())
	require.NoError(t, err)
	var held int
	require.NoError(t, tx.QueryRow(t.Context(), "SELECT count(*) FROM (SELECT 1 FROM "+booksTable(w, "ledger_daily")+
		" WHERE merchant_id=$1 AND book='general' AND account_type=$2 FOR UPDATE) held", merchant, accountType).Scan(&held))
	require.Positive(t, held)
	var once sync.Once
	release = func() { once.Do(func() { _ = tx.Rollback(context.Background()) }) }
	t.Cleanup(release)
	return release
}

// booksCommitting is a transaction of alpha's run on its own goroutine.
type booksCommitting struct {
	pid  chan int32
	done chan error
}

// booksCommit runs fn in a transaction of alpha's and commits it, on a
// goroutine of its own.
func booksCommit(t *testing.T, w *world, fn func(ctx context.Context, tx pgx.Tx, q *gen.Queries) error) *booksCommitting {
	c := &booksCommitting{pid: make(chan int32, 1), done: make(chan error, 1)}
	go func() {
		c.done <- booksRawTx(t, w, func(ctx context.Context, tx pgx.Tx, q *gen.Queries) error {
			var pid int32
			if err := tx.QueryRow(ctx, "SELECT pg_backend_pid()").Scan(&pid); err != nil {
				return err
			}
			if err := fn(ctx, tx, q); err != nil {
				return err
			}
			c.pid <- pid
			return nil
		})
	}()
	return c
}

// blocked waits until the transaction, its statements done, waits on a lock
// as it commits.
func (c *booksCommitting) blocked(t *testing.T, w *world, what string) {
	t.Helper()
	var pid int32
	select {
	case pid = <-c.pid:
	case err := <-c.done:
		t.Fatalf("%s ended before it committed: %v", what, err)
	case <-time.After(15 * time.Second):
		t.Fatalf("%s never finished its statements", what)
	}
	require.Eventually(t, func() bool {
		var waiting bool
		err := w.pool.QueryRow(t.Context(), "SELECT COALESCE(wait_event_type = 'Lock', false) FROM pg_stat_activity WHERE pid = $1", pid).Scan(&waiting)
		return err == nil && waiting
	}, 15*time.Second, 20*time.Millisecond, "%s waits on a lock at commit", what)
}

// wait is the transaction's outcome.
func (c *booksCommitting) wait() error {
	select {
	case err := <-c.done:
		return err
	case <-time.After(30 * time.Second):
		return fmt.Errorf("the transaction did not finish")
	}
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

// booksCounters are the credits and debits posted to alpha's USD account of
// one general-ledger type.
func booksCounters(t *testing.T, w *world, accountType string) [2]int64 {
	t.Helper()
	var out [2]int64
	require.NoError(t, w.pool.QueryRow(t.Context(), "SELECT credits_posted, debits_posted FROM "+booksTable(w, "ledger_accounts")+" WHERE merchant_id=$1 AND book='general' AND currency='USD' AND account_type=$2", w.alpha.MerchantID().UUID(), accountType).Scan(&out[0], &out[1]))
	return out
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
			// Before the books: no book, effective_at, order line or product,
			// nor the revenue schedule that came after them.
			require.Len(t, v, len(transfers.Columns))
			v = v[:len(v)-5]
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
