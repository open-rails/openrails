//go:build integration

package critical_test

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river/rivertype"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/catalog"
	"github.com/open-rails/openrails/internal/modules/dashboard"
	"github.com/open-rails/openrails/internal/stripemock"
)

// TestDisputes is journey J46: chargebacks, inquiries and fraud reports.
func TestDisputes(t *testing.T) {
	t.Run("OR-J46-S01/a-chargeback-is-recorded-reversed-and-ends-its-charge", disputeChargeback)
	t.Run("OR-J46-S02/an-inquiry-books-and-cancels-nothing", disputeInquiry)
	t.Run("OR-J46-S03/a-prevented-dispute-is-no-chargeback", disputePrevented)
	t.Run("OR-J46-S04/a-won-dispute-stays-won", disputeWon)
	t.Run("OR-J46-S05/an-nmi-batch-records-every-entry", disputeNMIBatch)
	t.Run("OR-J46-S06/a-ccbill-chargeback-is-recorded", disputeCCBill)
	t.Run("OR-J46-S07/an-early-fraud-warning-refunds-its-charge", disputeFraudWarning)
	t.Run("OR-J46-S08/the-monitor-raises-and-escalates-alerting-findings", disputeMonitor)
	t.Run("OR-J46-S09/mastercard-divides-by-the-previous-months-sales", disputeMastercardLag)
	t.Run("OR-J46-S10/the-program-in-force-is-the-months", disputeProgramDates)
	t.Run("OR-J46-S11/a-charged-back-card-is-never-charged-again", disputeBlockedCard)
	t.Run("OR-J46-S12/the-dispute-metrics-equal-the-records", disputeMetrics)
	t.Run("OR-J46-S13/the-rollup-equals-its-records-after-a-restore", disputeRollupRestore)
	t.Run("OR-J46-S14/refunds-carry-their-reason-and-count-in-no-ratio", disputeRefundReasons)
	t.Run("OR-J46-S15/a-chargeback-blocks-the-charged-card-only", disputeChargedCardOnly)
	t.Run("OR-J46-S16/a-report-on-an-unknown-charge-is-answered", disputeUnknownCharge)
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

func newDisputedMembership(t *testing.T, configure ...func(*world)) *disputedMembership {
	t.Helper()
	a, p, sub := purchasedMembershipWith(t, 24, configure...)
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
		"status": status, "reason": reason, "created": m.a.clock.Now().Unix(),
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
		iw, _ := pulledChargeback(t)
		require.Eventually(t, func() bool { return waiting(t, iw.authorityWorld) != nil }, 20*time.Second, 50*time.Millisecond,
			"an advisory pull's chargeback waits for the merchant to be armed")
		require.False(t, responded(t, iw.authorityWorld, "dp_pulled"))
		armMerchant(t, iw.authorityWorld)
		require.Eventually(t, func() bool { return responded(t, iw.authorityWorld, "dp_pulled") }, 20*time.Second, 50*time.Millisecond, "arming wakes it")
	})
	t.Run("a-notice-releases-a-pulled-case", func(t *testing.T) {
		iw, sub := pulledChargeback(t)
		require.Eventually(t, func() bool { return waiting(t, iw.authorityWorld) != nil }, 20*time.Second, 50*time.Millisecond)
		trail(iw).send(t, "charge.dispute.updated", iw.stripe.Dispute("dp_pulled", sub, "under_review", "chargeback", "fraudulent"))
		require.Eventually(t, func() bool { return responded(t, iw.authorityWorld, "dp_pulled") }, 20*time.Second, 50*time.Millisecond,
			"a provider's notice is no advisory pull's: the response runs unarmed")
	})
}

// pulledChargeback is a Stripe membership imported by an advisory pull whose
// charge a later pull finds charged back, its webhook missed.
func pulledChargeback(t *testing.T) (*importWorld, string) {
	t.Helper()
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
	recorded, err := iw.alpha.ListDisputes(t.Context(), billing.DisputeListParams{})
	require.NoError(t, err)
	require.Len(t, recorded.Items, 1, "a missed webhook's inquiry is recorded by the pull")
	require.Equal(t, billing.DisputeInquiry, recorded.Items[0].Kind)
	iw.stripe.Dispute("dp_pulled", sub, "needs_response", "chargeback", "fraudulent")
	require.Equal(t, 1, chargebackProposals(iw.manual(t, "stripe").ID), "the escalated inquiry is")
	require.True(t, iw.entitled(t, customerOf(iw.alice), "member:paid"), "a pull lands nothing")
	return iw, sub
}

// responded reports whether the dispute's response finished.
func responded(t *testing.T, a *authorityWorld, dispute string) bool {
	t.Helper()
	var at *time.Time
	require.NoError(t, a.pool.QueryRow(t.Context(), "SELECT responded_at FROM "+pgx.Identifier{a.schema, "disputes"}.Sanitize()+
		" WHERE merchant_id = $1 AND provider_ref = $2", a.alpha.MerchantID().UUID(), dispute).Scan(&at))
	return at != nil
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
	startFleet(t, m.a.world)
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
	waitDisputeJobs(t, m.a)
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
		m.notice(t, "evt_rdr_stale", "charge.dispute.updated", m.dispute("dp_rdr", "warning_needs_response", "inquiry", "subscription_canceled"))
		dispute = disputeOf(t, m.a, m.payment)
		require.Equal(t, billing.DisputePrevented, dispute.Kind, "a stale inquiry notice reopens nothing")
		require.Equal(t, billing.DisputeClosed, dispute.Status)
		require.Len(t, m.reversals(t), 1)
	})
	t.Run("refund-first-is-the-resolution", func(t *testing.T) {
		m := newDisputedMembership(t)
		d := m.dispute("dp_rdr_late", "lost", "resolution", "subscription_canceled")
		ch := m.charge
		body := stripeNotice(t, "evt_rdr_refund", "refund.created", stripemock.Object{"id": "re_rdr", "object": "refund", "amount": int64(1000), "currency": "usd",
			"charge": ch, "payment_intent": m.intent, "status": "succeeded", "reason": "fraudulent", "created": m.a.clock.Now().Unix(), "metadata": map[string]string{}})
		status, raw := callback(t, m.a, "acct_e2e", body, stripeSignature(body, "whsec_critical_fixture"))
		require.Equal(t, http.StatusOK, status, raw)
		m.notice(t, "evt_rdr_after", "charge.dispute.created", d)
		reversals := m.reversals(t)
		require.Len(t, reversals, 1, "Stripe's refund is the resolution's")
		require.Equal(t, "re_rdr", reversals[0].TransactionID)
		dispute := disputeOf(t, m.a, m.payment)
		require.Equal(t, reversals[0].ID, *dispute.ReversalPaymentID)
		require.Equal(t, "re_rdr", absorbedRefund(t, m.a, "dp_rdr_late"))
	})
	t.Run("resolution-first-absorbs-the-refund", func(t *testing.T) {
		m := newDisputedMembership(t)
		m.notice(t, "evt_rdr_first", "charge.dispute.created", m.dispute("dp_rdr_first", "lost", "resolution", "subscription_canceled"))
		body := stripeNotice(t, "evt_rdr_refund_after", "refund.created", stripemock.Object{"id": "re_rdr_after", "object": "refund", "amount": int64(1000), "currency": "usd",
			"charge": m.charge, "payment_intent": m.intent, "status": "succeeded", "reason": "fraudulent", "created": m.a.clock.Now().Unix(), "metadata": map[string]string{}})
		for range 2 {
			status, raw := callback(t, m.a, "acct_e2e", body, stripeSignature(body, "whsec_critical_fixture"))
			require.Equal(t, http.StatusOK, status, raw)
		}
		require.Len(t, m.reversals(t), 1, "one refund, whichever arrives first")
		require.Equal(t, "re_rdr_after", absorbedRefund(t, m.a, "dp_rdr_first"))
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
// opened reverses nothing. A lost one stays lost against a late notice from
// before the decision.
func disputeWon(t *testing.T) {
	t.Run("won", func(t *testing.T) {
		m := newDisputedMembership(t)
		d := m.dispute("dp_won", "needs_response", "chargeback", "product_not_received")
		d["payment_method_details"] = map[string]any{"type": "card", "card": map[string]any{"brand": "visa", "case_type": "chargeback", "network_reason_code": "13.1"}}
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
	})
	t.Run("lost", func(t *testing.T) {
		m := newDisputedMembership(t)
		d := m.dispute("dp_lost", "needs_response", "chargeback", "fraudulent")
		m.notice(t, "evt_lost_created", "charge.dispute.created", d)
		d["status"] = "lost"
		m.notice(t, "evt_lost_closed", "charge.dispute.closed", d)
		d["status"] = "under_review"
		m.notice(t, "evt_lost_late", "charge.dispute.updated", d)
		dispute := disputeOf(t, m.a, m.payment)
		require.Equal(t, billing.DisputeLost, dispute.Status, "a notice from before the decision reopens nothing")
		require.NotNil(t, dispute.ClosedAt)
		require.Len(t, m.reversals(t), 1)
	})
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

func (m *disputedMembership) fraudWarning(t *testing.T, event, kind, id string, actionable bool) {
	t.Helper()
	m.notice(t, event, kind, stripemock.Object{"id": id, "object": "radar.early_fraud_warning", "charge": m.charge,
		"payment_intent": m.intent, "fraud_type": "made_with_stolen_card", "actionable": actionable, "created": m.a.clock.Now().Unix(), "livemode": false})
}

// waitDisputeJobs waits until no dispute job is left to run now: the
// monitor's and the response's. A job in one of keep (a snoozed response)
// is allowed to stay.
func waitDisputeJobs(t *testing.T, a *authorityWorld, keep ...rivertype.JobState) {
	t.Helper()
	pending := []rivertype.JobState{rivertype.JobStateAvailable, rivertype.JobStateRunning, rivertype.JobStateRetryable, rivertype.JobStatePending, rivertype.JobStateScheduled}
	var waiting []string
	for _, state := range pending {
		if !slices.Contains(keep, state) {
			waiting = append(waiting, string(state))
		}
	}
	require.Eventually(t, func() bool {
		var n int
		require.NoError(t, a.pool.QueryRow(t.Context(), "SELECT count(*) FROM "+pgx.Identifier{a.schema, "river_job"}.Sanitize()+
			" WHERE kind IN ('openrails.dispute_monitor', 'openrails.dispute_response') AND state::text = ANY($1)", waiting).Scan(&n))
		return n == 0
	}, 20*time.Second, 50*time.Millisecond, "the dispute jobs ran")
}

// An actionable early fraud warning is a fraud report Visa counts; it refunds
// the charge in full once (fraud_warning, sent to Stripe as fraudulent), ends
// what it paid for and blocks the card, however often and in whatever events
// the report arrives, without waiting for the destructive switch; one any
// event calls unactionable is not acted on. A charge already in dispute is
// not refunded; a charge with nothing left to refund is still ended. A merchant
// that records reports only refunds nothing. Below a full write posture the
// response waits, raises one alerting finding, and runs when the posture is
// raised.
func disputeFraudWarning(t *testing.T) {
	t.Run("refunds-by-default", func(t *testing.T) {
		m := newDisputedMembership(t)
		startFleet(t, m.a.world)
		m.fraudWarning(t, "evt_efw_created", "radar.early_fraud_warning.created", "issfr_refund", true)
		m.fraudWarning(t, "evt_efw_created", "radar.early_fraud_warning.created", "issfr_refund", true)
		m.fraudWarning(t, "evt_efw_again", "radar.early_fraud_warning.created", "issfr_refund", true)
		m.fraudWarning(t, "evt_efw_updated", "radar.early_fraud_warning.updated", "issfr_refund", true)
		require.Eventually(t, func() bool {
			reversals := m.reversals(t)
			return len(reversals) == 1 && reversals[0].Status == billing.PaymentSucceeded
		}, 20*time.Second, 50*time.Millisecond, "one refund, settled at the provider")
		reversals := m.reversals(t)
		require.Equal(t, billing.PaymentRefund, reversals[0].Kind)
		require.EqualValues(t, -10_000_000, reversals[0].Amount)
		require.Equal(t, billing.RefundFraudWarning, *reversals[0].RefundReason)
		var reasons []string
		rows, err := m.p.pool.Query(t.Context(), "SELECT object->>'reason' FROM "+pgx.Identifier{m.p.schema, "effects"}.Sanitize()+" WHERE kind = 'refund'")
		require.NoError(t, err)
		reasons, err = pgx.CollectRows(rows, pgx.RowTo[string])
		require.NoError(t, err)
		require.Equal(t, []string{"fraudulent"}, reasons, "Stripe is told the refund answers fraud")
		subscriptionState(t, m.a, m.sub.ID, "canceled")
		require.False(t, m.entitled(t))
		require.Contains(t, cardStatuses(t, m.a, m.sub.CustomerID), "disputed")
		require.EqualValues(t, 1, standing(t, m.a, billing.ProgramVisaVAMP, m.a.clock.Now()).Count, "Visa counts the report once, refunded or not")
	})
	t.Run("a-disputed-charge-is-not-refunded", func(t *testing.T) {
		m := newDisputedMembership(t)
		startFleet(t, m.a.world)
		m.notice(t, "evt_open_inquiry", "charge.dispute.created", m.dispute("dp_open_inquiry", "warning_needs_response", "inquiry", "fraudulent"))
		m.fraudWarning(t, "evt_efw_disputed", "radar.early_fraud_warning.created", "issfr_disputed", true)
		waitDisputeJobs(t, m.a)
		require.Empty(t, m.reversals(t), "the open inquiry decides the charge")
		require.True(t, m.entitled(t))
	})
	t.Run("unactionable-stays-so", func(t *testing.T) {
		m := newDisputedMembership(t)
		startFleet(t, m.a.world)
		m.fraudWarning(t, "evt_efw_unactionable", "radar.early_fraud_warning.created", "issfr_unactionable", false)
		m.fraudWarning(t, "evt_efw_actionable", "radar.early_fraud_warning.updated", "issfr_unactionable", true)
		waitDisputeJobs(t, m.a)
		require.Empty(t, m.reversals(t), "a report one event called unactionable stays so")
		require.True(t, m.entitled(t))
	})
	t.Run("record-only", func(t *testing.T) {
		m := newDisputedMembership(t, func(w *world) {
			declaration := cfgMerchant(w.cfg, "alpha")
			declaration.FraudWarningRefund = ptr(billing.FraudWarningRecord)
			setCfgMerchant(&w.cfg, "alpha", declaration)
		})
		startFleet(t, m.a.world)
		m.fraudWarning(t, "evt_efw_record", "radar.early_fraud_warning.created", "issfr_record", true)
		waitDisputeJobs(t, m.a)
		require.Empty(t, m.reversals(t))
		require.True(t, m.entitled(t))
		require.EqualValues(t, 1, standing(t, m.a, billing.ProgramVisaVAMP, m.a.clock.Now()).Count)
	})
	t.Run("a-held-response-waits-and-wakes", func(t *testing.T) {
		m := newDisputedMembership(t)
		setPosture(t, m.a, "limited")
		startFleet(t, m.a.world)
		m.fraudWarning(t, "evt_efw_limited", "radar.early_fraud_warning.created", "issfr_limited", true)
		var held *billing.Finding
		require.Eventually(t, func() bool { held = waiting(t, m.a); return held != nil }, 20*time.Second, 50*time.Millisecond,
			"a held response is a finding")
		require.Equal(t, "high", notifiedAt(t, m.a, held.ID), "and alerts once")
		waitDisputeJobs(t, m.a)
		require.Empty(t, m.reversals(t), "the refund waits with everything else")
		require.True(t, m.entitled(t), "access stays while the refund cannot be sent")
		require.NotContains(t, cardStatuses(t, m.a, m.sub.CustomerID), "disputed")
		setPosture(t, m.a, "full")
		require.Eventually(t, func() bool {
			reversals := m.reversals(t)
			return len(reversals) == 1 && reversals[0].Status == billing.PaymentSucceeded
		}, 20*time.Second, 50*time.Millisecond, "a full posture wakes it")
		require.False(t, m.entitled(t))
		require.Eventually(t, func() bool { return waiting(t, m.a) == nil }, 20*time.Second, 50*time.Millisecond, "and resolves the finding")
	})
	t.Run("nothing-to-refund-still-ends", func(t *testing.T) {
		m := newDisputedMembership(t)
		startFleet(t, m.a.world)
		_, err := m.a.alpha.RefundPayment(t.Context(), "efw-prior-refund", m.payment, billing.RefundPaymentParams{Full: true})
		require.NoError(t, err)
		m.fraudWarning(t, "evt_efw_refunded", "radar.early_fraud_warning.created", "issfr_refunded", true)
		waitDisputeJobs(t, m.a)
		var action string
		require.NoError(t, m.a.pool.QueryRow(t.Context(), "SELECT action FROM "+pgx.Identifier{m.a.schema, "fraud_reports"}.Sanitize()+
			" WHERE merchant_id = $1 AND provider_ref = 'issfr_refunded'", m.a.alpha.MerchantID().UUID()).Scan(&action))
		require.Equal(t, "nothing_to_refund", action)
		require.Len(t, m.reversals(t), 1, "the earlier refund only")
		subscriptionState(t, m.a, m.sub.ID, "canceled")
		require.Contains(t, cardStatuses(t, m.a, m.sub.CustomerID), "disputed")
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
	s.dispute(t, payment, "evt_"+id, "charge.dispute.created", id, "needs_response", "chargeback", "fraudulent", "10.4")
}

// dispute is Stripe's notice of a dispute of payment's whole charge.
func (s *sales) dispute(t *testing.T, payment billing.PaymentID, event, kind, id, status, caseType, reason, code string) {
	t.Helper()
	ch := s.charges[payment]
	d := stripemock.Object{"id": id, "object": "dispute", "amount": int64(1000), "currency": "usd", "charge": ch[0], "payment_intent": ch[1],
		"status": status, "reason": reason, "created": s.a.clock.Now().Unix(),
		"payment_method_details": map[string]any{"type": "card", "card": map[string]any{"brand": "visa", "case_type": caseType, "network_reason_code": code}}}
	body := stripeNotice(t, event, kind, d)
	code2, raw := callback(t, s.a, "acct_e2e", body, stripeSignature(body, "whsec_critical_fixture"))
	require.Equal(t, http.StatusOK, code2, raw)
}

// The dispute metrics equal the records: disputes by kind, reason, outcome
// and the disputed charge's kind, the win rate, the chargeback rate by
// network against the networks' levels, fraud reports by outcome and refunds
// by reason. A card merchant's overview carries the programs widget, whose
// read answers.
func disputeMetrics(t *testing.T) {
	s := newSales(t)
	var paid []billing.Payment
	for i := range 4 {
		paid = append(paid, s.buy(t, fmt.Sprintf("metrics-sale-%d", i)))
	}
	s.chargeback(t, paid[0].ID, "dp_metrics_fraud")
	s.dispute(t, paid[1].ID, "evt_metrics_won_created", "charge.dispute.created", "dp_metrics_won", "needs_response", "chargeback", "product_not_received", "13.1")
	s.dispute(t, paid[1].ID, "evt_metrics_won_closed", "charge.dispute.closed", "dp_metrics_won", "won", "chargeback", "product_not_received", "13.1")
	s.dispute(t, paid[2].ID, "evt_metrics_inquiry", "charge.dispute.created", "dp_metrics_inquiry", "warning_needs_response", "inquiry", "general", "")
	_, err := s.a.alpha.RefundPayment(t.Context(), "metrics-refund", paid[3].ID, billing.RefundPaymentParams{Full: true, RefundReason: billing.RefundDuplicate})
	require.NoError(t, err)
	ch := s.charges[paid[2].ID]
	body := stripeNotice(t, "evt_metrics_efw", "radar.early_fraud_warning.created", stripemock.Object{"id": "issfr_metrics", "object": "radar.early_fraud_warning",
		"charge": ch[0], "payment_intent": ch[1], "fraud_type": "made_with_stolen_card", "actionable": true, "created": s.a.clock.Now().Unix()})
	status, raw := callback(t, s.a, "acct_e2e", body, stripeSignature(body, "whsec_critical_fixture"))
	require.Equal(t, http.StatusOK, status, raw)

	window := &billing.MetricsRange{Last: "30d"}
	by := func(measure, dim string, filters map[string][]string) map[string]int64 {
		t.Helper()
		return split(t, metricTable(t, s.a.alpha, billing.QueryMetricsParams{Measures: []string{measure}, By: []string{dim}, Filters: filters, Range: window}), dim, measure)
	}
	require.Equal(t, map[string]int64{"chargeback": 2, "inquiry": 1}, by("disputes", "dispute_kind", nil))
	require.Equal(t, map[string]int64{"fraud": 1, "not_received": 1}, by("disputes", "dispute_reason", map[string][]string{"dispute_kind": {"chargeback"}}))
	require.Equal(t, map[string]int64{"open": 2, "won": 1}, by("disputes", "outcome", nil))
	require.Equal(t, map[string]int64{"initial": 3}, by("disputes", "kind", nil), "every disputed charge was a first sale")
	require.Equal(t, map[string]int64{"recorded": 1}, by("fraud_reports", "outcome", nil))
	require.Equal(t, map[string]int64{"duplicate": 1}, by("payments", "refund_reason", map[string][]string{"payment_kind": {"refund"}}))
	win := only(t, metricTable(t, s.a.alpha, billing.QueryMetricsParams{Measures: []string{"dispute_win_rate"}, Range: window}))
	require.InDelta(t, 1, fraction(t, win["dispute_win_rate"]), 1e-12, "one won of one decided")
	rates := metricTable(t, s.a.alpha, billing.QueryMetricsParams{Measures: []string{"chargeback_rate"}, By: []string{"network"}, Range: window})
	require.Len(t, rates, 1)
	require.Equal(t, "visa", rates[0]["network"])
	require.InDelta(t, 0.5, fraction(t, rates[0]["chargeback_rate"]), 1e-12, "two chargebacks of four visa sales")

	vamp := only(t, metricTable(t, s.a.alpha, billing.QueryMetricsParams{Measures: []string{"vamp_ratio"}, Range: window}))
	require.InDelta(t, 0.75, fraction(t, vamp["vamp_ratio"]), 1e-12, "two chargebacks and a fraud report of four visa sales")
	schema, err := s.a.alpha.GetMetricsSchema(t.Context())
	require.NoError(t, err)
	lines := map[string]int{}
	for _, m := range schema.Measures {
		lines[m.Name] = len(m.Thresholds)
	}
	require.Zero(t, lines["chargeback_rate"], "a chargeback rate is no program's ratio")
	require.Equal(t, 2, lines["vamp_ratio"], "Visa's warning and excessive levels")
	require.Equal(t, 3, lines["ecm_ratio"], "Mastercard's warning, excessive and high excessive levels")
	board, err := s.a.alpha.GetDashboard(t.Context())
	require.NoError(t, err)
	programs := 0
	for _, w := range board.Widgets {
		if w.Viz == dashboard.Programs {
			programs++
		}
	}
	require.Equal(t, 1, programs, "a card merchant's payment health shows the dispute programs")
	standings, err := s.a.alpha.ListDisputePrograms(t.Context(), billing.DisputeProgramListParams{})
	require.NoError(t, err)
	require.NotEmpty(t, standings.Items)
	require.EqualValues(t, 3, standing(t, s.a, billing.ProgramVisaVAMP, s.a.clock.Now()).Count, "two chargebacks and a fraud report")
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
// alerts again. Sales that settle afterwards re-evaluate it, and enough of
// them resolve it. The minimums are actual counts, never projected ones.
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
		paid = append(paid, s.buy(t, fmt.Sprintf("monitor-sale-%d", i)))
	}
	psp := *paid[0].PSPID
	for i := range 4 {
		s.chargeback(t, paid[i].ID, fmt.Sprintf("dp_monitor_%d", i))
	}
	waitDisputeJobs(t, s.a)
	require.Nil(t, thresholdFinding(t, s.a, psp, billing.ProgramAcquirer), "four chargebacks are under every minimum, whatever the projection")
	require.Equal(t, billing.DisputeLevelNone, standing(t, s.a, billing.ProgramAcquirer, s.a.clock.Now()).Level)

	s.chargeback(t, paid[4].ID, "dp_monitor_4")
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

	s.chargeback(t, paid[5].ID, "dp_monitor_5")
	require.Eventually(t, func() bool {
		f := thresholdFinding(t, s.a, psp, billing.ProgramAcquirer)
		return f != nil && f.Severity == "critical" && notifiedAt(t, s.a, f.ID) == "critical"
	}, 20*time.Second, 50*time.Millisecond, "6 of 10 is the limit: the finding escalates and alerts again")
	require.Equal(t, billing.DisputeLevelExcessive, standing(t, s.a, billing.ProgramAcquirer, s.a.clock.Now()).Level)
	require.NotNil(t, thresholdFinding(t, s.a, psp, billing.ProgramStripe), "Stripe's own 0.75% is passed too")

	for i := 10; i < 30; i++ {
		s.buy(t, fmt.Sprintf("monitor-sale-%d", i))
	}
	require.Eventually(t, func() bool {
		return thresholdFinding(t, s.a, psp, billing.ProgramAcquirer) == nil
	}, 20*time.Second, 50*time.Millisecond, "6 of 30 is under the warning: the settled sales resolve the finding")
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

// A charged-back card is never charged again: the customer's other engine
// subscription on it keeps its paid period and waits for another card at
// renewal, while a subscription on their other card renews on it.
func disputeBlockedCard(t *testing.T) {
	m := newDisputedMembership(t)
	enableLifecycleMaintenance(t, m.a)
	methods, err := m.a.alpha.ListPaymentMethods(t.Context(), m.sub.CustomerID, billing.PaymentMethodListParams{})
	require.NoError(t, err)
	require.Len(t, methods.Items, 1)
	cardA := methods.Items[0].ID
	cardB := saveMethod(t, m.a, m.p, m.a.aliceToken, "1111").ID
	subscribe := func(key string, card billing.PaymentMethodID) billing.SubscriptionID {
		product, err := m.a.alpha.CreateProduct(t.Context(), billing.CreateProductParams{Key: key, DisplayName: key, Entitlements: []string{key + ":paid"}})
		require.NoError(t, err)
		price, err := m.a.alpha.CreatePrice(t.Context(), billing.CreatePriceParams{ProductID: product.ID, Key: key + "-day", UnitAmount: 5_000_000, Currency: "USD", BillingIntervalHours: ptr(24), AccessDurationHours: ptr(24)})
		require.NoError(t, err)
		input := billing.CreateMyOrderParams{Lines: []billing.OrderLineParams{{PriceID: price.ID}}, ExpectedTotal: ptr(int64(5_000_000)), Payment: &billing.OrderPaymentParams{PaymentMethodID: &card}}
		status, raw := m.a.request(t, http.MethodPost, "/v1/me/orders", m.a.aliceToken, input, map[string]string{"Idempotency-Key": key + "-purchase"})
		require.Equal(t, http.StatusCreated, status, string(raw))
		var order billing.Order
		require.NoError(t, json.Unmarshal(raw, &order))
		require.NotNil(t, order.Lines[0].SubscriptionID)
		id := *order.Lines[0].SubscriptionID
		_, err = m.a.alpha.SetSubscriptionPaymentMethod(t.Context(), id, billing.SetSubscriptionPaymentMethodParams{PaymentMethodID: &card})
		require.NoError(t, err)
		return id
	}
	onA, onB := subscribe("on-card-a", cardA), subscribe("on-card-b", cardB)
	charges := len(m.p.effects(t))

	fleet := startFleet(t, m.a.world)
	m.notice(t, "evt_blocked_card", "charge.dispute.created", m.dispute("dp_blocked_card", "needs_response", "chargeback", "fraudulent"))
	waitDisputeJobs(t, m.a)
	statuses := map[billing.PaymentMethodID]billing.PaymentMethodStatus{}
	read, err := m.a.alpha.ListPaymentMethods(t.Context(), m.sub.CustomerID, billing.PaymentMethodListParams{})
	require.NoError(t, err)
	for _, method := range read.Items {
		statuses[method.ID] = method.Status
	}
	require.Contains(t, cardStatuses(t, m.a, m.sub.CustomerID), "disputed")
	require.NotEqual(t, billing.PaymentMethodDisputed, statuses[cardB], "only the charged-back card is blocked")
	current, err := m.a.alpha.GetSubscription(t.Context(), onA)
	require.NoError(t, err)
	require.Equal(t, "active", string(current.Status), "its paid period stands")
	m.a.clock.Advance(current.CurrentPeriodEndsAt.Sub(m.a.clock.Now()))
	runRegistered(t, fleet, "openrails.billing_pass", 2)
	subscriptionState(t, m.a, onA, "awaiting_method")
	renewed := subscriptionState(t, m.a, onB, "active")
	require.True(t, renewed.CurrentPeriodEndsAt.After(*current.CurrentPeriodEndsAt), "the other card renews")
	require.Len(t, m.p.effects(t), charges+1, "only card B is charged")
}

// A chargeback blocks the card the charge was taken from, never the card the
// subscription moved to since.
func disputeChargedCardOnly(t *testing.T) {
	m := newDisputedMembership(t)
	methods, err := m.a.alpha.ListPaymentMethods(t.Context(), m.sub.CustomerID, billing.PaymentMethodListParams{})
	require.NoError(t, err)
	require.Len(t, methods.Items, 1)
	cardA := methods.Items[0].ID
	cardB := saveMethod(t, m.a, m.p, m.a.aliceToken, "1111").ID
	_, err = m.a.alpha.SetSubscriptionPaymentMethod(t.Context(), m.sub.ID, billing.SetSubscriptionPaymentMethodParams{PaymentMethodID: &cardB})
	require.NoError(t, err)
	startFleet(t, m.a.world)
	m.notice(t, "evt_switched_card", "charge.dispute.created", m.dispute("dp_switched_card", "needs_response", "chargeback", "fraudulent"))
	waitDisputeJobs(t, m.a)
	statuses := map[billing.PaymentMethodID]string{}
	rows, err := m.a.pool.Query(t.Context(), "SELECT id, status FROM "+pgx.Identifier{m.a.schema, "payment_methods"}.Sanitize()+" WHERE merchant_id = $1 AND customer_id = $2",
		m.a.alpha.MerchantID().UUID(), m.sub.CustomerID.UUID())
	require.NoError(t, err)
	for rows.Next() {
		var id uuid.UUID
		var status string
		require.NoError(t, rows.Scan(&id, &status))
		statuses[billing.PaymentMethodID(id)] = status
	}
	require.NoError(t, rows.Err())
	require.Equal(t, "disputed", statuses[cardA], "the charged card is blocked")
	require.Equal(t, "active", statuses[cardB], "the card switched to since is not")
}

// A dispute update or an early fraud warning on a charge OpenRails does not
// know is answered, and a finding names it; it is never retried.
func disputeUnknownCharge(t *testing.T) {
	m := newDisputedMembership(t)
	d := m.dispute("dp_unknown_charge", "needs_response", "chargeback", "fraudulent")
	d["charge"], d["payment_intent"] = "ch_unknown", "pi_unknown"
	m.notice(t, "evt_unknown_update", "charge.dispute.updated", d)
	m.notice(t, "evt_unknown_efw", "radar.early_fraud_warning.created", stripemock.Object{"id": "issfr_unknown", "object": "radar.early_fraud_warning",
		"charge": "ch_unknown", "payment_intent": "pi_unknown", "fraud_type": "misc", "actionable": true, "created": m.a.clock.Now().Unix()})
	page, err := m.a.alpha.ListFindings(t.Context(), billing.FindingListParams{Type: "life.disputes.unknown_charge"})
	require.NoError(t, err)
	require.Len(t, page.Items, 2, "each unbooked report is a finding")
}

// An NMI batch records every entry with its code. A matched entry is its
// charge's, reversed and linked; an unmatched one takes the masked number's
// network, books nothing and still counts in the monitor.
func disputeNMIBatch(t *testing.T) {
	a, _, sub := nmiMembership(t)
	payments, err := a.alpha.ListPayments(t.Context(), billing.PaymentListParams{SubscriptionID: sub.ID})
	require.NoError(t, err)
	require.Len(t, payments.Items, 1)
	day := payments.Items[0].CreatedAt.UTC().Format("2006-01-02")
	body, err := json.Marshal(map[string]any{"event_id": "nmi-chargeback-batch", "event_type": "chargeback.batch.complete", "event_body": map[string]any{
		"merchant": map[string]string{"id": "critical-gateway"}, "count": 2,
		"chargebacks": []map[string]any{
			{"id": "cb-matched", "date": day, "cc_number": "xxxxxxxxxxxx4242", "amount": "10.00", "reason_code": "10.4", "reason": "Other Fraud - Card Absent Environment"},
			{"id": "cb-unmatched", "date": day, "cc_number": "5555xxxxxxxx9999", "amount": "25.00", "reason_code": "4837", "reason": "No Cardholder Authorization"},
		}}})
	require.NoError(t, err)
	at := fmt.Sprint(time.Now().Unix())
	h := hmac.New(sha256.New, []byte("critical-nmi-webhook"))
	_, _ = h.Write([]byte(at + "."))
	_, _ = h.Write(body)
	r, err := http.NewRequestWithContext(t.Context(), http.MethodPost, a.server.URL+"/v1/webhooks/nmi/critical-gateway", bytes.NewReader(body))
	require.NoError(t, err)
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Webhook-Signature", "t="+at+",s="+hex.EncodeToString(h.Sum(nil)))
	response, err := a.server.Client().Do(r)
	require.NoError(t, err)
	defer response.Body.Close()
	raw, err := io.ReadAll(response.Body)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, response.StatusCode, string(raw))

	recorded, err := a.alpha.ListDisputes(t.Context(), billing.DisputeListParams{})
	require.NoError(t, err)
	cases := map[string]billing.Dispute{}
	for _, d := range recorded.Items {
		cases[d.ProviderRef] = d
	}
	require.Len(t, cases, 2, "every entry is a case")
	matched, unmatched := cases["cb-matched"], cases["cb-unmatched"]
	require.Equal(t, billing.DisputeChargeback, matched.Kind)
	require.Equal(t, billing.DisputeUnreported, matched.Status, "NMI reports no outcome")
	require.Equal(t, billing.DisputeFraud, matched.Reason)
	require.Equal(t, "10.4", matched.ReasonCode)
	require.NotNil(t, matched.PaymentID)
	require.Equal(t, payments.Items[0].ID, *matched.PaymentID)
	require.NotNil(t, matched.ReversalPaymentID, "the matched reversal is linked")
	require.Nil(t, unmatched.PaymentID)
	require.Nil(t, unmatched.ReversalPaymentID, "nothing to book")
	require.Equal(t, billing.NetworkMastercard, unmatched.Network)
	require.Equal(t, billing.DisputeFraud, unmatched.Reason)
	subscriptionState(t, a, sub.ID, "canceled")
	require.EqualValues(t, 1, standing(t, a, billing.ProgramMastercardECP, a.clock.Now()).Count, "the unmatched chargeback counts")
}

// The network rollup is derived, so an archive carries none: a restored book
// rebuilds it from its payments, disputes and fraud reports, equal to the
// source's, and its program standings with it.
func disputeRollupRestore(t *testing.T) {
	s := newSales(t)
	first, second := s.buy(t, "rollup-first"), s.buy(t, "rollup-second")
	s.chargeback(t, first.ID, "dp_rollup")
	ch := s.charges[second.ID]
	body := stripeNotice(t, "evt_rollup_efw", "radar.early_fraud_warning.created", stripemock.Object{"id": "issfr_rollup", "object": "radar.early_fraud_warning",
		"charge": ch[0], "payment_intent": ch[1], "fraud_type": "misc", "actionable": false, "created": s.a.clock.Now().Unix()})
	status, raw := callback(t, s.a, "acct_e2e", body, stripeSignature(body, "whsec_critical_fixture"))
	require.Equal(t, http.StatusOK, status, raw)

	rollup := func(w *world) string {
		t.Helper()
		var out string
		require.NoError(t, w.pool.QueryRow(t.Context(), "SELECT COALESCE(jsonb_agg(to_jsonb(r) ORDER BY psp_id, network, currency, day_at), '[]')::text FROM "+
			pgx.Identifier{w.schema, "network_activity_daily"}.Sanitize()+" r WHERE merchant_id = $1", s.a.alpha.MerchantID().UUID()).Scan(&out))
		return out
	}
	source := rollup(s.a.world)
	require.Contains(t, source, `"chargebacks": 1`)
	require.Contains(t, source, `"fraud_reports": 1`)
	archive := archiveBytes(t, s.a.alpha)
	targetWorld, target := archiveTarget(t, s.a.world)
	_, err := target.ImportBillingArchive(t.Context(), bytes.NewReader(archive))
	require.NoError(t, err)
	require.JSONEq(t, source, rollup(targetWorld), "the restored rollup is the source's")
	at := s.a.clock.Now()
	restored, err := target.ListDisputePrograms(t.Context(), billing.DisputeProgramListParams{At: &at})
	require.NoError(t, err)
	for _, st := range restored.Items {
		if st.Program == billing.ProgramVisaVAMP {
			require.EqualValues(t, 2, st.Count, "the chargeback and the fraud report")
			require.EqualValues(t, 2, st.Denominator)
		}
	}
}

// waiting is the open finding that a dispute response waits, if any.
func waiting(t *testing.T, a *authorityWorld) *billing.Finding {
	t.Helper()
	page, err := a.alpha.ListFindings(t.Context(), billing.FindingListParams{Type: "life.disputes.response_waiting"})
	require.NoError(t, err)
	for _, f := range page.Items {
		if f.ResolvedAt == nil {
			return &f
		}
	}
	return nil
}

// setPosture stores the merchant's writes as an operator would.
func setPosture(t *testing.T, a *authorityWorld, mode string) {
	t.Helper()
	setWritePosture(t, a.world, mode, a.alpha.MerchantID())
}

// armMerchant arms the merchant's destructive actions as an operator would.
func armMerchant(t *testing.T, a *authorityWorld) {
	t.Helper()
	setControl(t, a.world, "destructive_armed", true, a.alpha.MerchantID())
}

// absorbedRefund is the provider refund a prevented dispute took as its own.
func absorbedRefund(t *testing.T, a *authorityWorld, dispute string) string {
	t.Helper()
	var ref *string
	require.NoError(t, a.pool.QueryRow(t.Context(), "SELECT absorbed_refund_ref FROM "+pgx.Identifier{a.schema, "disputes"}.Sanitize()+
		" WHERE merchant_id = $1 AND provider_ref = $2", a.alpha.MerchantID().UUID(), dispute).Scan(&ref))
	if ref == nil {
		return ""
	}
	return *ref
}
