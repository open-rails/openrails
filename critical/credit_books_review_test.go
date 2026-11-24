//go:build integration

package critical_test

import (
	"bytes"
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/modules/money/books"
	"github.com/open-rails/openrails/internal/modules/money/credits"
	"github.com/open-rails/openrails/internal/modules/money/ledger"
	"github.com/open-rails/openrails/internal/modules/payments"
	"github.com/open-rails/openrails/internal/modules/purchasedcredits"
	"github.com/open-rails/openrails/internal/stripemock"
)

// The credit books' edges: restores, recoveries, repayment of owed from
// before the books, refunds in pieces, merchant units and revocation keys.
func TestCreditBooksEdges(t *testing.T) {
	t.Run("OR-J40-S14/an-archive-with-a-reinstated-pack-and-a-pack-from-before-the-books-restores", restoresCreditBooks)
	t.Run("OR-J40-S15/a-won-dispute-repays-again-only-owed-still-outstanding", wonDisputeAfterOwedRepaidAgain)
	t.Run("OR-J40-S16/partial-recoveries-follow-the-units-and-end-exact", partialRecoveriesEndExact)
	t.Run("OR-J40-S17/owed-from-before-the-books-is-repaid-as-revenue-beside-the-receivable", repaymentSplitsAtTheReceivable)
	t.Run("OR-J40-S18/refunds-in-pieces-strand-nothing-and-a-merchant-unit-lot-refunds-in-its-paid-currency", piecedAndUnitRefunds)
	t.Run("OR-J40-S19/a-revocation-retried-under-its-idempotency-key-replays-and-a-lapsed-lot-is-not-reinstated", revocationKeysAndLapsedReinstatement)
	t.Run("OR-J40-S25/a-lot-applied-beyond-its-payments-cash-fails-ledger-audit", overappliedLotFailsAudit)
	t.Run("OR-J40-S26/a-lot-whose-payment-brought-no-cash-defers-nothing-and-raises-a-finding", unfundedLotRaisesAFinding)
}

// A credit lot's issue applies its payment's cash: ledger-audit holds it to
// the same per-payment bound as a bill's lines.
func overappliedLotFailsAudit(t *testing.T) {
	s := newCreditShop(t)
	w := s.a.world
	payment, lot := s.buy(t, s.pack(t, "pack-55", 55_000_000, 50_000_000, 365), 50_000_000, "pack-55")
	booksAudited(t, w)

	// A second application of the lot, as a broken writer would post it.
	customer := s.customer.UUID()
	require.Len(t, lotLegs(t, w, lot, "cash_applied"), 1)
	require.NoError(t, booksTx(t, w, func(ctx context.Context, q *gen.Queries) error {
		_, _, err := books.New(q, w.alpha.MerchantID().UUID()).Post(ctx, books.Posting{
			Debit: books.Ref{Type: books.UnappliedCash}, Credit: books.Ref{Type: books.DeferredRevenueCredit, Customer: &customer},
			Amount: 1_000_000, Currency: "USD", Type: books.CashApplied,
			Coord:       books.Coord{Operation: books.OpCreditIssue, Source: "payment", SourceID: payment.UUID().String()},
			EffectiveAt: w.clock.Now(), Customer: &customer,
		})
		return err
	}))
	output, err := hostCLIResult(t, w, "ledger-audit", "--merchant", "id:"+w.alpha.MerchantID().String())
	require.Error(t, err)
	require.Contains(t, output, "overapplied payment: "+payment.UUID().String())
}

// chargeback opens a Stripe dispute of amount cents on payment and returns
// its reversal.
func (s *creditShop) chargeback(t *testing.T, payment billing.PaymentID, id string, cents int) (stripemock.Object, uuid.UUID) {
	t.Helper()
	m := &mixedSale{a: s.a, p: s.p}
	charge, intent := m.charge(t, payment)
	dispute := stripemock.Object{"id": id, "object": "dispute", "amount": cents, "currency": "usd", "charge": charge, "payment_intent": intent, "status": "needs_response", "reason": "fraudulent"}
	m.notice(t, "evt_"+id, "charge.dispute.created", dispute)
	read, err := s.a.alpha.GetPayment(t.Context(), payment)
	require.NoError(t, err)
	for _, r := range read.Refunds {
		if r.Kind == billing.PaymentChargeback {
			return dispute, r.ID.UUID()
		}
	}
	t.Fatalf("no chargeback of %s", payment)
	return nil, uuid.Nil
}

// recover records a won dispute's partial recovery of a chargeback, as the
// Stripe webhook records a whole one.
func (s *creditShop) recover(t *testing.T, payment billing.PaymentID, chargeback uuid.UUID, amount int64, key string) {
	t.Helper()
	w := s.a.world
	database, err := db.NewWithPGXPool(slog.Default(), w.pool, w.schema)
	require.NoError(t, err)
	ctx := merchant.WithID(t.Context(), w.alpha.MerchantID())
	service := payments.NewPaymentService(database, w.clock)
	original, err := service.GetByID(ctx, payment.UUID())
	require.NoError(t, err)
	kind := payments.ReversalDisputeReversal
	require.NoError(t, service.Create(ctx, &models.Payment{
		ID: uuid.New(), CustomerID: original.CustomerID, OrderID: original.OrderID, RefundedPaymentID: &original.ID, Rail: original.Rail,
		TransactionID: key, Metadata: map[string]any{"reverses_payment_id": chargeback.String()}, Amount: amount, ListAmount: original.ListAmount,
		Currency: original.Currency, ReversalKind: &kind, Status: payments.PaymentStatusSucceededValue, MoneyMovement: models.MoneyMovementRail,
		PurchasedAt: w.clock.Now(), CreatedAt: w.clock.Now(),
	}))
}

// owes gives the customer usage owed beyond its balance.
func (s *creditShop) owes(t *testing.T, amount int64, request string) {
	t.Helper()
	_, err := s.a.alpha.UpdateCustomer(t.Context(), s.customer, billing.UpdateCustomerParams{CreditLimits: []billing.CreditLimit{{Currency: "USD", Amount: 100_000_000}}})
	require.NoError(t, err)
	s.usage(t, amount, request)
}

// lotUnits is what a lot holds.
func lotUnits(t *testing.T, w *world, lot uuid.UUID) int64 {
	t.Helper()
	var units int64
	require.NoError(t, w.pool.QueryRow(t.Context(), "SELECT units_credited - units_debited FROM "+booksTable(w, "credit_grants")+" WHERE merchant_id=$1 AND id=$2",
		w.alpha.MerchantID().UUID(), lot).Scan(&units))
	return units
}

// A restore checks paid lots while it is still restoring: a pack reinstated
// after its revocation deferred more than its price, and a pack from before
// the credit books deferred none, and both restore as they were.
func restoresCreditBooks(t *testing.T) {
	s := newCreditShop(t)
	a, w := s.a, s.a.world
	_, reinstated := s.buy(t, s.pack(t, "pack-55", 55_000_000, 50_000_000, 365), 50_000_000, "pack-55")
	s.spend(t, 11_000_000, "before-the-revocation")
	_, err := a.alpha.RevokeCreditGrant(t.Context(), "", reinstated, billing.RevokeCreditGrantParams{Reason: "abuse"})
	require.NoError(t, err)
	_, err = a.alpha.ReinstateCreditGrant(t.Context(), "", reinstated, billing.ReinstateCreditGrantParams{Reason: "appeal upheld"})
	require.NoError(t, err)
	deferred, released := lotMoney(t, w, reinstated)
	require.EqualValues(t, 90_000_000, deferred, "the forfeit's reversal deferred again")
	require.EqualValues(t, 50_000_000, released)

	// A pack bought before the credit books: its units posted, its price
	// never deferred.
	before := uuid.New()
	lots := booksTable(w, "credit_grants")
	tx, err := w.pool.Begin(t.Context())
	require.NoError(t, err)
	defer func() { _ = tx.Rollback(context.Background()) }()
	_, err = tx.Exec(t.Context(), "ALTER TABLE "+lots+" DISABLE TRIGGER credit_grants_require_books")
	require.NoError(t, err)
	_, err = tx.Exec(t.Context(), "INSERT INTO "+lots+" (merchant_id, id, customer_id, account_id, unit, units, funding_source, paid_amount, paid_currency, source, source_id, invoker, starts_at)"+
		" SELECT merchant_id, $3, customer_id, account_id, unit, 2000000, 'purchase', 2000000, 'USD', 'pack_purchase', 'before-the-books', invoker, starts_at FROM "+lots+" WHERE merchant_id = $1 AND id = $2",
		w.alpha.MerchantID().UUID(), reinstated.UUID(), before)
	require.NoError(t, err)
	_, err = tx.Exec(t.Context(), "ALTER TABLE "+lots+" ENABLE TRIGGER credit_grants_require_books")
	require.NoError(t, err)
	require.NoError(t, tx.Commit(t.Context()))
	require.NoError(t, booksTx(t, w, func(ctx context.Context, q *gen.Queries) error {
		return credits.New(q, w.alpha.MerchantID().UUID(), w.clock.Now).Materialize(ctx, before)
	}))
	deferred, _ = lotMoney(t, w, billing.CreditGrantID(before))
	require.Zero(t, deferred)
	balance(t, a.alpha, s.customer, 46_000_000, 0, 46_000_000)
	booksAudited(t, w)

	archive := archiveBytes(t, a.alpha)
	target, client := archiveTarget(t, w)
	_, err = client.ImportBillingArchive(t.Context(), bytes.NewReader(archive))
	require.NoError(t, err, "the restore's commit accepts both lots")
	balance(t, client, s.customer, 46_000_000, 0, 46_000_000)
	for _, lot := range []billing.CreditGrantID{reinstated, billing.CreditGrantID(before)} {
		sd, sr := lotMoney(t, w, lot)
		var td, tr int64
		require.NoError(t, target.pool.QueryRow(t.Context(), "SELECT deferred_credited, deferred_debited FROM "+booksTable(target, "credit_grants")+" WHERE merchant_id=$1 AND id=$2",
			w.alpha.MerchantID().UUID(), lot.UUID()).Scan(&td, &tr))
		require.Equal(t, [2]int64{sd, sr}, [2]int64{td, tr}, "lot %s keeps its money", lot)
	}
	require.Contains(t, hostCLI(t, target, "ledger-audit", "--merchant", "id:"+w.alpha.MerchantID().String()), "Ledger integrity OK")
}

// A chargeback reopens owed a pack repaid; a gift repays it again. Winning
// the dispute finds nothing owed: the pack's credit returns to the balance
// with its money, and the receivable keeps its sign. The payment refunds
// after.
func wonDisputeAfterOwedRepaidAgain(t *testing.T) {
	s := newCreditShop(t)
	a, w := s.a, s.a.world
	s.owes(t, 11_000_000, "owed-before-the-pack")
	payment, lot := s.buy(t, s.pack(t, "pack-55", 55_000_000, 50_000_000, 365), 50_000_000, "pack-55")
	require.Equal(t, []int64{10_000_000}, lotLegs(t, w, lot, "usage_repaid"))

	dispute, _ := s.chargeback(t, payment, "dp_owed_again", 5_000)
	require.Equal(t, []int64{10_000_000, 1_000_000}, lotLegs(t, w, lot, "usage_reopened"), "the repaid owed is receivable again: $10 of cash, $1 promotional")
	require.EqualValues(t, -11_000_000, glBalance(t, w, "usage_receivable"))
	_, err := a.alpha.CreateCreditGrants(t.Context(), []billing.CreateCreditGrantParams{{CustomerID: s.customer, Currency: "USD", Amount: 11_000_000, Source: "critical", SourceID: "repays-again"}})
	require.NoError(t, err)
	require.Zero(t, glBalance(t, w, "usage_receivable"), "the gift repaid it again")
	balance(t, a.alpha, s.customer, 0, 0, 0)

	dispute["status"] = "won"
	(&mixedSale{a: a, p: s.p}).notice(t, "evt_dp_owed_again_won", "charge.dispute.closed", dispute)
	require.Equal(t, []int64{50_000_000}, lotLegs(t, w, lot, "dispute_won"), "all 55 credits return to the balance, worth $50")
	require.Zero(t, glBalance(t, w, "usage_receivable"))
	balance(t, a.alpha, s.customer, 55_000_000, 0, 55_000_000)

	refund, err := a.alpha.RefundPayment(t.Context(), "after-the-win", payment, billing.RefundPaymentParams{Amount: 10_000_000, Reason: "Partial refund"})
	require.NoError(t, err)
	require.Equal(t, billing.PaymentSucceeded, refund.Status)
	require.Equal(t, []int64{10_000_000}, lotLegs(t, w, lot, "refund_deferred"))
	balance(t, a.alpha, s.customer, 44_000_000, 0, 44_000_000)
	booksAudited(t, w)
}

// A $50 chargeback recovered in two halves: each half's money follows the
// units it restores, so the lot's deferred money is always the worth of its
// units, and the two end exactly where the chargeback began.
func partialRecoveriesEndExact(t *testing.T) {
	t.Run("without-owed", func(t *testing.T) {
		s := newCreditShop(t)
		w := s.a.world
		payment, lot := s.buy(t, s.pack(t, "pack-55", 55_000_000, 50_000_000, 365), 50_000_000, "pack-55")
		s.spend(t, 11_000_000, "before-the-chargeback")
		_, chargeback := s.chargeback(t, payment, "dp_halves", 5_000)
		require.Equal(t, []int64{40_000_000}, lotLegs(t, w, lot, "chargeback_deferred"))
		require.Equal(t, []int64{10_000_000}, lotLegs(t, w, lot, "chargeback_recognized"))

		s.recover(t, payment, chargeback, 25_000_000, "dispute_won:first-half")
		require.EqualValues(t, 27_500_000, lotUnits(t, w, lot.UUID()))
		deferred, released := lotMoney(t, w, lot)
		require.EqualValues(t, 25_000_000, deferred-released, "27.5 credits restored are worth $25")
		s.recover(t, payment, chargeback, 25_000_000, "dispute_won:second-half")
		require.EqualValues(t, 44_000_000, lotUnits(t, w, lot.UUID()))
		require.Equal(t, []int64{25_000_000, 15_000_000, 10_000_000}, lotLegs(t, w, lot, "dispute_won"))
		deferred, released = lotMoney(t, w, lot)
		require.EqualValues(t, 40_000_000, deferred-released)
		require.Zero(t, glBalance(t, w, "chargebacks"))
		booksAudited(t, w)
	})
	t.Run("with-reopened-owed", func(t *testing.T) {
		s := newCreditShop(t)
		w := s.a.world
		s.owes(t, 11_000_000, "owed-before-the-pack")
		payment, lot := s.buy(t, s.pack(t, "pack-55", 55_000_000, 50_000_000, 365), 50_000_000, "pack-55")
		_, chargeback := s.chargeback(t, payment, "dp_owed_halves", 5_000)
		require.EqualValues(t, -11_000_000, glBalance(t, w, "usage_receivable"))

		s.recover(t, payment, chargeback, 25_000_000, "dispute_won:first-half")
		deferred, released := lotMoney(t, w, lot)
		require.EqualValues(t, 25_000_000, deferred-released)
		require.EqualValues(t, -11_000_000, glBalance(t, w, "usage_receivable"), "the first half restores credit, not owed")
		s.recover(t, payment, chargeback, 25_000_000, "dispute_won:second-half")
		require.Equal(t, []int64{25_000_000, 15_000_000, 10_000_000, 1_000_000}, lotLegs(t, w, lot, "dispute_won"))
		deferred, released = lotMoney(t, w, lot)
		require.EqualValues(t, 40_000_000, deferred-released)
		require.Zero(t, glBalance(t, w, "usage_receivable"))
		require.EqualValues(t, -1_000_000, glBalance(t, w, "promotional_credit"), "as the repayment left it")
		require.Zero(t, glBalance(t, w, "chargebacks"))
		balance(t, s.a.alpha, s.customer, 44_000_000, 0, 44_000_000)
		booksAudited(t, w)
	})
}

// owedBeforeTheBooks posts owed as the units book did before the credit
// books: no receivable.
func owedBeforeTheBooks(t *testing.T, w *world, customer billing.CustomerID, amount int64, key string) {
	t.Helper()
	require.NoError(t, booksTx(t, w, func(ctx context.Context, q *gen.Queries) error {
		l := ledger.New(q, w.alpha.MerchantID().UUID())
		owed, err := l.EnsureCustomerArrears(ctx, customer.UUID(), "USD")
		if err != nil {
			return err
		}
		redeemed, err := l.EnsureSystemAccount(ctx, ledger.UnitsRedeemed, "USD")
		if err != nil {
			return err
		}
		c := customer.UUID()
		_, err = l.Apply(ctx, ledger.Transfer{Debit: owed, Credit: redeemed, Amount: amount, Currency: "USD", Type: ledger.OwedAccrual,
			Coord: ledger.Coord{Operation: ledger.OpArrearsAccrual, Source: "critical", SourceID: key}, Customer: &c})
		return err
	}))
}

// Of owed that accrued partly before the books, credit repays the
// receivable a usage posting accrued first and the rest as revenue.
func repaymentSplitsAtTheReceivable(t *testing.T) {
	s := newCreditShop(t)
	a, w := s.a, s.a.world
	_, err := a.alpha.UpdateCustomer(t.Context(), s.customer, billing.UpdateCustomerParams{})
	require.NoError(t, err)
	owedBeforeTheBooks(t, w, s.customer, 2_000_000, "unbooked")
	s.owes(t, 3_000_000, "booked")
	gift, err := a.alpha.CreateCreditGrants(t.Context(), []billing.CreateCreditGrantParams{{CustomerID: s.customer, Currency: "USD", Amount: 5_000_000, Source: "critical", SourceID: "gift"}})
	require.NoError(t, err)
	require.Equal(t, []int64{3_000_000, 2_000_000}, lotLegs(t, w, gift[0].ID, "promotional_credit_applied"))
	require.Zero(t, glBalance(t, w, "usage_receivable"))
	require.EqualValues(t, 5_000_000, glBalance(t, w, "usage_revenue"), "$3 accrued as it posted, $2 recognised as it was repaid")

	owedBeforeTheBooks(t, w, s.customer, 5_500_000, "unbooked-before-the-pack")
	s.usage(t, 5_500_000, "booked-before-the-pack")
	_, pack := s.buy(t, s.pack(t, "pack-55", 55_000_000, 50_000_000, 365), 50_000_000, "pack-55")
	require.Equal(t, []int64{5_000_000}, lotLegs(t, w, pack, "usage_repaid"), "5.5 credits repay the receivable at $5")
	require.Equal(t, []int64{500_000}, lotLegs(t, w, pack, "promotional_credit_applied"))
	require.Equal(t, []int64{5_000_000}, lotLegs(t, w, pack, "credit_released"), "the other 5.5 are revenue at the rate")
	require.Zero(t, glBalance(t, w, "usage_receivable"))
	require.EqualValues(t, 40_000_000, glBalance(t, w, "deferred_revenue_credit"))
	booksAudited(t, w)
}

func piecedAndUnitRefunds(t *testing.T) {
	// $1 for 3 micro-credits, refunded 40, 30 and 30 cents: credits leave
	// whole while cash leaves in pieces, and the deferred money still ends
	// at zero, its excess over a piece's cash back against refunds.
	t.Run("pieces", func(t *testing.T) {
		s := newCreditShop(t)
		w := s.a.world
		payment, lot := s.buy(t, s.pack(t, "tiny", 3, 1_000_000, 365), 1_000_000, "tiny")
		for i, cents := range []int64{400_000, 300_000, 300_000} {
			refund, err := s.a.alpha.RefundPayment(t.Context(), "piece-"+string(rune('a'+i)), payment, billing.RefundPaymentParams{Amount: cents, Reason: "Piece"})
			require.NoError(t, err)
			require.Equal(t, billing.PaymentSucceeded, refund.Status)
		}
		deferred, released := lotMoney(t, w, lot)
		require.EqualValues(t, 1_000_000, deferred)
		require.Equal(t, deferred, released, "no micro is stranded")
		require.Equal(t, []int64{66_667}, lotLegs(t, w, lot, "refund_recognized"))
		require.Zero(t, glBalance(t, w, "refunds"), "the excess came back against the first piece's contra")
		require.Zero(t, glBalance(t, w, "cash_clearing"))
		balance(t, s.a.alpha, s.customer, 0, 0, 0)
		booksAudited(t, w)
	})
	// A lot of a merchant's own unit, 400 GOLD bought for $20 as #1379 sells
	// it: the pack's line is a credit line, so its payment's cash is the lot's
	// alone to apply. It reverses its units in GOLD and its money in USD.
	t.Run("merchant-unit", func(t *testing.T) {
		s := newCreditShop(t)
		w := s.a.world
		payment := s.cardPayment(t, 20_000_000, models.MoneyMovementRail, "gold-pack")
		lot := s.issue(t, "GOLD", 400, 20_000_000, payment)
		require.NoError(t, booksTx(t, w, func(ctx context.Context, q *gen.Queries) error {
			_, err := credits.New(q, w.alpha.MerchantID().UUID(), w.clock.Now).Spend(ctx, s.customer.UUID(), "GOLD", 100, s.customer.String(), "critical",
				ledger.Coord{Operation: ledger.OpCapture, Source: "critical", SourceID: "gold-spend"})
			return err
		}))
		require.Equal(t, []int64{5_000_000}, lotLegs(t, w, billing.CreditGrantID(lot), "credit_released"), "100 GOLD at $0.05")

		s.reverse(t, payment, -10_000_000, payments.ReversalChargeback, "gold-chargeback")
		require.EqualValues(t, 100, lotUnits(t, w, lot), "200 GOLD withdrawn")
		require.Equal(t, []int64{10_000_000}, lotLegs(t, w, billing.CreditGrantID(lot), "chargeback_deferred"), "in dollars")
		var refunded int64
		require.NoError(t, w.pool.QueryRow(t.Context(), "SELECT credits_posted FROM "+booksTable(w, "ledger_type_totals")+" WHERE merchant_id=$1 AND book='units' AND currency='GOLD' AND account_type='units_refunded'",
			w.alpha.MerchantID().UUID()).Scan(&refunded))
		require.EqualValues(t, 200, refunded)
		booksAudited(t, w)
	})
}

// cardPayment stores a succeeded card payment with no order, as a checkout's
// credit line leaves its cash: for the lot it bought alone.
func (s *creditShop) cardPayment(t *testing.T, amount int64, movement models.MoneyMovement, key string) uuid.UUID {
	t.Helper()
	w := s.a.world
	_, err := s.a.alpha.UpdateCustomer(t.Context(), s.customer, billing.UpdateCustomerParams{})
	require.NoError(t, err)
	var psp uuid.UUID
	require.NoError(t, w.pool.QueryRow(t.Context(), "SELECT id FROM "+booksTable(w, "psps")+" WHERE merchant_id=$1 AND rail='stripe' ORDER BY created_at LIMIT 1",
		w.alpha.MerchantID().UUID()).Scan(&psp))
	database, err := db.NewWithPGXPool(slog.Default(), w.pool, w.schema)
	require.NoError(t, err)
	ctx := merchant.WithID(t.Context(), w.alpha.MerchantID())
	id := uuid.New()
	require.NoError(t, payments.NewPaymentService(database, w.clock).Create(ctx, &models.Payment{
		ID: id, CustomerID: s.customer.UUID(), Channel: models.ChannelRail, Rail: models.RailStripe, PspID: &psp, TransactionID: key,
		Amount: amount, ListAmount: amount, Currency: "USD", Status: payments.PaymentStatusSucceededValue, MoneyMovement: movement,
		PurchasedAt: w.clock.Now(), CreatedAt: w.clock.Now(),
	}))
	return id
}

// issue records the lot a payment bought, as purchasedcredits.Fund does.
func (s *creditShop) issue(t *testing.T, unit string, units, paid int64, payment uuid.UUID) uuid.UUID {
	t.Helper()
	w := s.a.world
	var lot uuid.UUID
	require.NoError(t, booksTx(t, w, func(ctx context.Context, q *gen.Queries) error {
		issued, err := credits.New(q, w.alpha.MerchantID().UUID(), w.clock.Now).Issue(ctx, credits.Issue{
			Customer: s.customer.UUID(), Unit: unit, Units: units, Funding: credits.Purchase, PaidAmount: ptr(paid), PaidCurrency: ptr("USD"),
			Source: "payment", SourceID: payment.String(), Invoker: s.customer.String(), Payment: &payment,
		})
		lot = issued.ID
		return err
	}))
	return lot
}

// reverse records a succeeded reversal of payment and applies it to the lot
// the payment bought, as every reversal path does.
func (s *creditShop) reverse(t *testing.T, payment uuid.UUID, amount int64, kind, key string) {
	t.Helper()
	w := s.a.world
	database, err := db.NewWithPGXPool(slog.Default(), w.pool, w.schema)
	require.NoError(t, err)
	ctx := merchant.WithID(t.Context(), w.alpha.MerchantID())
	service := payments.NewPaymentService(database, w.clock)
	original, err := service.GetByID(ctx, payment)
	require.NoError(t, err)
	id := uuid.New()
	require.NoError(t, service.Create(ctx, &models.Payment{
		ID: id, CustomerID: original.CustomerID, RefundedPaymentID: &original.ID, Channel: original.Channel, Rail: original.Rail, PspID: original.PspID,
		TransactionID: key, Amount: amount, ListAmount: original.ListAmount, Currency: original.Currency, ReversalKind: &kind,
		Status: payments.PaymentStatusSucceededValue, MoneyMovement: models.MoneyMovementRail, PurchasedAt: w.clock.Now(), CreatedAt: w.clock.Now(),
	}))
	posted, err := purchasedcredits.New(database, w.clock).ApplyReversal(ctx, payment, id, nil)
	require.NoError(t, err)
	require.True(t, posted)
}

// A lot whose payment brought no cash (a bookkeeping row) defers nothing, raises
// books.credit.unapplied and still commits: a payment is never applied beyond
// its receipt.
func unfundedLotRaisesAFinding(t *testing.T) {
	s := newCreditShop(t)
	w := s.a.world
	payment := s.cardPayment(t, 20_000_000, models.MoneyMovementNone, "bookkeeping")
	lot := s.issue(t, "USD", 20_000_000, 20_000_000, payment)
	require.Empty(t, lotLegs(t, w, billing.CreditGrantID(lot), "cash_applied"))
	deferred, _ := lotMoney(t, w, billing.CreditGrantID(lot))
	require.Zero(t, deferred)
	page, err := s.a.alpha.ListFindings(t.Context(), billing.FindingListParams{Type: "books.credit.unapplied"})
	require.NoError(t, err)
	require.Len(t, page.Items, 1)
	balance(t, s.a.alpha, s.customer, 20_000_000, 0, 20_000_000)
	booksAudited(t, w)
}

func revocationKeysAndLapsedReinstatement(t *testing.T) {
	t.Run("idempotency-key", func(t *testing.T) {
		s := newCreditShop(t)
		a := s.a
		_, lot := s.buy(t, s.pack(t, "pack-55", 55_000_000, 50_000_000, 365), 50_000_000, "pack-55")
		revoked, err := a.alpha.RevokeCreditGrant(t.Context(), "first", lot, billing.RevokeCreditGrantParams{Reason: "abuse"})
		require.NoError(t, err)
		require.Equal(t, billing.CreditGrantRevoked, revoked.State)
		_, err = a.alpha.ReinstateCreditGrant(t.Context(), "", lot, billing.ReinstateCreditGrantParams{Reason: "appeal upheld"})
		require.NoError(t, err)

		// The first revocation retried under its key replays; the lot stays
		// reinstated.
		retried, err := a.alpha.RevokeCreditGrant(t.Context(), "first", lot, billing.RevokeCreditGrantParams{Reason: "abuse"})
		require.NoError(t, err)
		require.True(t, retried.Replayed)
		require.Equal(t, billing.CreditGrantActive, retried.State)
		balance(t, a.alpha, s.customer, 55_000_000, 0, 55_000_000)

		// A new key revokes it again.
		again, err := a.alpha.RevokeCreditGrant(t.Context(), "second", lot, billing.RevokeCreditGrantParams{Reason: "abuse"})
		require.NoError(t, err)
		require.False(t, again.Replayed)
		require.Equal(t, billing.CreditGrantRevoked, again.State)
		require.Len(t, lotLegs(t, a.world, lot, "credit_forfeited"), 2)

		// The first reinstatement retried under its key after that revocation
		// replays; it does not undo it.
		_, err = a.alpha.ReinstateCreditGrant(t.Context(), "first-reinstate", lot, billing.ReinstateCreditGrantParams{Reason: "appeal upheld"})
		require.NoError(t, err)
		_, err = a.alpha.RevokeCreditGrant(t.Context(), "third", lot, billing.RevokeCreditGrantParams{Reason: "abuse"})
		require.NoError(t, err)
		stale, err := a.alpha.ReinstateCreditGrant(t.Context(), "first-reinstate", lot, billing.ReinstateCreditGrantParams{Reason: "appeal upheld"})
		require.NoError(t, err)
		require.True(t, stale.Replayed)
		require.Equal(t, billing.CreditGrantRevoked, stale.State)
		balance(t, a.alpha, s.customer, 0, 0, 0)
	})
	t.Run("idempotency-key-after-the-lot-is-spent", func(t *testing.T) {
		s := newCreditShop(t)
		a := s.a
		_, lot := s.buy(t, s.pack(t, "pack-55", 55_000_000, 50_000_000, 365), 50_000_000, "pack-55")
		_, err := a.alpha.RevokeCreditGrant(t.Context(), "spent-later", lot, billing.RevokeCreditGrantParams{Reason: "abuse"})
		require.NoError(t, err)
		_, err = a.alpha.ReinstateCreditGrant(t.Context(), "", lot, billing.ReinstateCreditGrantParams{Reason: "appeal upheld"})
		require.NoError(t, err)
		s.spend(t, 55_000_000, "all-of-it")
		retried, err := a.alpha.RevokeCreditGrant(t.Context(), "spent-later", lot, billing.RevokeCreditGrantParams{Reason: "abuse"})
		require.NoError(t, err, "a retry under its key replays before the lot's remainder is read")
		require.True(t, retried.Replayed)
	})
	t.Run("lapsed", func(t *testing.T) {
		s := newCreditShop(t)
		a := s.a
		_, lot := s.buy(t, s.pack(t, "pack-11", 11_000_000, 10_000_000, 1), 10_000_000, "pack-11")
		_, err := a.alpha.RevokeCreditGrant(t.Context(), "", lot, billing.RevokeCreditGrantParams{Reason: "abuse"})
		require.NoError(t, err)
		a.clock.Advance(49 * time.Hour)
		_, err = a.alpha.ReinstateCreditGrant(t.Context(), "", lot, billing.ReinstateCreditGrantParams{Reason: "too late"})
		var refusal *billing.StatusError
		require.ErrorAs(t, err, &refusal)
		require.Equal(t, billing.CodeCreditGrantUnavailable, refusal.Code)
		require.Empty(t, lotLegs(t, a.world, lot, "credit_forfeit_reversed"))
		read, err := a.alpha.GetCreditGrant(t.Context(), lot)
		require.NoError(t, err)
		require.Equal(t, billing.CreditGrantRevoked, read.State)
	})
}
