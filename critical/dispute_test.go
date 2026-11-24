//go:build integration

package critical_test

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/catalog"
	"github.com/open-rails/openrails/internal/stripemock"
)

// TestDisputes is journey J46: chargebacks, inquiries and fraud reports.
func TestDisputes(t *testing.T) {
	t.Run("OR-J46-S01/a-chargeback-is-recorded-reversed-and-ends-its-charge", disputeChargeback)
	t.Run("OR-J46-S02/an-inquiry-books-and-cancels-nothing", disputeInquiry)
	t.Run("OR-J46-S03/a-prevented-dispute-is-no-chargeback", disputePrevented)
	t.Run("OR-J46-S04/a-won-dispute-stays-won", disputeWon)
	t.Run("OR-J46-S06/a-ccbill-chargeback-is-recorded", disputeCCBill)
	t.Run("OR-J46-S07/an-early-fraud-warning-refunds-its-charge", disputeFraudWarning)
	t.Run("OR-J46-S08/the-monitor-raises-and-escalates-alerting-findings", disputeMonitor)
	t.Run("OR-J46-S09/mastercard-divides-by-the-previous-months-sales", disputeMastercardLag)
	t.Run("OR-J46-S10/the-program-in-force-is-the-months", disputeProgramDates)
	t.Run("OR-J46-S14/refunds-carry-their-reason-and-count-in-no-ratio", disputeRefundReasons)
}

// disputedMembership is alice's engine-renewed $10 Stripe membership with the
// charge and payment intent that paid it.
type disputedMembership struct {
	a              *authorityWorld
	p              *providerLedger
	sub            *billing.Subscription
	payment        billing.PaymentID
	charge, intent string
}

func newDisputedMembership(t *testing.T) *disputedMembership {
	t.Helper()
	a, p, sub := purchasedMembership(t)
	effects := p.effects(t)
	require.Len(t, effects, 1)
	paid, err := a.alpha.ListPayments(t.Context(), billing.PaymentListParams{SubscriptionID: sub.ID})
	require.NoError(t, err)
	require.Len(t, paid.Items, 1)
	return &disputedMembership{a: a, p: p, sub: sub, payment: paid.Items[0].ID, charge: effects[0].ID, intent: effects[0].Operation}
}

// dispute is a Stripe dispute of the membership's whole charge.
func (m *disputedMembership) dispute(id, status, caseType, reason string) stripemock.Object {
	return stripemock.Object{"id": id, "object": "dispute", "amount": int64(1000), "currency": "usd", "charge": m.charge, "payment_intent": m.intent,
		"status": status, "reason": reason, "created": time.Now().Unix(),
		"payment_method_details": map[string]any{"type": "card", "card": map[string]any{"brand": "visa", "case_type": caseType, "network_reason_code": "10.4"}}}
}

func (m *disputedMembership) notice(t *testing.T, id, kind string, object stripemock.Object) {
	t.Helper()
	body := stripeNotice(t, id, kind, object)
	status, raw := callback(t, m.a, "acct_e2e", body, stripeSignature(body, "whsec_critical_fixture"))
	require.Equal(t, http.StatusOK, status, raw)
}

// reversals are the reversal rows of the membership's charge.
func (m *disputedMembership) reversals(t *testing.T) []billing.Payment {
	t.Helper()
	read, err := m.a.alpha.GetPayment(t.Context(), m.payment)
	require.NoError(t, err)
	return read.Refunds
}

func (m *disputedMembership) entitled(t *testing.T) bool {
	t.Helper()
	access, err := m.a.alpha.CheckEntitlements(t.Context(), billing.CheckEntitlementsParams{CustomerID: m.sub.CustomerID, Entitlements: []string{"membership:paid"}})
	require.NoError(t, err)
	return access.Entitlements["membership:paid"]
}

// An inquiry withdraws nothing: it books no reversal, cancels nothing and
// keeps access, and its closing undoes nothing because nothing was done. An
// inquiry the issuer escalates is a chargeback from that notice, booked once.
// The reconcile pull proposes nothing for an inquiry either.
func disputeInquiry(t *testing.T) {
	t.Run("webhooks", func(t *testing.T) {
		m := newDisputedMembership(t)
		untouched := func(why string) {
			t.Helper()
			require.Empty(t, m.reversals(t), why)
			current, err := m.a.alpha.GetSubscription(t.Context(), m.sub.ID)
			require.NoError(t, err)
			require.Equal(t, "active", string(current.Status), why)
			require.True(t, m.entitled(t), why)
		}

		inquiry := m.dispute("dp_inquiry", "warning_needs_response", "inquiry", "general")
		m.notice(t, "evt_inquiry_created", "charge.dispute.created", inquiry)
		untouched("an inquiry is not a chargeback")
		inquiry["status"] = "warning_under_review"
		m.notice(t, "evt_inquiry_updated", "charge.dispute.updated", inquiry)
		untouched("an inquiry under review is not a chargeback")
		inquiry["status"] = "warning_closed"
		m.notice(t, "evt_inquiry_closed", "charge.dispute.closed", inquiry)
		untouched("a closed inquiry undoes nothing")

		escalated := m.dispute("dp_escalated", "warning_needs_response", "inquiry", "fraudulent")
		m.notice(t, "evt_escalated_created", "charge.dispute.created", escalated)
		untouched("the second inquiry is not a chargeback yet")
		escalated["status"] = "needs_response"
		escalated["payment_method_details"] = map[string]any{"type": "card", "card": map[string]any{"brand": "visa", "case_type": "chargeback", "network_reason_code": "10.4"}}
		m.notice(t, "evt_escalated_updated", "charge.dispute.updated", escalated)
		m.notice(t, "evt_escalated_withdrawn", "charge.dispute.funds_withdrawn", escalated)
		reversals := m.reversals(t)
		require.Len(t, reversals, 1, "the escalation books the chargeback once")
		require.Equal(t, billing.PaymentChargeback, reversals[0].Kind)
		require.EqualValues(t, -10_000_000, reversals[0].Amount)
		canceled := subscriptionState(t, m.a, m.sub.ID, "canceled")
		require.NotNil(t, canceled.CancelType)
		require.Equal(t, "chargeback", *canceled.CancelType)
		require.False(t, m.entitled(t), "a chargeback ends access")
	})

	t.Run("reconcile-pull", func(t *testing.T) {
		iw := newImportWorld(t, "stripe")
		iw.stripe.AddProduct("prod_member", "Member")
		iw.stripe.AddPrice(stripemock.Price{ID: "price_member", Product: "prod_member", Amount: 999, Interval: "month"})
		product, err := iw.alphaRemote.CreateProduct(t.Context(), billing.CreateProductParams{Key: "member", DisplayName: "Member", Entitlements: []string{"member:paid"}})
		require.NoError(t, err)
		_, err = iw.alphaRemote.CreateProviderPrice(t.Context(), billing.CreateProviderPriceParams{ProductID: product.ID, Key: "stripe-member", PSP: "stripe", PriceID: "price_member"})
		require.NoError(t, err)
		start := iw.clock.Now().Add(-10 * 24 * time.Hour)
		customer := iw.stripe.AddCustomer(iw.alice.Email, "Alice", nil)
		sub := iw.stripe.AddSubscription(customer, "pm_alice", "price_member", 999, start, start.AddDate(0, 1, 0))
		since := start.Add(-24 * time.Hour)
		first := iw.pull(t, billing.CreatePSPImportParams{PSP: billing.PSPRef{Key: "stripe"}, HistorySince: &since})
		iw.apply(t, first.ID)
		require.True(t, iw.entitled(t, customerOf(iw.alice), "member:paid"))

		chargebackProposals := func(pull billing.PSPImportID) int {
			n := 0
			for _, f := range iw.proposals(t, pull) {
				if f.Type == "pull.dispute.chargeback" {
					n++
				}
			}
			return n
		}
		iw.stripe.Dispute("dp_pulled", sub, "warning_needs_response", "inquiry", "general")
		require.Zero(t, chargebackProposals(iw.manual(t, "stripe").ID), "an inquiry is no chargeback to propose")
		iw.stripe.Dispute("dp_pulled", sub, "needs_response", "chargeback", "fraudulent")
		require.Equal(t, 1, chargebackProposals(iw.manual(t, "stripe").ID), "the escalated inquiry is")
		require.True(t, iw.entitled(t, customerOf(iw.alice), "member:paid"), "a pull lands nothing")
	})
}

// disputeOf is the one dispute recorded against payment.
func disputeOf(t *testing.T, a *authorityWorld, payment billing.PaymentID) billing.Dispute {
	t.Helper()
	page, err := a.alpha.ListDisputes(t.Context(), billing.DisputeListParams{PaymentID: payment})
	require.NoError(t, err)
	require.Len(t, page.Items, 1)
	return page.Items[0]
}

// cardStatuses are the statuses of customer's saved cards.
func cardStatuses(t *testing.T, a *authorityWorld, customer billing.CustomerID) []string {
	t.Helper()
	rows, err := a.pool.Query(t.Context(), "SELECT status FROM "+pgx.Identifier{a.schema, "payment_methods"}.Sanitize()+" WHERE merchant_id = $1 AND customer_id = $2 ORDER BY id",
		a.alpha.MerchantID().UUID(), customer.UUID())
	require.NoError(t, err)
	statuses, err := pgx.CollectRows(rows, pgx.RowTo[string])
	require.NoError(t, err)
	return statuses
}

// standing is the PSP's standing in program for the month containing at.
func standing(t *testing.T, a *authorityWorld, program billing.DisputeProgram, at time.Time) billing.DisputeStanding {
	t.Helper()
	page, err := a.alpha.ListDisputePrograms(t.Context(), billing.DisputeProgramListParams{At: &at})
	require.NoError(t, err)
	for _, s := range page.Items {
		if s.Program == program {
			return s
		}
	}
	t.Fatalf("no %s standing in %+v", program, page.Items)
	return billing.DisputeStanding{}
}

// A chargeback is one dispute with its network, reason family, code and due
// date; it books one reversal, ends the subscription and its access, blocks
// the card and asks staff to review the customer. Replays change nothing.
func disputeChargeback(t *testing.T) {
	m := newDisputedMembership(t)
	due := time.Now().Add(7 * 24 * time.Hour).UTC().Truncate(time.Second)
	d := m.dispute("dp_chargeback", "needs_response", "chargeback", "fraudulent")
	d["evidence_details"] = map[string]any{"due_by": due.Unix()}
	m.notice(t, "evt_chargeback_created", "charge.dispute.created", d)
	m.notice(t, "evt_chargeback_created", "charge.dispute.created", d)
	m.notice(t, "evt_chargeback_withdrawn", "charge.dispute.funds_withdrawn", d)

	reversals := m.reversals(t)
	require.Len(t, reversals, 1, "one reversal however often the chargeback is reported")
	require.Equal(t, billing.PaymentChargeback, reversals[0].Kind)
	require.EqualValues(t, -10_000_000, reversals[0].Amount)
	dispute := disputeOf(t, m.a, m.payment)
	require.Equal(t, "dp_chargeback", dispute.ProviderRef)
	require.Equal(t, billing.DisputeChargeback, dispute.Kind)
	require.Equal(t, billing.DisputeOpen, dispute.Status)
	require.Equal(t, billing.NetworkVisa, dispute.Network)
	require.Equal(t, billing.DisputeFraud, dispute.Reason)
	require.Equal(t, "10.4", dispute.ReasonCode)
	require.NotNil(t, dispute.EvidenceDueAt)
	require.True(t, dispute.EvidenceDueAt.Equal(due))
	require.NotNil(t, dispute.ChargebackAt)
	require.Equal(t, reversals[0].ID, *dispute.ReversalPaymentID)
	require.Equal(t, m.sub.CustomerID, *dispute.CustomerID)

	canceled := subscriptionState(t, m.a, m.sub.ID, "canceled")
	require.Equal(t, "chargeback", *canceled.CancelType)
	require.False(t, m.entitled(t), "a chargeback ends access")
	require.Contains(t, cardStatuses(t, m.a, m.sub.CustomerID), "disputed", "the charged-back card is never charged again")
	reviews, err := m.a.alpha.ListFindings(t.Context(), billing.FindingListParams{Type: "life.disputes.customer_review"})
	require.NoError(t, err)
	require.Len(t, reviews.Items, 1)
	require.Equal(t, m.sub.CustomerID.String(), reviews.Items[0].SubjectKey)
}

// An RDR resolution returns the money as a refund (dispute_prevention); a
// Compelling Evidence 3.0 block moves none. Neither is a chargeback nor
// counts in a ratio.
func disputePrevented(t *testing.T) {
	t.Run("rdr-resolution-is-a-refund", func(t *testing.T) {
		m := newDisputedMembership(t)
		m.notice(t, "evt_rdr", "charge.dispute.created", m.dispute("dp_rdr", "lost", "resolution", "subscription_canceled"))
		reversals := m.reversals(t)
		require.Len(t, reversals, 1)
		require.Equal(t, billing.PaymentRefund, reversals[0].Kind)
		require.EqualValues(t, -10_000_000, reversals[0].Amount)
		require.NotNil(t, reversals[0].RefundReason)
		require.Equal(t, billing.RefundDisputePrevention, *reversals[0].RefundReason)
		dispute := disputeOf(t, m.a, m.payment)
		require.Equal(t, billing.DisputePrevented, dispute.Kind)
		require.Equal(t, billing.DisputeClosed, dispute.Status)
		require.Nil(t, dispute.ChargebackAt)
		require.Zero(t, standing(t, m.a, billing.ProgramStripe, m.a.clock.Now()).Count)
	})
	t.Run("ce3-block-books-nothing", func(t *testing.T) {
		m := newDisputedMembership(t)
		m.notice(t, "evt_block", "charge.dispute.created", m.dispute("dp_block", "prevented", "block", "fraudulent"))
		require.Empty(t, m.reversals(t))
		require.True(t, m.entitled(t))
		dispute := disputeOf(t, m.a, m.payment)
		require.Equal(t, billing.DisputePrevented, dispute.Kind)
		require.Zero(t, standing(t, m.a, billing.ProgramStripe, m.a.clock.Now()).Count)
		require.Zero(t, standing(t, m.a, billing.ProgramVisaVAMP, m.a.clock.Now()).Count)
	})
}

// A won dispute returns the money and stays won: a late notice that it was
// opened reverses nothing.
func disputeWon(t *testing.T) {
	m := newDisputedMembership(t)
	d := m.dispute("dp_won", "needs_response", "chargeback", "product_not_received")
	m.notice(t, "evt_won_created", "charge.dispute.created", d)
	d["status"] = "won"
	m.notice(t, "evt_won_closed", "charge.dispute.closed", d)
	d["status"] = "needs_response"
	m.notice(t, "evt_won_late", "charge.dispute.updated", d)

	reversals := m.reversals(t)
	require.Len(t, reversals, 2, "the chargeback and its recovery")
	dispute := disputeOf(t, m.a, m.payment)
	require.Equal(t, billing.DisputeWon, dispute.Status)
	require.Equal(t, billing.DisputeNotReceived, dispute.Reason)
	require.NotNil(t, dispute.ClosedAt)
	require.NotNil(t, dispute.RecoveryPaymentID)
	require.NotNil(t, dispute.ReversalPaymentID)
}

// A CCBill chargeback is recorded with its reason and card network; CCBill
// reports no outcome.
func disputeCCBill(t *testing.T) {
	a := ccbillWebhookWorld(t)
	price := ccbillMemberPrice(t, a.alpha)
	carol := customerOf(a.alice)
	now := a.clock.Now().UTC().Truncate(time.Second)
	importLegacyCCBill(t, a.alpha, now)(legacyCCBillMember(carol, price, "334455667788990011", "0812233445566778803", now.Add(5*24*time.Hour)))
	status, body := ccbillNotice(t, a, "Chargeback", map[string]any{"subscriptionId": "0334455667788990011", "transactionId": "0812233445566778803",
		"amount": "10.00", "currencyCode": "840", "reason": "Fraudulent transaction", "cardType": "VISA"})
	require.Equal(t, http.StatusOK, status, body)
	page, err := a.alpha.ListDisputes(t.Context(), billing.DisputeListParams{CustomerID: carol})
	require.NoError(t, err)
	require.Len(t, page.Items, 1)
	dispute := page.Items[0]
	require.Equal(t, billing.RailCCBill, dispute.Rail)
	require.Equal(t, billing.DisputeChargeback, dispute.Kind)
	require.Equal(t, billing.DisputeUnreported, dispute.Status)
	require.Equal(t, billing.NetworkVisa, dispute.Network)
	require.Equal(t, billing.DisputeFraud, dispute.Reason)
	require.NotNil(t, dispute.ReversalPaymentID)
	require.NotNil(t, dispute.Amount)
	require.EqualValues(t, 10_000_000, *dispute.Amount)
}

func (m *disputedMembership) fraudWarning(t *testing.T, id string) {
	t.Helper()
	m.notice(t, "evt_"+id, "radar.early_fraud_warning.created", stripemock.Object{"id": id, "object": "radar.early_fraud_warning", "charge": m.charge,
		"payment_intent": m.intent, "fraud_type": "made_with_stolen_card", "actionable": true, "created": time.Now().Unix(), "livemode": false})
}

// An actionable early fraud warning is a fraud report Visa counts; it refunds
// the charge in full (fraud_warning) once, ends what it paid for and blocks
// the card. A merchant that records reports only refunds nothing.
func disputeFraudWarning(t *testing.T) {
	t.Run("refunds-by-default", func(t *testing.T) {
		m := newDisputedMembership(t)
		startFleet(t, m.a.world)
		m.fraudWarning(t, "issfr_refund")
		m.fraudWarning(t, "issfr_refund")
		reversals := m.reversals(t)
		require.Len(t, reversals, 1, "one refund however often the report arrives")
		require.Equal(t, billing.PaymentRefund, reversals[0].Kind)
		require.EqualValues(t, -10_000_000, reversals[0].Amount)
		require.Equal(t, billing.RefundFraudWarning, *reversals[0].RefundReason)
		subscriptionState(t, m.a, m.sub.ID, "canceled")
		require.False(t, m.entitled(t))
		require.Contains(t, cardStatuses(t, m.a, m.sub.CustomerID), "disputed")
		require.EqualValues(t, 1, standing(t, m.a, billing.ProgramVisaVAMP, m.a.clock.Now()).Count, "Visa counts the report, refunded or not")
		require.Eventually(t, func() bool {
			return m.reversals(t)[0].Status == billing.PaymentSucceeded
		}, 20*time.Second, 50*time.Millisecond, "the refund settles at the provider")
	})
	t.Run("record-only", func(t *testing.T) {
		m := newDisputedMembership(t)
		_, err := m.a.alpha.UpdateMerchantConfiguration(t.Context(), billing.UpdateMerchantConfigurationParams{MerchantSettings: &billing.MerchantSettings{FraudWarningRefund: ptr(billing.FraudWarningRecord)}})
		require.NoError(t, err)
		m.fraudWarning(t, "issfr_record")
		require.Empty(t, m.reversals(t))
		require.True(t, m.entitled(t))
		require.EqualValues(t, 1, standing(t, m.a, billing.ProgramVisaVAMP, m.a.clock.Now()).Count)
	})
}

// sales is a world of one-off $10 Stripe sales.
type sales struct {
	a       *authorityWorld
	p       *providerLedger
	price   billing.PriceID
	charges map[billing.PaymentID][2]string
}

func newSales(t *testing.T, configure ...func(*world)) *sales {
	t.Helper()
	a, p := newPaymentWorld(t, configure...)
	product, err := a.alpha.CreateProduct(t.Context(), billing.CreateProductParams{Key: "download", DisplayName: "Download", Ownership: catalog.OwnershipConsumable, Entitlements: []string{"download:owned"}})
	require.NoError(t, err)
	price, err := a.alpha.CreatePrice(t.Context(), billing.CreatePriceParams{ProductID: product.ID, Key: "download", UnitAmount: 10_000_000, Currency: "USD"})
	require.NoError(t, err)
	return &sales{a: a, p: p, price: price.ID, charges: map[billing.PaymentID][2]string{}}
}

func (s *sales) buy(t *testing.T, key string) billing.Payment {
	t.Helper()
	input := billing.CreateMyOrderParams{Lines: []billing.OrderLineParams{{PriceID: s.price}}, ExpectedTotal: ptr(int64(10_000_000)), Reusable: ptr(true),
		Payment: &billing.OrderPaymentParams{Token: s.p.core.NewPaymentMethod(stripemock.Card{Brand: "visa", Last4: "4242"})}}
	status, raw := s.a.request(t, http.MethodPost, "/v1/me/orders", s.a.aliceToken, input, map[string]string{"Idempotency-Key": key})
	require.Equal(t, http.StatusCreated, status, string(raw))
	var order billing.Order
	require.NoError(t, json.Unmarshal(raw, &order))
	paid, err := s.a.alpha.GetPayment(t.Context(), *order.Payment.PaymentID)
	require.NoError(t, err)
	for _, effect := range s.p.effects(t) {
		if effect.Kind == "charge" && (effect.ID == paid.TransactionID || effect.Operation == paid.TransactionID) {
			s.charges[paid.ID] = [2]string{effect.ID, effect.Operation}
		}
	}
	require.Contains(t, s.charges, paid.ID)
	return *paid
}

func (s *sales) chargeback(t *testing.T, payment billing.PaymentID, id string) {
	t.Helper()
	ch := s.charges[payment]
	d := stripemock.Object{"id": id, "object": "dispute", "amount": int64(1000), "currency": "usd", "charge": ch[0], "payment_intent": ch[1],
		"status": "needs_response", "reason": "fraudulent", "created": time.Now().Unix(),
		"payment_method_details": map[string]any{"type": "card", "card": map[string]any{"brand": "visa", "case_type": "chargeback", "network_reason_code": "10.4"}}}
	body := stripeNotice(t, "evt_"+id, "charge.dispute.created", d)
	status, raw := callback(t, s.a, "acct_e2e", body, stripeSignature(body, "whsec_critical_fixture"))
	require.Equal(t, http.StatusOK, status, raw)
}

// thresholdFinding is the open threshold finding of program, if any.
func thresholdFinding(t *testing.T, a *authorityWorld, psp billing.PSPID, program billing.DisputeProgram) *billing.Finding {
	t.Helper()
	page, err := a.alpha.ListFindings(t.Context(), billing.FindingListParams{Type: "life.disputes.threshold"})
	require.NoError(t, err)
	subject := psp.String() + ":" + string(program) + ":" + a.clock.Now().UTC().Format("2006-01")
	for _, f := range page.Items {
		if f.SubjectKey == subject {
			return &f
		}
	}
	return nil
}

// notifiedAt is the severity a finding was last alerted at ("" for none).
func notifiedAt(t *testing.T, a *authorityWorld, id billing.FindingID) string {
	t.Helper()
	var severity *string
	require.NoError(t, a.pool.QueryRow(t.Context(), "SELECT notified_severity FROM "+pgx.Identifier{a.schema, "reconciliation_findings"}.Sanitize()+" WHERE merchant_id = $1 AND id = $2",
		a.alpha.MerchantID().UUID(), id.UUID()).Scan(&severity))
	if severity == nil {
		return ""
	}
	return *severity
}

// Chargebacks that bring a PSP's ratio to its acquirer limit's warning raise
// one high finding that alerts; at the limit it escalates to critical and
// alerts again. The program read shows the same standing.
func disputeMonitor(t *testing.T) {
	s := newSales(t, func(w *world) {
		declaration := cfgMerchant(w.cfg, "alpha")
		psp := declaration.PSPs["stripe"]
		psp.Settings["dispute_ratio_limit"] = "60%"
		declaration.PSPs["stripe"] = psp
		setCfgMerchant(&w.cfg, "alpha", declaration)
	})
	startFleet(t, s.a.world)
	var paid []billing.Payment
	for i := range 10 {
		paid = append(paid, s.buy(t, "monitor-sale-"+string(rune('a'+i))))
	}
	psp := *paid[0].PSPID
	for i := range 4 {
		s.chargeback(t, paid[i].ID, "dp_monitor_"+string(rune('a'+i)))
	}
	require.Nil(t, thresholdFinding(t, s.a, psp, billing.ProgramAcquirer), "four chargebacks are under every minimum")

	s.chargeback(t, paid[4].ID, "dp_monitor_e")
	var warning *billing.Finding
	require.Eventually(t, func() bool {
		warning = thresholdFinding(t, s.a, psp, billing.ProgramAcquirer)
		return warning != nil && notifiedAt(t, s.a, warning.ID) == "high"
	}, 20*time.Second, 50*time.Millisecond, "5 of 10 is past two thirds of 60%%: a high finding that alerts")
	require.Equal(t, "high", warning.Severity)
	acquirer := standing(t, s.a, billing.ProgramAcquirer, s.a.clock.Now())
	require.Equal(t, billing.DisputeLevelWarning, acquirer.Level)
	require.EqualValues(t, 5, acquirer.Count)
	require.EqualValues(t, 10, acquirer.Denominator)

	s.chargeback(t, paid[5].ID, "dp_monitor_f")
	require.Eventually(t, func() bool {
		f := thresholdFinding(t, s.a, psp, billing.ProgramAcquirer)
		return f != nil && f.Severity == "critical" && notifiedAt(t, s.a, f.ID) == "critical"
	}, 20*time.Second, 50*time.Millisecond, "6 of 10 is the limit: the finding escalates and alerts again")
	require.Equal(t, billing.DisputeLevelExcessive, standing(t, s.a, billing.ProgramAcquirer, s.a.clock.Now()).Level)
	require.NotNil(t, thresholdFinding(t, s.a, psp, billing.ProgramStripe), "Stripe's own 0.75% is passed too")
}

// Mastercard divides a month's chargebacks by the previous month's sales;
// Visa by the month's own.
func disputeMastercardLag(t *testing.T) {
	s := newSales(t)
	s.buy(t, "lag-sale")
	var psp uuid.UUID
	require.NoError(t, s.a.pool.QueryRow(t.Context(), "SELECT id FROM "+pgx.Identifier{s.a.schema, "psps"}.Sanitize()+" WHERE merchant_id = $1 AND key = 'stripe'", s.a.alpha.MerchantID().UUID()).Scan(&psp))
	now := s.a.clock.Now().UTC()
	month := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC).AddDate(0, -2, 0)
	insert := func(day time.Time, network string, sales, chargebacks int64) {
		_, err := s.a.pool.Exec(t.Context(), "INSERT INTO "+pgx.Identifier{s.a.schema, "network_activity_daily"}.Sanitize()+
			" (merchant_id, psp_id, rail, network, currency, day_at, sales, chargebacks) VALUES ($1, $2, 'stripe', $3, 'USD', $4, $5, $6)",
			s.a.alpha.MerchantID().UUID(), psp, network, day, sales, chargebacks)
		require.NoError(t, err)
	}
	insert(month.AddDate(0, -1, 3), "mastercard", 1000, 0)
	insert(month.AddDate(0, 0, 5), "mastercard", 10, 20)
	insert(month.AddDate(0, 0, 5), "visa", 400, 4)
	at := month.AddDate(0, 0, 20)
	ecp := standing(t, s.a, billing.ProgramMastercardECP, at)
	require.EqualValues(t, 20, ecp.Count)
	require.EqualValues(t, 1000, ecp.Denominator, "the previous month's sales")
	require.InDelta(t, 0.02, ecp.ProjectedRatio, 1e-9)
	vamp := standing(t, s.a, billing.ProgramVisaVAMP, at)
	require.EqualValues(t, 4, vamp.Count)
	require.EqualValues(t, 400, vamp.Denominator, "the month's own sales")
}

// The VAMP level in force is the evaluated month's: 2.2% in March 2026,
// 1.5% from April.
func disputeProgramDates(t *testing.T) {
	s := newSales(t)
	excessive := func(at time.Time) float64 {
		for _, th := range standing(t, s.a, billing.ProgramVisaVAMP, at).Thresholds {
			if th.Level == billing.DisputeLevelExcessive {
				return th.Ratio
			}
		}
		return 0
	}
	require.InDelta(t, 0.022, excessive(time.Date(2026, time.March, 15, 0, 0, 0, 0, time.UTC)), 1e-9)
	require.InDelta(t, 0.015, excessive(time.Date(2026, time.April, 15, 0, 0, 0, 0, time.UTC)), 1e-9)
}

// A staff refund carries its reason (requested_by_customer unless named); a
// Stripe dashboard refund Stripe's. Refunds count in no dispute ratio.
func disputeRefundReasons(t *testing.T) {
	s := newSales(t)
	named, plain, dashboard := s.buy(t, "reason-named"), s.buy(t, "reason-plain"), s.buy(t, "reason-dashboard")
	refund, err := s.a.alpha.RefundPayment(t.Context(), "reason-named", named.ID, billing.RefundPaymentParams{Full: true, RefundReason: billing.RefundDuplicate, Reason: "charged twice"})
	require.NoError(t, err)
	require.Equal(t, billing.RefundDuplicate, *refund.RefundReason)
	refund, err = s.a.alpha.RefundPayment(t.Context(), "reason-plain", plain.ID, billing.RefundPaymentParams{Full: true})
	require.NoError(t, err)
	require.Equal(t, billing.RefundRequestedByCustomer, *refund.RefundReason)
	ch := s.charges[dashboard.ID]
	body := stripeNotice(t, "evt_dashboard_refund", "refund.created", stripemock.Object{"id": "re_dashboard", "object": "refund", "amount": int64(1000), "currency": "usd",
		"charge": ch[0], "payment_intent": ch[1], "status": "succeeded", "reason": "fraudulent", "metadata": map[string]string{}})
	status, raw := callback(t, s.a, "acct_e2e", body, stripeSignature(body, "whsec_critical_fixture"))
	require.Equal(t, http.StatusOK, status, raw)
	read, err := s.a.alpha.GetPayment(t.Context(), dashboard.ID)
	require.NoError(t, err)
	require.Len(t, read.Refunds, 1)
	require.Equal(t, billing.RefundFraudulent, *read.Refunds[0].RefundReason)
	now := s.a.clock.Now()
	require.Zero(t, standing(t, s.a, billing.ProgramStripe, now).Count)
	require.Zero(t, standing(t, s.a, billing.ProgramVisaVAMP, now).Count)
	require.EqualValues(t, 3, standing(t, s.a, billing.ProgramVisaVAMP, now).Denominator, "three settled sales")
}
