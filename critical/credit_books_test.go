//go:build integration

package critical_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/catalog"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/modules/money/credits"
	"github.com/open-rails/openrails/internal/modules/money/ledger"
	"github.com/open-rails/openrails/internal/stripemock"
)

// A credit lot's money in the general ledger: a paid lot defers its price
// and releases it at its rate as its units move; a gift grosses usage up
// against promotional credit. Amounts are micros of USD.
func TestCreditBooks(t *testing.T) {
	t.Run("OR-J40-S10/a-pack-releases-its-price-at-its-rate-and-the-last-credit-the-remainder", packReleasesAtItsRate)
	t.Run("OR-J40-S11/expiry-and-revocation-are-breakage-and-reinstatement-reverses-it", creditBreakageAndReinstatement)
	t.Run("OR-J40-S12/a-gift-spend-grosses-up-and-owed-is-repaid-at-face", giftSpendAndRepaidOwed)
	t.Run("OR-J40-S13/refunds-of-unspent-and-spent-credit-and-a-won-dispute", creditRefundsAndWonDispute)
}

// creditShop sells packs of USD credit by card.
type creditShop struct {
	a        *authorityWorld
	p        *providerLedger
	customer billing.CustomerID
	fleet    *river.Client[pgx.Tx]
}

func newCreditShop(t *testing.T, configure ...func(*world)) *creditShop {
	t.Helper()
	a, p := newPaymentWorld(t, configure...)
	meterForCapture(t, a.world)
	return &creditShop{a: a, p: p, customer: billing.CustomerID(uuid.MustParse(a.alice.ID))}
}

// pack is a product granting credits for a price.
func (s *creditShop) pack(t *testing.T, key string, credits, price int64, days int) billing.PriceID {
	t.Helper()
	product, err := s.a.alpha.CreateProduct(t.Context(), billing.CreateProductParams{Key: key, DisplayName: key, Ownership: catalog.OwnershipConsumable,
		CreditGrant: &catalog.CreditGrantSpec{Currency: "USD", Amount: ptr(credits), ExpiresAfterDays: ptr(days)}})
	require.NoError(t, err)
	created, err := s.a.alpha.CreatePrice(t.Context(), billing.CreatePriceParams{ProductID: product.ID, Key: key, UnitAmount: price, Currency: "USD"})
	require.NoError(t, err)
	return created.ID
}

// buy pays for one pack by card and returns its payment and its lot.
func (s *creditShop) buy(t *testing.T, price billing.PriceID, total int64, key string) (billing.PaymentID, billing.CreditGrantID) {
	t.Helper()
	input := billing.CreateMyInvoiceParams{Lines: []billing.InvoiceLineParams{{PriceID: price}}, ExpectedTotal: ptr(total), Reusable: ptr(true),
		Payment: &billing.InvoicePaymentParams{Token: s.p.core.NewPaymentMethod(stripemock.Card{Brand: "visa", Last4: "4242"})}}
	status, raw := s.a.request(t, http.MethodPost, "/v1/me/invoices", s.a.aliceToken, input, map[string]string{"Idempotency-Key": key})
	require.Equal(t, http.StatusCreated, status, string(raw))
	var invoice billing.Invoice
	require.NoError(t, json.Unmarshal(raw, &invoice))
	require.Equal(t, billing.InvoiceComplete, invoice.Status)
	require.NotNil(t, invoice.Payment.PaymentID)
	return *invoice.Payment.PaymentID, paymentLot(t, s.a.alpha, s.customer, *invoice.Payment.PaymentID)
}

// paymentLot is the lot a payment bought.
func paymentLot(t *testing.T, client *openrails.Client, customer billing.CustomerID, payment billing.PaymentID) billing.CreditGrantID {
	t.Helper()
	lots, err := client.ListCreditGrants(t.Context(), billing.CreditGrantListParams{CustomerID: customer, Currency: "USD"})
	require.NoError(t, err)
	for _, lot := range lots.Items {
		if lot.SourceID != nil && *lot.SourceID == payment.String() {
			return lot.ID
		}
	}
	t.Fatalf("no lot for payment %s", payment)
	return billing.CreditGrantID{}
}

// spend draws the customer's lots as every spend does (credits.Lots.Spend),
// soonest expiry first, at its own coordinate.
func (s *creditShop) spend(t *testing.T, amount int64, key string) {
	t.Helper()
	w := s.a.world
	require.NoError(t, booksTx(t, w, func(ctx context.Context, q *gen.Queries) error {
		_, err := credits.New(q, w.alpha.MerchantID().UUID(), w.clock.Now).Spend(ctx, s.customer.UUID(), "USD", amount, s.customer.String(), "critical",
			ledger.Coord{Operation: ledger.OpCapture, Source: "critical", SourceID: key})
		return err
	}))
}

// usage settles usage on the customer's credit line and posts it: its day
// ends and a billing pass takes it.
func (s *creditShop) usage(t *testing.T, amount int64, request string) {
	t.Helper()
	spend(t, s.a.alpha, s.customer, request, amount)
	if s.fleet == nil {
		s.fleet = quietFleet(t, s.a.world)
	}
	nextDay(s.a.world)
	require.False(t, runPass(t, s.fleet))
}

// lotMoney is a lot's money deferred and released.
func lotMoney(t *testing.T, w *world, lot billing.CreditGrantID) (deferred, released int64) {
	t.Helper()
	require.NoError(t, w.pool.QueryRow(t.Context(), "SELECT deferred_credited, deferred_debited FROM "+booksTable(w, "credit_grants")+" WHERE merchant_id=$1 AND id=$2",
		w.alpha.MerchantID().UUID(), lot.UUID()).Scan(&deferred, &released))
	return deferred, released
}

// lotLegs is the amounts of a lot's general-ledger legs of one type, in the
// order they posted.
func lotLegs(t *testing.T, w *world, lot billing.CreditGrantID, typ string) []int64 {
	t.Helper()
	rows, err := w.pool.Query(t.Context(), "SELECT amount FROM "+booksTable(w, "ledger_transfers")+" WHERE merchant_id=$1 AND credit_grant_id=$2 AND book='general' AND transfer_type=$3 ORDER BY created_at, id",
		w.alpha.MerchantID().UUID(), lot.UUID(), typ)
	require.NoError(t, err)
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var amount int64
		require.NoError(t, rows.Scan(&amount))
		out = append(out, amount)
	}
	require.NoError(t, rows.Err())
	return out
}

// glBalance is a general-ledger account type's credits less debits.
func glBalance(t *testing.T, w *world, accountType string) int64 {
	t.Helper()
	total := booksTotal(t, w, "general", accountType)
	return total[0] - total[1]
}

func booksAudited(t *testing.T, w *world) {
	t.Helper()
	require.Contains(t, hostCLI(t, w, "ledger-audit", "--merchant", "id:"+w.alpha.MerchantID().String()), "Ledger integrity OK")
}

// $50 buys 55 credits: each credit is worth $50/55. Releases are cumulative
// at that rate, so 11 credits release exactly $10 and the last credit what
// rounding left.
func packReleasesAtItsRate(t *testing.T) {
	s := newCreditShop(t)
	w := s.a.world
	payment, lot := s.buy(t, s.pack(t, "pack-55", 55_000_000, 50_000_000, 365), 50_000_000, "pack-55")
	balance(t, s.a.alpha, s.customer, 55_000_000, 0, 55_000_000)
	read, err := s.a.alpha.GetCreditGrant(t.Context(), lot)
	require.NoError(t, err)
	require.EqualValues(t, 50_000_000, *read.PaidAmount)

	// Its price is deferred from the payment's cash, received unapplied.
	require.Equal(t, []int64{50_000_000}, lotLegs(t, w, lot, "cash_applied"))
	deferred, released := lotMoney(t, w, lot)
	require.EqualValues(t, 50_000_000, deferred)
	require.Zero(t, released)
	require.EqualValues(t, 50_000_000, glBalance(t, w, "deferred_revenue_credit"))
	require.Zero(t, glBalance(t, w, "unapplied_cash"), "the payment bought only the pack")
	require.EqualValues(t, -50_000_000, glBalance(t, w, "cash_clearing"), "the card's cash sits in its PSP's clearing account")
	var units int
	require.NoError(t, w.pool.QueryRow(t.Context(), "SELECT count(*) FROM "+booksTable(w, "ledger_transfers")+
		" WHERE merchant_id=$1 AND credit_grant_id=$2 AND book='units' AND transfer_type IN ('deposit_bonus','credit_purchase_revenue')", w.alpha.MerchantID().UUID(), lot.UUID()).Scan(&units))
	require.Zero(t, units, "the bonus is part of the rate, not a units-book leg")
	var cashPayment string
	require.NoError(t, w.pool.QueryRow(t.Context(), "SELECT source_id FROM "+booksTable(w, "ledger_transfers")+
		" WHERE merchant_id=$1 AND book='general' AND transfer_type='cash_received'", w.alpha.MerchantID().UUID()).Scan(&cashPayment))
	require.Equal(t, payment.UUID().String(), cashPayment)

	// One credit, then ten: 909,090 then 9,090,910, exactly $10 in all.
	s.spend(t, 1_000_000, "one")
	s.spend(t, 10_000_000, "ten")
	require.Equal(t, []int64{909_090, 9_090_910}, lotLegs(t, w, lot, "credit_released"))
	_, released = lotMoney(t, w, lot)
	require.EqualValues(t, 10_000_000, released, "11 credits release exactly $10")
	require.EqualValues(t, 10_000_000, glBalance(t, w, "usage_revenue"))

	// 43 more, then the last: it releases what rounding left, one micro
	// more than the first.
	s.spend(t, 43_000_000, "forty-three")
	s.spend(t, 1_000_000, "last")
	require.Equal(t, []int64{909_090, 9_090_910, 39_090_909, 909_091}, lotLegs(t, w, lot, "credit_released"))
	deferred, released = lotMoney(t, w, lot)
	require.Equal(t, deferred, released, "a spent lot has released its whole price")
	require.EqualValues(t, 50_000_000, glBalance(t, w, "usage_revenue"))
	require.Zero(t, glBalance(t, w, "deferred_revenue_credit"))
	balance(t, s.a.alpha, s.customer, 0, 0, 0)
	booksAudited(t, w)
}

// A lapsed pack's unreleased money is breakage; a revoked pack's is
// forfeited to breakage, and reinstating it reverses the forfeit.
func creditBreakageAndReinstatement(t *testing.T) {
	// Begun in the past: the 49 hours it moves on end before now, where a
	// payment may be taken.
	s := newCreditShop(t, func(w *world) { w.clock.Advance(-50 * time.Hour) })
	a, w := s.a, s.a.world
	_, short := s.buy(t, s.pack(t, "pack-11", 11_000_000, 10_000_000, 1), 10_000_000, "pack-11")
	s.spend(t, 1_000_000, "before-the-lapse")
	require.Equal(t, []int64{909_090}, lotLegs(t, w, short, "credit_released"))
	gift, err := a.alpha.CreateCreditGrants(t.Context(), []billing.CreateCreditGrantParams{{CustomerID: s.customer, Currency: "USD", Amount: 5_000_000,
		Source: "critical", SourceID: "lapsing-gift", ExpiresAt: ptr(a.clock.Now().Add(time.Hour))}})
	require.NoError(t, err)

	// Both lapse and are expired, as the balance watch and admission do. The
	// pack's 10 credits left are worth $9.090910; the gift deferred nothing.
	a.clock.Advance(49 * time.Hour)
	require.NoError(t, booksTx(t, w, func(ctx context.Context, q *gen.Queries) error {
		expired, err := credits.New(q, w.alpha.MerchantID().UUID(), w.clock.Now).ExpireLapsed(ctx, s.customer.UUID(), "USD")
		require.EqualValues(t, 15_000_000, expired)
		return err
	}))
	require.Equal(t, []int64{9_090_910}, lotLegs(t, w, short, "credit_breakage"))
	require.Empty(t, lotLegs(t, w, gift[0].ID, "credit_breakage"), "a gift has no money to break")
	deferred, released := lotMoney(t, w, short)
	require.Equal(t, deferred, released)
	require.EqualValues(t, 9_090_910, glBalance(t, w, "breakage_revenue"))

	// A revoked pack forfeits what it had not released.
	_, long := s.buy(t, s.pack(t, "pack-55", 55_000_000, 50_000_000, 365), 50_000_000, "pack-55")
	s.spend(t, 11_000_000, "before-the-revocation")
	revoked, err := a.alpha.RevokeCreditGrant(t.Context(), "", long, billing.RevokeCreditGrantParams{Reason: "abuse"})
	require.NoError(t, err)
	require.Equal(t, billing.CreditGrantRevoked, revoked.State)
	require.Equal(t, []int64{40_000_000}, lotLegs(t, w, long, "credit_forfeited"))
	require.EqualValues(t, 49_090_910, glBalance(t, w, "breakage_revenue"))
	balance(t, a.alpha, s.customer, 0, 0, 0)

	// Reinstating it returns its 44 credits and their $40 to deferral.
	reinstated, err := a.alpha.ReinstateCreditGrant(t.Context(), "", long, billing.ReinstateCreditGrantParams{Reason: "appeal upheld"})
	require.NoError(t, err)
	require.Equal(t, billing.CreditGrantActive, reinstated.State)
	require.EqualValues(t, 44_000_000, reinstated.RemainingAmount)
	require.Equal(t, []int64{40_000_000}, lotLegs(t, w, long, "credit_forfeit_reversed"))
	deferred, released = lotMoney(t, w, long)
	require.EqualValues(t, 40_000_000, deferred-released)
	require.EqualValues(t, 9_090_910, glBalance(t, w, "breakage_revenue"))
	balance(t, a.alpha, s.customer, 44_000_000, 0, 44_000_000)
	again, err := a.alpha.ReinstateCreditGrant(t.Context(), "", long, billing.ReinstateCreditGrantParams{Reason: "appeal upheld"})
	require.NoError(t, err)
	require.True(t, again.Replayed, "reinstating a live grant replays")

	// It spends at its rate again, and can be revoked again.
	s.spend(t, 11_000_000, "after-the-reinstatement")
	require.Equal(t, []int64{10_000_000, 10_000_000}, lotLegs(t, w, long, "credit_released"))
	_, err = a.alpha.RevokeCreditGrant(t.Context(), "", long, billing.RevokeCreditGrantParams{Reason: "abuse again"})
	require.NoError(t, err)
	require.Equal(t, []int64{40_000_000, 30_000_000}, lotLegs(t, w, long, "credit_forfeited"))
	require.EqualValues(t, 39_090_910, glBalance(t, w, "breakage_revenue"))
	require.Zero(t, glBalance(t, w, "deferred_revenue_credit"))
	booksAudited(t, w)
}

// Usage owed beyond the balance is usage revenue receivable. A gift repays
// it at face against promotional credit, and a gift's spend grosses usage up
// the same way; a pack's bonus repays at face too, its money at its rate and
// promotional credit the rest.
func giftSpendAndRepaidOwed(t *testing.T) {
	s := newCreditShop(t)
	a, w := s.a, s.a.world
	_, err := a.alpha.UpdateCustomer(t.Context(), s.customer, billing.UpdateCustomerParams{CreditLimits: []billing.CreditLimit{{Currency: "USD", Amount: 100_000_000}}})
	require.NoError(t, err)

	// $3 owed, then a $5 gift: the usage posted owed, and the gift repays
	// it.
	s.usage(t, 3_000_000, "owed-before-the-gift")
	gift, err := a.alpha.CreateCreditGrants(t.Context(), []billing.CreateCreditGrantParams{{CustomerID: s.customer, Currency: "USD", Amount: 5_000_000, Source: "critical", SourceID: "goodwill"}})
	require.NoError(t, err)
	require.EqualValues(t, 3_000_000, booksTotal(t, w, "general", "usage_receivable")[1], "the owed usage accrued receivable")
	require.Equal(t, []int64{3_000_000}, lotLegs(t, w, gift[0].ID, "promotional_credit_applied"), "the gift repaid it at face")
	require.Zero(t, glBalance(t, w, "usage_receivable"))
	require.EqualValues(t, 3_000_000, glBalance(t, w, "usage_revenue"))
	balance(t, a.alpha, s.customer, 2_000_000, 0, 2_000_000)

	// $13 of usage: the gift's $2 pays part, grossed up; $11 is owed. The
	// pack bought next repays the $11 at face: $10 of its money for 11
	// credits, $1 promotional.
	s.usage(t, 13_000_000, "beyond-the-gift")
	_, pack := s.buy(t, s.pack(t, "pack-55", 55_000_000, 50_000_000, 365), 50_000_000, "pack-55")
	require.Equal(t, []int64{3_000_000, 2_000_000}, lotLegs(t, w, gift[0].ID, "promotional_credit_applied"), "the gift's spend grossed usage up")
	require.Equal(t, []int64{10_000_000}, lotLegs(t, w, pack, "usage_repaid"))
	require.Equal(t, []int64{1_000_000}, lotLegs(t, w, pack, "promotional_credit_applied"))
	require.Empty(t, lotLegs(t, w, pack, "credit_released"))
	deferred, released := lotMoney(t, w, pack)
	require.EqualValues(t, 50_000_000, deferred)
	require.EqualValues(t, 10_000_000, released)
	balance(t, a.alpha, s.customer, 44_000_000, 0, 44_000_000)

	require.Zero(t, glBalance(t, w, "usage_receivable"), "the receivable is repaid")
	require.EqualValues(t, [2]int64{14_000_000, 14_000_000}, booksTotal(t, w, "general", "usage_receivable"))
	require.EqualValues(t, 16_000_000, glBalance(t, w, "usage_revenue"), "$14 accrued and the gift's $2 grossed up")
	require.EqualValues(t, -6_000_000, glBalance(t, w, "promotional_credit"))
	require.EqualValues(t, 40_000_000, glBalance(t, w, "deferred_revenue_credit"))
	booksAudited(t, w)
}

// A $25 refund of a $50 pack withdraws its unspent half and their deferred
// money; once the rest is spent, a $25 chargeback takes consumed credit,
// contra-revenue; winning the dispute reverses it.
func creditRefundsAndWonDispute(t *testing.T) {
	s := newCreditShop(t)
	a, w := s.a, s.a.world
	payment, lot := s.buy(t, s.pack(t, "pack-55", 55_000_000, 50_000_000, 365), 50_000_000, "pack-55")
	s.spend(t, 11_000_000, "before-the-refund")

	refund, err := a.alpha.RefundPayment(t.Context(), "half-back", payment, billing.RefundPaymentParams{Amount: 25_000_000, Reason: "Half back"})
	require.NoError(t, err)
	require.Equal(t, billing.PaymentSucceeded, refund.Status)
	balance(t, a.alpha, s.customer, 16_500_000, 0, 16_500_000)
	require.Equal(t, []int64{25_000_000}, lotLegs(t, w, lot, "refund_deferred"), "27.5 unspent credits were worth $25")
	require.Empty(t, lotLegs(t, w, lot, "refund_recognized"))
	var units, cashLegs int
	require.NoError(t, w.pool.QueryRow(t.Context(), "SELECT count(*) FILTER (WHERE book='units' AND transfer_type='credit_refund' AND amount=27500000),"+
		" count(*) FILTER (WHERE book='units' AND transfer_type IN ('credit_refund_cash','credit_refund_cash_restore'))"+
		" FROM "+booksTable(w, "ledger_transfers")+" WHERE merchant_id=$1 AND credit_grant_id=$2", w.alpha.MerchantID().UUID(), lot.UUID()).Scan(&units, &cashLegs))
	require.Equal(t, 1, units, "the withdrawn credits go to units_refunded")
	require.Zero(t, cashLegs, "the refund's cash is the general ledger's")
	require.EqualValues(t, [2]int64{27_500_000, 0}, booksTotal(t, w, "units", "units_refunded"))

	// The rest is spent: 16.5 credits release the $15 left.
	s.spend(t, 16_500_000, "the-rest")
	require.Equal(t, []int64{10_000_000, 15_000_000}, lotLegs(t, w, lot, "credit_released"))

	// A $25 chargeback finds only consumed credit.
	charge, intent := (&mixedSale{a: a, p: s.p}).charge(t, payment)
	dispute := stripemock.Object{"id": "dp_credit_books", "object": "dispute", "amount": 2500, "currency": "usd", "charge": charge, "payment_intent": intent, "status": "needs_response", "reason": "fraudulent"}
	m := &mixedSale{a: a, p: s.p}
	m.notice(t, "evt_credit_books_dispute", "charge.dispute.created", dispute)
	require.Equal(t, []int64{25_000_000}, lotLegs(t, w, lot, "chargeback_recognized"))
	require.Empty(t, lotLegs(t, w, lot, "chargeback_deferred"))
	require.EqualValues(t, -25_000_000, glBalance(t, w, "chargebacks"))
	deferred, released := lotMoney(t, w, lot)
	require.Equal(t, deferred, released)

	// Won: the contra-revenue is reversed and the consumed credit returns to
	// where it was.
	dispute["status"] = "won"
	m.notice(t, "evt_credit_books_dispute_won", "charge.dispute.closed", dispute)
	require.Equal(t, []int64{25_000_000}, lotLegs(t, w, lot, "dispute_won"))
	require.Zero(t, glBalance(t, w, "chargebacks"))
	require.EqualValues(t, -25_000_000, glBalance(t, w, "cash_clearing"), "the half refunded is gone; the disputed half came back")
	require.Zero(t, glBalance(t, w, "deferred_revenue_credit"))
	require.EqualValues(t, 25_000_000, glBalance(t, w, "usage_revenue"))
	require.EqualValues(t, [2]int64{55_000_000, 27_500_000}, booksTotal(t, w, "units", "units_refunded"))
	balance(t, a.alpha, s.customer, 0, 0, 0)
	booksAudited(t, w)
}
