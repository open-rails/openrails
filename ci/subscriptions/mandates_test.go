//go:build e2e && integration

package subscriptions_test

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/billing"
)

// mandates is the customer's mandates as the Client lists them.
func (w *world) mandates(tp topology, customer billing.CustomerID) []billing.Mandate {
	w.t.Helper()
	page, err := w.client[tp].ListMandates(w.t.Context(), customer, billing.MandateListParams{PageRequest: billing.PageRequest{Limit: 100}})
	require.NoError(w.t, err)
	return page.Items
}

func (w *world) mandate(tp topology, customer billing.CustomerID, id billing.MandateID) billing.Mandate {
	w.t.Helper()
	for _, m := range w.mandates(tp, customer) {
		if m.ID == id {
			return m
		}
	}
	w.t.Fatalf("mandate %s not listed", id)
	return billing.Mandate{}
}

// subscriptionMandate is the subscription's one live recurring mandate.
func (w *world) subscriptionMandate(tp topology, customer billing.CustomerID, sub billing.SubscriptionID) billing.Mandate {
	w.t.Helper()
	var live []billing.Mandate
	for _, m := range w.mandates(tp, customer) {
		if m.Kind == billing.MandateRecurring && m.SubscriptionID != nil && *m.SubscriptionID == sub && (m.Status == billing.MandateActive || m.Status == billing.MandateRequiresReconsent) {
			live = append(live, m)
		}
	}
	require.Len(w.t, live, 1, "one live recurring mandate per subscription")
	return live[0]
}

// A saved card and a subscription on it each hold their agreement: the card's
// verification stores it for reuse, the enrollment charge stores the
// subscription's recurring agreement. A renewal is a merchant-initiated
// charge that sends that mandate's reference, and its attempt records the
// mandate and what it sent. Another customer sees none of them.
func TestRenewalSendsItsMandateReferences(t *testing.T) {
	t.Parallel()
	for _, tp := range []topology{embedded, remote} {
		t.Run(string(tp), func(t *testing.T) {
			t.Parallel()
			w := newWorld(t)
			e := enroll(t, w, "nmi", tp)
			card := w.storedCard(e.method)
			saved := w.nmi.Validations(card.vault)
			require.Len(t, saved, 1)
			enrollment := w.nmi.LastSale()
			require.Equal(t, "customer", enrollment.InitiatedBy)
			require.Equal(t, "stored", enrollment.Indicator, "a new subscription's charge stores its agreement")
			require.Empty(t, enrollment.Initial)

			all := w.mandates(tp, e.c.cid())
			require.Len(t, all, 2)
			recurring := w.subscriptionMandate(tp, e.c.cid(), e.sub)
			require.Equal(t, enrollment.TransactionID, str(recurring.InitialTransactionID))
			require.Equal(t, w.psp["nmi"], recurring.PSPID, "references belong to the account that ran the storing charge")
			require.Equal(t, billing.MandateActive, recurring.Status)
			require.Equal(t, "visa", str(recurring.CardBrand))
			require.Nil(t, recurring.EndReason)
			require.NotNil(t, recurring.StoringAttemptID)
			for _, m := range all {
				if m.Kind == billing.MandateCardOnFile {
					require.Equal(t, saved[0].TransactionID, str(m.InitialTransactionID), "the save stored the card for reuse")
					require.Equal(t, card.onFileRef, str(m.InitialTransactionID))
					require.Nil(t, m.SubscriptionID)
					require.Nil(t, m.Currency)
				}
			}
			require.Empty(t, w.mandates(tp, w.newCustomer().cid()), "mandates are the customer's own")

			sales := len(w.nmi.ledger(""))
			end := e.periodEnd()
			e.toFreshPeriodEnd()
			w.runRenewals()
			require.True(t, e.periodEnd().After(end), "the membership renews")
			require.Len(t, w.nmi.ledger(""), sales+1)
			renewal := w.nmi.LastSale()
			require.Equal(t, "merchant", renewal.InitiatedBy)
			require.Equal(t, "used", renewal.Indicator)
			require.Equal(t, str(recurring.InitialTransactionID), renewal.Initial, "the renewal sends its mandate's reference")

			attempts, err := w.client[tp].ListPaymentAttempts(t.Context(), billing.PaymentAttemptListParams{SubscriptionID: e.sub, Kind: []string{"rebill"}})
			require.NoError(t, err)
			require.Len(t, attempts.Items, 1)
			require.NotNil(t, attempts.Items[0].MandateID)
			require.Equal(t, recurring.ID, *attempts.Items[0].MandateID)
			require.Equal(t, renewal.Initial, attempts.Items[0].SentInitialTransactionID)
			require.Empty(t, w.nmi.Unexpected())
		})
	}
}

// A merchant-initiated charge runs only under an active mandate: a renewal
// whose mandate waits for the customer's consent sends nothing and waits for
// the member, and the customer's own collection agreement covers its
// currency only while it is active.
func TestMITOnNonActiveMandateIsRefused(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	e := enroll(t, w, "nmi", embedded)
	recurring := w.subscriptionMandate(embedded, e.c.cid(), e.sub)
	w.setMandateStatus(recurring.ID, billing.MandateRequiresReconsent)

	sales, sent := len(w.nmi.ledger("")), len(w.nmi.Attempts())
	end := e.periodEnd()
	e.toFreshPeriodEnd()
	w.runRenewals()
	require.Len(t, w.nmi.ledger(""), sales, "no merchant-initiated charge without an active mandate")
	require.Len(t, w.nmi.Attempts(), sent, "nothing reached the gateway")
	sub := w.subscription(embedded, e.sub)
	require.True(t, sub.CurrentPeriodEndsAt.Equal(end), "the unpaid period is not granted")
	require.Equal(t, billing.SubscriptionAwaitingMethod, sub.Status, "the renewal waits for the member")

	// Consent restored: the next pass renews with the mandate's reference.
	w.setMandateStatus(recurring.ID, billing.MandateActive)
	e.c.must(http.MethodPut, "/subscriptions/"+e.sub.String()+"/payment-method", "", map[string]any{"payment_method_id": e.method})
	w.runRenewals()
	require.Len(t, w.nmi.ledger(""), sales+1)
	require.Equal(t, str(recurring.InitialTransactionID), w.nmi.LastSale().Initial)
}

// setMandateStatus stands in for the account updater's brand change, which
// sends a mandate to requires_reconsent, and the customer's re-consent.
func (w *world) setMandateStatus(id billing.MandateID, status billing.MandateStatus) {
	w.t.Helper()
	tag, err := w.pool.Exec(w.t.Context(), w.q(`UPDATE billing.mandates SET status = $2, updated_at = now() WHERE id = $1::uuid`), id.UUID(), string(status))
	require.NoError(w.t, err)
	require.EqualValues(w.t, 1, tag.RowsAffected())
}

// Deleting a card ends its agreements; the mandates stay as evidence without
// the card.
func TestDeletedCardEndsItsMandates(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	c := w.newCustomer()
	method := c.saveCard("nmi", visa)
	saved := w.mandates(embedded, c.cid())
	require.Len(t, saved, 1)
	require.Equal(t, billing.MandateCardOnFile, saved[0].Kind)
	require.Equal(t, billing.MandateActive, saved[0].Status)

	status, body := c.call(http.MethodDelete, "/payment-methods/"+method, "", nil)
	require.Contains(t, []int{http.StatusAccepted, http.StatusNoContent}, status, "%v", body)
	w.settle()
	ended := w.mandate(embedded, c.cid(), saved[0].ID)
	require.Equal(t, billing.MandateEnded, ended.Status)
	require.Equal(t, billing.MandateEndPaymentMethodRemoved, *ended.EndReason)
	require.NotNil(t, ended.EndedAt)
	require.Nil(t, ended.PaymentMethodID, "the card is gone")
	require.Equal(t, saved[0].InitialTransactionID, ended.InitialTransactionID, "its references stay")
}
