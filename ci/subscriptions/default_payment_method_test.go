//go:build e2e && integration

package subscriptions_test

import (
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/billing"
)

// defaults maps each of the customer's cards to the currencies it is the
// default for, as the Client lists them.
func (c *customer) defaults(tp topology) map[string][]string {
	c.w.t.Helper()
	page, err := c.w.client[tp].ListPaymentMethods(c.w.t.Context(), c.cid(), billing.PaymentMethodListParams{PageRequest: billing.PageRequest{Limit: 100}})
	require.NoError(c.w.t, err)
	out := map[string][]string{}
	for _, m := range page.Items {
		out[m.ID.String()] = m.DefaultCurrencies
	}
	return out
}

// setDefault makes method the customer's default card for USD.
func (c *customer) setDefault(method string) {
	c.w.t.Helper()
	set := c.must(http.MethodPut, "/default-payment-methods/USD", "", map[string]any{"payment_method_id": method})
	require.Equal(c.w.t, map[string]any{"currency": "USD", "payment_method_id": method}, set)
}

// setOwnCard gives a subscription its own card, or with "" makes it follow
// the default.
func (c *customer) setOwnCard(sub billing.SubscriptionID, method string) map[string]any {
	c.w.t.Helper()
	var body any
	if method != "" {
		body = method
	}
	return c.must(http.MethodPut, "/subscriptions/"+sub.String()+"/payment-method", "", map[string]any{"payment_method_id": body})
}

// recurringVerifications are the $0 verifications declaring a recurring
// agreement on a card's vault.
func (w *world) recurringVerifications(method string) []string {
	w.t.Helper()
	var out []string
	for _, v := range w.nmi.Validations(w.vaultOf(method)) {
		if v.Form.Get("billing_method") == "recurring" {
			out = append(out, v.TransactionID)
		}
	}
	return out
}

// ownCard is the subscription's own card, "" when it follows the default,
// and the last four of the card that pays it.
func (w *world) ownCard(tp topology, id billing.SubscriptionID) (own, paidBy string) {
	w.t.Helper()
	sub := w.subscription(tp, id)
	if sub.PaymentMethodID != nil {
		own = sub.PaymentMethodID.String()
	}
	if sub.Card != nil {
		paidBy = str(sub.Card.Last4)
	}
	return own, paidBy
}

// A subscription without its own card follows the customer's per-currency
// default: changing the default moves every follower with its agreement, citing
// one recurring verification of the new card, and renewals charge that card.
func TestDefaultCardMovesFollowingSubscriptions(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	c := w.newCustomer()
	first := c.saveCard("nmi", visa)
	second := c.saveCard("nmi", mastercard)
	third := c.saveCard("nmi", card{Brand: "visa", Last4: "1111"})
	require.Equal(t, map[string][]string{first: {}, second: {}, third: {}}, c.defaults(embedded), "a saved card is no default until named")
	c.setDefault(first)
	require.Equal(t, map[string][]string{first: {"USD"}, second: {}, third: {}}, c.defaults(embedded))

	a := w.membership("content:a", 9_990_000)
	b := w.membership("content:b", 4_990_000)
	subA := c.subscribeAgain(embedded, "nmi", a.ID.String(), "content:a", first)
	subB := c.subscribeAgain(embedded, "nmi", b.ID.String(), "content:b", first)
	for _, sub := range []billing.SubscriptionID{subA, subB} {
		own, paidBy := w.ownCard(embedded, sub)
		require.Equal(t, []string{"", visa.Last4}, []string{own, paidBy}, "bought with the default card, it follows the default")
	}

	// An override pins subB to the second card, verified for recurring use.
	c.setOwnCard(subB, second)
	require.Len(t, w.recurringVerifications(second), 1, "a card without a recurring agreement is verified first")
	own, paidBy := w.ownCard(embedded, subB)
	require.Equal(t, []string{second, mastercard.Last4}, []string{own, paidBy})
	require.Equal(t, second, w.subscriptionMandate(embedded, c.cid(), subB).PaymentMethodID.String())

	// The default moves; only the subscription following it moves with it.
	c.setDefault(third)
	verified := w.recurringVerifications(third)
	require.Len(t, verified, 1, "one storing verification for the change")
	require.Equal(t, map[string][]string{first: {}, second: {}, third: {"USD"}}, c.defaults(embedded))
	own, paidBy = w.ownCard(embedded, subA)
	require.Equal(t, []string{"", "1111"}, []string{own, paidBy})
	moved := w.subscriptionMandate(embedded, c.cid(), subA)
	require.Equal(t, third, moved.PaymentMethodID.String())
	require.Equal(t, verified[0], str(moved.InitialTransactionID), "the moved agreement cites the verification")
	own, _ = w.ownCard(embedded, subB)
	require.Equal(t, second, own, "an override stays put")
	require.Equal(t, second, w.subscriptionMandate(embedded, c.cid(), subB).PaymentMethodID.String())
	page, err := w.client[embedded].ListPaymentMethods(t.Context(), c.cid(), billing.PaymentMethodListParams{PageRequest: billing.PageRequest{Limit: 100}})
	require.NoError(t, err)
	for _, m := range page.Items {
		var paid []billing.SubscriptionID
		for _, s := range m.Subscriptions {
			paid = append(paid, s.ID)
		}
		switch m.ID.String() {
		case third:
			require.Equal(t, []billing.SubscriptionID{subA}, paid, "the default lists the subscriptions following it")
		case second:
			require.Equal(t, []billing.SubscriptionID{subB}, paid)
		default:
			require.Empty(t, paid)
		}
	}

	// Renewals charge each subscription's effective card under its agreement.
	sales := len(w.nmi.Sales())
	w.advanceHealthyTo(w.subscription(embedded, subA).CurrentPeriodEndsAt.Add(time.Second))
	w.runRenewals()
	renewals := w.nmi.Sales()[sales:]
	require.Len(t, renewals, 2)
	byVault := map[string]string{}
	for _, sale := range renewals {
		require.Equal(t, "merchant", sale.InitiatedBy)
		byVault[sale.Vault] = sale.Initial
	}
	require.Equal(t, verified[0], byVault[w.vaultOf(third)], "the follower renews on the default")
	require.Equal(t, w.recurringVerifications(second)[0], byVault[w.vaultOf(second)], "the override renews on its own card")

	// Cleared, the override follows the default under the card's agreement.
	cleared := c.setOwnCard(subB, "")
	require.Nil(t, cleared["payment_method_id"])
	own, paidBy = w.ownCard(embedded, subB)
	require.Equal(t, []string{"", "1111"}, []string{own, paidBy})
	require.Len(t, w.recurringVerifications(third), 1, "the default's agreement is cited, not verified again")
	require.Equal(t, verified[0], str(w.subscriptionMandate(embedded, c.cid(), subB).InitialTransactionID))
	require.Empty(t, w.nmi.Unexpected())
}

// A default that cannot pay a subscription following it is refused, and
// nothing moves; following a default the customer never named is refused.
func TestDefaultCardMustPayItsFollowers(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	c := w.newCustomer()
	method := c.saveCard("nmi", visa)
	price := w.membership("content:members", 9_990_000)
	sub := c.subscribe(embedded, "nmi", price.ID.String(), "content:members", method)

	status, body := c.call(http.MethodPut, "/subscriptions/"+sub.String()+"/payment-method", "", map[string]any{"payment_method_id": nil})
	require.Equal(t, http.StatusBadRequest, status, "%v", body)
	require.Equal(t, billing.CodeDefaultPaymentMethodRequired, body["error"].(map[string]any)["code"])

	c.setDefault(method)
	c.setOwnCard(sub, "")
	stripe := c.saveCard("stripe", mastercard)
	status, body = c.call(http.MethodPut, "/default-payment-methods/USD", "", map[string]any{"payment_method_id": stripe})
	require.Equal(t, http.StatusConflict, status, "%v", body)
	require.Equal(t, billing.CodePaymentMethodPSPMismatch, body["error"].(map[string]any)["code"])
	require.Equal(t, map[string][]string{method: {"USD"}, stripe: {}}, c.defaults(embedded), "the refused default changed nothing")
	require.Equal(t, method, w.subscriptionMandate(embedded, c.cid(), sub).PaymentMethodID.String())
}

// Staff move a subscription onto another of the customer's saved cards and
// back to the default, through the Client in process and remotely. A
// verification of the card anchors the moved agreement.
func TestStaffSetsSubscriptionCard(t *testing.T) {
	t.Parallel()
	for _, tp := range []topology{embedded, remote} {
		t.Run(string(tp), func(t *testing.T) {
			t.Parallel()
			w := newWorld(t)
			e := enroll(t, w, "nmi", tp)
			e.c.setDefault(e.method)
			other := e.c.saveCard("nmi", mastercard)
			id := pmid(other)

			moved, err := w.client[tp].SetSubscriptionPaymentMethod(t.Context(), e.sub, billing.SetSubscriptionPaymentMethodParams{PaymentMethodID: &id})
			require.NoError(t, err)
			require.Equal(t, other, moved.PaymentMethodID.String())
			require.Equal(t, mastercard.Last4, str(moved.Card.Last4))
			mandate := w.subscriptionMandate(tp, e.c.cid(), e.sub)
			require.Equal(t, other, mandate.PaymentMethodID.String())
			require.Equal(t, w.recurringVerifications(other), []string{str(mandate.InitialTransactionID)})

			back, err := w.client[tp].SetSubscriptionPaymentMethod(t.Context(), e.sub, billing.SetSubscriptionPaymentMethodParams{})
			require.NoError(t, err)
			require.Nil(t, back.PaymentMethodID, "it follows the default")
			require.Equal(t, visa.Last4, str(back.Card.Last4))
			require.Equal(t, e.method, w.subscriptionMandate(tp, e.c.cid(), e.sub).PaymentMethodID.String())

			sales := len(w.nmi.Sales())
			e.toFreshPeriodEnd()
			w.runRenewals()
			require.Len(t, w.nmi.Sales(), sales+1)
			require.Equal(t, w.vaultOf(e.method), w.nmi.LastSale().Vault, "the renewal charges the default")
		})
	}
}

// An NMI schedule follows the default through the durable payment-source
// swap: clearing its own card moves the schedule onto the default, and a
// default change moves it again. Each target is verified for recurring use
// first, and the schedule's agreement cites that verification.
func TestNMIScheduleFollowsTheDefault(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	l := importLegacy(t, w, "nmi", embedded)
	w.converge()
	w.refreshProviders()
	updates := func() []providerCall {
		return calls(w.nmi.CallsTo(http.MethodPost, "transact.php", func(f url.Values) bool { return f.Get("recurring") == "update_subscription" }))
	}

	first := l.c.saveCard("nmi", mastercard)
	l.c.setDefault(first)
	require.Empty(t, updates(), "the schedule has its own card")
	l.c.setOwnCard(l.sub, "")
	w.settle()
	require.Len(t, updates(), 1)
	require.Equal(t, w.vaultOf(first), w.nmi.Schedule(l.railSub).Vault)
	own, paidBy := w.ownCard(embedded, l.sub)
	require.Equal(t, []string{"", mastercard.Last4}, []string{own, paidBy})
	verified := w.recurringVerifications(first)
	require.Len(t, verified, 1, "the target is verified for recurring use")
	require.Equal(t, verified[0], str(w.subscriptionMandate(embedded, l.c.cid(), l.sub).InitialTransactionID))

	second := l.c.saveCard("nmi", card{Brand: "visa", Last4: "1111"})
	l.c.setDefault(second)
	w.settle()
	require.Len(t, updates(), 2, "the default change swaps the schedule")
	require.Equal(t, w.vaultOf(second), w.nmi.Schedule(l.railSub).Vault)
	own, paidBy = w.ownCard(embedded, l.sub)
	require.Equal(t, []string{"", "1111"}, []string{own, paidBy})
	verified = w.recurringVerifications(second)
	require.Len(t, verified, 1)
	mandate := w.subscriptionMandate(embedded, l.c.cid(), l.sub)
	require.Equal(t, second, mandate.PaymentMethodID.String())
	require.Equal(t, verified[0], str(mandate.InitialTransactionID))
	require.Empty(t, w.nmi.Unexpected())
}

// A card that is no longer active is never a default: the bank closing the
// default card releases it, its followers wait for a new card, and a closed
// card cannot be named again.
func TestClosedCardIsNoDefault(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	c := w.newCustomer()
	method := c.saveCard("nmi", visa)
	c.setDefault(method)
	price := w.membership("content:members", 9_990_000)
	sub := c.subscribe(embedded, "nmi", price.ID.String(), "content:members", method)
	own, _ := w.ownCard(embedded, sub)
	require.Empty(t, own, "bought with the default card, it follows the default")

	require.Equal(t, http.StatusOK, w.deliver("nmi", acuNotice("closedaccount", w.vaultOf(method))))
	require.Equal(t, "closed", w.methodRow(method, "status"))
	require.Equal(t, map[string][]string{method: {}}, c.defaults(embedded), "a closed card is nobody's default")
	own, paidBy := w.ownCard(embedded, sub)
	require.Equal(t, []string{"", ""}, []string{own, paidBy}, "the follower has no card")

	status, body := c.call(http.MethodPut, "/default-payment-methods/USD", "", map[string]any{"payment_method_id": method})
	require.Equal(t, http.StatusBadRequest, status, "%v", body)
	require.Equal(t, billing.CodeDefaultPaymentMethodInvalid, body["error"].(map[string]any)["code"])

	sent := len(w.nmi.Attempts())
	w.advanceHealthyTo(w.subscription(embedded, sub).CurrentPeriodEndsAt.Add(time.Second))
	w.runRenewals()
	require.Len(t, w.nmi.Attempts(), sent, "nothing is charged")
	require.Equal(t, billing.SubscriptionAwaitingMethod, w.subscription(embedded, sub).Status)

	next := c.saveCard("nmi", mastercard)
	c.setDefault(next)
	require.Equal(t, next, w.subscriptionMandate(embedded, c.cid(), sub).PaymentMethodID.String(), "the new default takes the follower")
}
