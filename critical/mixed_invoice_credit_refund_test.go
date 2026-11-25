//go:build integration

package critical_test

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/catalog"
	"github.com/open-rails/openrails/internal/stripemock"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"
	"github.com/stretchr/testify/require"
)

// An invoice buys a $10 pack of 25 credits beside a $90 toolkit. A reversal of
// its $100 payment reverses the credit line first, up to the line's $10; only
// that part touches the credit lot.
func TestMixedInvoiceCreditRefunds(t *testing.T) {
	t.Run("OR-J22-S07/pending-credit-line-refund-holds-the-whole-lot", mixedPendingRefund)
	t.Run("OR-J22-S08/refunds-and-chargebacks-reverse-the-credit-line-first", mixedReversals)
	t.Run("OR-J22-S09/won-dispute-credit-stays-when-a-refund-found-none", mixedDisputeAroundRefund)
}

type mixedSale struct {
	a        *authorityWorld
	p        *providerLedger
	customer billing.CustomerID
	lines    []billing.InvoiceLineParams
}

func newMixedSale(t *testing.T, configure ...func(*world)) *mixedSale {
	t.Helper()
	a, p := newPaymentWorld(t, configure...)
	pack, err := a.alpha.CreateProduct(t.Context(), billing.CreateProductParams{Key: "credit-pack", DisplayName: "Credit pack", Ownership: catalog.OwnershipConsumable,
		CreditGrant: &catalog.CreditGrantSpec{Currency: "USD", Amount: ptr(int64(25_000_000)), ExpiresAfterDays: ptr(365)}})
	require.NoError(t, err)
	packPrice, err := a.alpha.CreatePrice(t.Context(), billing.CreatePriceParams{ProductID: pack.ID, Key: "pack", UnitAmount: 10_000_000, Currency: "USD"})
	require.NoError(t, err)
	toolkit, err := a.alpha.CreateProduct(t.Context(), billing.CreateProductParams{Key: "toolkit", DisplayName: "Toolkit", Ownership: catalog.OwnershipConsumable, Entitlements: []string{"toolkit:owned"}})
	require.NoError(t, err)
	toolkitPrice, err := a.alpha.CreatePrice(t.Context(), billing.CreatePriceParams{ProductID: toolkit.ID, Key: "toolkit", UnitAmount: 90_000_000, Currency: "USD"})
	require.NoError(t, err)
	return &mixedSale{a: a, p: p, customer: billing.CustomerID(uuid.MustParse(a.alice.ID)), lines: []billing.InvoiceLineParams{{PriceID: packPrice.ID}, {PriceID: toolkitPrice.ID}}}
}

func (m *mixedSale) buy(t *testing.T, key string) billing.PaymentID {
	t.Helper()
	input := billing.CreateMyInvoiceParams{Lines: m.lines, ExpectedTotal: ptr(int64(100_000_000)), Reusable: ptr(true),
		Payment: &billing.InvoicePaymentParams{Token: m.p.core.NewPaymentMethod(stripemock.Card{Brand: "visa", Last4: "4242"})}}
	status, raw := m.a.request(t, http.MethodPost, "/v1/me/invoices", m.a.aliceToken, input, map[string]string{"Idempotency-Key": key})
	require.Equal(t, http.StatusCreated, status, string(raw))
	var invoice billing.Invoice
	require.NoError(t, json.Unmarshal(raw, &invoice))
	require.Equal(t, billing.InvoiceComplete, invoice.Status)
	require.NotNil(t, invoice.Payment.PaymentID)
	return *invoice.Payment.PaymentID
}

// charge names the provider charge and payment intent of payment.
func (m *mixedSale) charge(t *testing.T, payment billing.PaymentID) (string, string) {
	t.Helper()
	paid, err := m.a.alpha.GetPayment(t.Context(), payment)
	require.NoError(t, err)
	var charge, intent string
	for _, effect := range m.p.effects(t) {
		if effect.Kind == "charge" && (effect.ID == paid.TransactionID || effect.Operation == paid.TransactionID) {
			charge, intent = effect.ID, effect.Operation
		}
	}
	require.NotEmpty(t, charge)
	return charge, intent
}

func (m *mixedSale) notice(t *testing.T, id, kind string, object stripemock.Object) {
	t.Helper()
	body := stripeNotice(t, id, kind, object)
	status, raw := callback(t, m.a, "acct_e2e", body, stripeSignature(body, "whsec_critical_fixture"))
	require.Equal(t, http.StatusOK, status, raw)
}

func mixedPendingRefund(t *testing.T) {
	// Begun an hour ago: the refund settles twenty minutes on, never in the
	// future.
	m := newMixedSale(t, func(w *world) { w.clock.Advance(-time.Hour) })
	a, p := m.a, m.p
	payment := m.buy(t, "mixed-pending-refund")
	balance(t, a.alpha, m.customer, 25_000_000, 0, 25_000_000)
	// Credit without expiry is drawn after the purchased lot.
	_, err := a.alpha.CreateCreditGrants(t.Context(), []billing.CreateCreditGrantParams{{CustomerID: m.customer, Currency: "USD", Amount: 5_000_000, Source: "critical", SourceID: "goodwill"}})
	require.NoError(t, err)
	balance(t, a.alpha, m.customer, 30_000_000, 0, 30_000_000)

	p.mu.Lock()
	p.lostRefundResponse = true
	p.mu.Unlock()
	params := billing.RefundPaymentParams{Amount: 10_000_000, Reason: "Return the credit pack"}
	_, _ = a.alpha.RefundPayment(t.Context(), "credit-pack-return", payment, params)
	refunded := 0
	for _, effect := range p.effects(t) {
		if effect.Kind == "refund" {
			refunded++
			require.EqualValues(t, 1000, effect.Amount)
		}
	}
	require.Equal(t, 1, refunded, "the provider returned the $10")
	read, err := a.alpha.GetPayment(t.Context(), payment)
	require.NoError(t, err)
	require.Zero(t, read.AmountRefunded, "the lost response leaves the refund pending")
	// $10 is the credit line's whole amount: all 25 credits are held.
	balance(t, a.alpha, m.customer, 5_000_000, 25_000_000, 30_000_000)

	compute, err := a.alpha.CreateProduct(t.Context(), billing.CreateProductParams{Key: "compute", DisplayName: "Compute"})
	require.NoError(t, err)
	_, err = a.alpha.SetMeter(t.Context(), "compute", billing.SetMeterParams{
		EventType: "compute", ValueProperty: "units", Aggregation: catalog.AggregationSum,
		RateCard: catalog.Value(billing.MeterRateCardParams{ProductID: compute.ID, Price: catalog.RatePrice{Model: catalog.ModelDynamic, Currency: "USD"}}),
	})
	require.NoError(t, err)
	out := syncLease(t, a.alpha, billing.SyncLeasesParams{LeaseID: newLease(), Views: []billing.ViewParams{computeView(m.customer)}})
	require.EqualValues(t, 5_000_000, lineOf(t, viewOf(t, out, m.customer), catalog.BalanceLimit).Remaining, "held credit is not offered")
	spend(t, a.alpha, m.customer, "unheld", 5_000_000)
	// The usage accrues; a top-up posts it first, while the refund is
	// still pending.
	a.clock.Advance(time.Second)
	_, err = a.alpha.CreateCreditGrants(t.Context(), []billing.CreateCreditGrantParams{{CustomerID: m.customer, Currency: "USD", Amount: 1_000_000, Source: "critical", SourceID: "post-the-usage"}})
	require.NoError(t, err)
	balance(t, a.alpha, m.customer, 1_000_000, 25_000_000, 26_000_000)

	p.mu.Lock()
	p.lostRefundResponse = false
	p.mu.Unlock()
	a.clock.Advance(20 * time.Minute)
	fleet := startFleet(t, a.world)
	jobs, err := fleet.JobList(t.Context(), river.NewJobListParams().Kinds("openrails.provider_operation"))
	require.NoError(t, err)
	for _, job := range jobs.Jobs {
		if job.State != rivertype.JobStateCompleted && job.State != rivertype.JobStateRunning {
			_, err = fleet.JobRetry(t.Context(), job.ID)
			require.NoError(t, err)
		}
	}
	require.Eventually(t, func() bool {
		read, err := a.alpha.GetPayment(t.Context(), payment)
		return err == nil && read.AmountRefunded == 10_000_000
	}, 20*time.Second, 20*time.Millisecond)
	// The spend drew the goodwill credit; the refund withdrew the whole lot.
	balance(t, a.alpha, m.customer, 1_000_000, 0, 1_000_000)
	var losses int
	require.NoError(t, a.pool.QueryRow(t.Context(), "SELECT count(*) FROM "+pgx.Identifier{a.schema, "ledger_transfers"}.Sanitize()+
		" WHERE merchant_id = $1 AND operation = 'purchased_credit_refund' AND source = 'consumed_credit'", a.alpha.MerchantID().UUID()).Scan(&losses))
	require.Zero(t, losses, "no refunded credit was spent")
	lot := paymentLot(t, a.alpha, m.customer, payment)
	require.Equal(t, []int64{10_000_000}, lotLegs(t, a.world, lot, "refund_deferred"), "its 25 unspent credits were worth the line's $10")
	require.Empty(t, lotLegs(t, a.world, lot, "refund_recognized"))
}

func mixedReversals(t *testing.T) {
	m := newMixedSale(t)
	a := m.a

	t.Run("full-refund", func(t *testing.T) {
		payment := m.buy(t, "mixed-full-refund")
		balance(t, a.alpha, m.customer, 25_000_000, 0, 25_000_000)
		refund, err := a.alpha.RefundPayment(t.Context(), "mixed-full-refund", payment, billing.RefundPaymentParams{Full: true, Reason: "Refund the invoice"})
		require.NoError(t, err)
		require.Equal(t, billing.PaymentSucceeded, refund.Status)
		require.EqualValues(t, -100_000_000, refund.Amount)
		balance(t, a.alpha, m.customer, 0, 0, 0)
		charge, err := a.alpha.GetPayment(t.Context(), payment)
		require.NoError(t, err)
		require.Equal(t, billing.PaymentRefunded, charge.Status)
		require.EqualValues(t, 100_000_000, charge.AmountRefunded)
		// The line's $10 leaves deferral; the toolkit's $90 is not the lot's.
		lot := paymentLot(t, a.alpha, m.customer, payment)
		require.Equal(t, []int64{10_000_000}, lotLegs(t, a.world, lot, "cash_applied"))
		require.Equal(t, []int64{10_000_000}, lotLegs(t, a.world, lot, "refund_deferred"))
		deferred, released := lotMoney(t, a.world, lot)
		require.Equal(t, deferred, released)
	})

	t.Run("credit-line-first-then-the-rest", func(t *testing.T) {
		payment := m.buy(t, "mixed-split-refund")
		balance(t, a.alpha, m.customer, 25_000_000, 0, 25_000_000)
		first, err := a.alpha.RefundPayment(t.Context(), "mixed-half", payment, billing.RefundPaymentParams{Amount: 50_000_000, Reason: "Half back"})
		require.NoError(t, err)
		require.EqualValues(t, -50_000_000, first.Amount)
		balance(t, a.alpha, m.customer, 0, 0, 0)
		rest, err := a.alpha.RefundPayment(t.Context(), "mixed-rest", payment, billing.RefundPaymentParams{Full: true, Reason: "The rest"})
		require.NoError(t, err)
		require.EqualValues(t, -50_000_000, rest.Amount, "the rest is the toolkit's and needs no credit")
		balance(t, a.alpha, m.customer, 0, 0, 0)
		charge, err := a.alpha.GetPayment(t.Context(), payment)
		require.NoError(t, err)
		require.EqualValues(t, 100_000_000, charge.AmountRefunded)
	})

	t.Run("chargeback-then-won-dispute", func(t *testing.T) {
		payment := m.buy(t, "mixed-chargeback")
		balance(t, a.alpha, m.customer, 25_000_000, 0, 25_000_000)
		charge, intent := m.charge(t, payment)
		dispute := stripemock.Object{"id": "dp_mixed", "object": "dispute", "amount": 10_000, "currency": "usd", "charge": charge, "payment_intent": intent, "status": "needs_response", "reason": "fraudulent"}
		m.notice(t, "evt_mixed_dispute", "charge.dispute.created", dispute)
		read, err := a.alpha.GetPayment(t.Context(), payment)
		require.NoError(t, err)
		require.Len(t, read.Refunds, 1, "the chargeback is recorded")
		require.Equal(t, billing.PaymentChargeback, read.Refunds[0].Kind)
		require.EqualValues(t, -100_000_000, read.Refunds[0].Amount)
		balance(t, a.alpha, m.customer, 0, 0, 0)

		dispute["status"] = "won"
		m.notice(t, "evt_mixed_dispute_won", "charge.dispute.closed", dispute)
		read, err = a.alpha.GetPayment(t.Context(), payment)
		require.NoError(t, err)
		require.Len(t, read.Refunds, 2, "the won dispute is recorded")
		// The recovered $100 restores what the chargeback took from the lot.
		balance(t, a.alpha, m.customer, 25_000_000, 0, 25_000_000)
		lot := paymentLot(t, a.alpha, m.customer, payment)
		require.Equal(t, []int64{10_000_000}, lotLegs(t, a.world, lot, "chargeback_deferred"))
		require.Equal(t, []int64{10_000_000}, lotLegs(t, a.world, lot, "dispute_won"), "its money returns to deferral")
		deferred, released := lotMoney(t, a.world, lot)
		require.EqualValues(t, 10_000_000, deferred-released)
	})
}

// A $10 chargeback takes the credit line; a $90 refund then finds none to take
// and refunds the toolkit. Its share was decided when it settled: the credit
// the won dispute restores stays with the customer.
func mixedDisputeAroundRefund(t *testing.T) {
	m := newMixedSale(t)
	a := m.a
	payment := m.buy(t, "mixed-dispute-around-refund")
	balance(t, a.alpha, m.customer, 25_000_000, 0, 25_000_000)
	charge, intent := m.charge(t, payment)
	dispute := stripemock.Object{"id": "dp_credit_line", "object": "dispute", "amount": 1000, "currency": "usd", "charge": charge, "payment_intent": intent, "status": "needs_response", "reason": "fraudulent"}
	m.notice(t, "evt_credit_line_dispute", "charge.dispute.created", dispute)
	balance(t, a.alpha, m.customer, 0, 0, 0)

	refund, err := a.alpha.RefundPayment(t.Context(), "toolkit-return", payment, billing.RefundPaymentParams{Amount: 90_000_000, Reason: "Return the toolkit"})
	require.NoError(t, err)
	require.Equal(t, billing.PaymentSucceeded, refund.Status)
	require.EqualValues(t, -90_000_000, refund.Amount)
	balance(t, a.alpha, m.customer, 0, 0, 0)

	dispute["status"] = "won"
	m.notice(t, "evt_credit_line_dispute_won", "charge.dispute.closed", dispute)
	read, err := a.alpha.GetPayment(t.Context(), payment)
	require.NoError(t, err)
	require.Len(t, read.Refunds, 3, "the chargeback, the refund and the won dispute are recorded")
	balance(t, a.alpha, m.customer, 25_000_000, 0, 25_000_000)
	var withdrawals int
	require.NoError(t, a.pool.QueryRow(t.Context(), "SELECT count(*) FROM "+pgx.Identifier{a.schema, "ledger_transfers"}.Sanitize()+
		" WHERE merchant_id = $1 AND operation = 'purchased_credit_refund' AND source_id = $2", a.alpha.MerchantID().UUID(), refund.ID.UUID().String()).Scan(&withdrawals))
	require.Zero(t, withdrawals, "the refund took none of the restored credit")
	lot := paymentLot(t, a.alpha, m.customer, payment)
	require.Equal(t, []int64{10_000_000}, lotLegs(t, a.world, lot, "chargeback_deferred"))
	require.Equal(t, []int64{10_000_000}, lotLegs(t, a.world, lot, "dispute_won"))
	require.Empty(t, lotLegs(t, a.world, lot, "refund_deferred"))
	deferred, released := lotMoney(t, a.world, lot)
	require.EqualValues(t, 10_000_000, deferred-released, "the restored credit's money is deferred again")
}
