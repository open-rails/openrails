//go:build integration

package critical_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/stripemock"
)

// TestDisputes is journey J46: chargebacks, inquiries and fraud reports.
func TestDisputes(t *testing.T) {
	t.Run("OR-J46-S02/an-inquiry-books-and-cancels-nothing", disputeInquiry)
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
