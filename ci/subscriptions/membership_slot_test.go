//go:build e2e && integration

package subscriptions_test

import (
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/billing"
)

// A customer holds one membership of a product or tier group, however it is
// bought: a customer's order, a merchant's order or a Solana subscribe each
// see what the others hold or are still buying, before any money moves.

func (w *world) charges() int { return len(w.nmi.ledger("")) + len(w.stripe.ledger("")) }

func (w *world) memberships(c *customer) []billing.Subscription {
	w.t.Helper()
	subs, err := w.client[embedded].ListSubscriptions(w.t.Context(), billing.SubscriptionListParams{CustomerID: c.customerID()})
	require.NoError(w.t, err)
	return subs.Items
}

// An unpaid order holds its membership: a merchant order for the same
// membership meanwhile is refused, naming it, and paying it charges once.
func TestUnpaidOrderHoldsMembershipSlot(t *testing.T) {
	t.Parallel()
	w := orderWorld(t)
	member := w.membership("slot:order-first", 10_000_000)
	c := w.newCustomer()
	card := c.saveCard("nmi", visa)
	open := c.order(http.MethodPost, "/orders", "open-"+uuid.NewString(), map[string]any{"lines": []any{line(member, 0)}})
	require.Equal(t, http.StatusCreated, open.status, "%v", open.body)
	require.Equal(t, "open", open.body["status"])

	_, err := c.sell(embedded, order{price: member.ID})
	require.Equal(t, open.body["id"], requireSlotHeld(t, err).Metadata["owned_by"])
	paid := c.order(http.MethodPost, "/orders/"+open.body["id"].(string)+"/pay", "pay-"+uuid.NewString(), map[string]any{"payment": map[string]any{"payment_method_id": card}, "expected_total": micros(10_000_000)})
	w.settle()

	require.Equal(t, 1, w.charges(), "the customer pays for the membership once")
	require.Equal(t, "complete", paid.body["status"], "%v", paid.body)
	require.Len(t, w.memberships(c), 1)
}

// An enrollment awaiting the issuer's challenge holds its membership: another
// order for it meanwhile is refused, so authenticating charges once.
func TestUnresolvedEnrollmentHoldsMembershipSlot(t *testing.T) {
	t.Parallel()
	w := orderWorld(t)
	member := w.membership("slot:enrollment-first", 9_990_000)
	c := w.newCustomer()
	challenged := c.saveCard("stripe", card{Brand: "visa", Last4: "3155", Decline: "auth"})
	enrolling, err := c.buyOrder(embedded, order{price: member.ID, rail: "stripe", method: challenged})
	require.NoError(t, err)
	require.Equal(t, "requires_action", enrolling.Status, "%+v", enrolling.Order)

	other := c.saveCard("nmi", visa)
	bought := c.order(http.MethodPost, "/orders", "buy-"+uuid.NewString(), map[string]any{"lines": []any{line(member, 0)}, "expected_total": micros(9_990_000), "payment": map[string]any{"payment_method_id": other}})
	require.True(t, w.stripe.Authenticate(enrolling.NextAction.Payload["payment_intent_id"]))
	confirmed := c.order(http.MethodPost, "/orders/"+enrolling.Order.ID.String()+"/confirm", "", nil)
	require.Equal(t, "complete", confirmed.body["status"], "%v", confirmed.body)

	require.Equal(t, 1, w.charges(), "the customer pays for the membership once")
	require.Equal(t, http.StatusConflict, bought.status, "%v", bought.body)
	require.Equal(t, "already_owned", orderError(bought))
	w.until(func() bool { return len(w.memberships(c)) == 1 }, "the authenticated enrollment completes")
}

// A canceled membership whose NMI schedule may still bill holds its slot
// after its paid period ends: an order for it is refused without a charge
// until the delete is verified.
func TestPendingProviderStopHoldsSlotAgainstOrders(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.armDestructive()
	l := importLegacy(t, w, "nmi", remote)
	w.converge()
	end := l.periodEnd()
	w.advanceHealthyTo(end.Add(-time.Hour))
	method := l.c.saveCard("nmi", visa)
	var failing atomic.Bool
	failing.Store(true)
	nmiDeleteFails(w, &failing)
	status, body := l.meCancel(l.sub)
	require.Equal(t, http.StatusOK, status, "%v", body)
	w.settle()
	w.advance(end.Add(time.Hour).Sub(w.clock.Now()))
	require.NotNil(t, w.subscription(remote, l.sub).DeletionScheduledAt, "the provider stop has not succeeded")
	before := len(w.nmi.Ledger(""))

	buy := func() orderCall {
		return l.c.order(http.MethodPost, "/orders", "buy-"+uuid.NewString(), map[string]any{"lines": []any{line(l.ourPrice(), 0)}, "expected_total": micros(9_990_000), "payment": map[string]any{"payment_method_id": method}})
	}
	refused := buy()
	require.Len(t, w.nmi.Ledger(""), before, "nothing is charged while the old schedule may still bill")
	require.Equal(t, "already_owned", orderError(refused), "%v", refused.body)
	require.Contains(t, errorMeta(refused, "owned_by"), billing.SubscriptionIDPrefix)

	failing.Store(false)
	w.until(func() bool { return w.subscription(remote, l.sub).DeletionScheduledAt == nil }, "the verified stop releases the slot")
	require.Equal(t, "complete", buy().body["status"])
	require.Len(t, w.nmi.Ledger(""), before+1, "one charge for the new membership")
}

// An unpaid order holds a lifetime product too: a merchant order for it
// meanwhile is refused, and paying the first charges once.
func TestUnpaidOrderHoldsLifetimeProduct(t *testing.T) {
	t.Parallel()
	w := orderWorld(t)
	life := w.lifetime("slot:lifetime", 25_000_000)
	c := w.newCustomer()
	card := c.saveCard("nmi", visa)
	open := c.order(http.MethodPost, "/orders", "open-"+uuid.NewString(), map[string]any{"lines": []any{line(life, 0)}})
	require.Equal(t, http.StatusCreated, open.status, "%v", open.body)

	_, err := c.sell(embedded, order{price: life.ID})
	require.Equal(t, open.body["id"], requireSlotHeld(t, err).Metadata["owned_by"])
	paid := c.order(http.MethodPost, "/orders/"+open.body["id"].(string)+"/pay", "pay-"+uuid.NewString(), map[string]any{"payment": map[string]any{"payment_method_id": card}, "expected_total": micros(25_000_000)})
	w.settle()

	require.Equal(t, 1, w.charges(), "the customer pays for the product once")
	require.Equal(t, "complete", paid.body["status"], "%v", paid.body)
	require.True(t, c.entitled("slot:lifetime"))
}
