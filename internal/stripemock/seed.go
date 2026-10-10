package stripemock

import (
	"fmt"
	"net/http"
	"net/url"
	"time"
)

// monthHours is a provider-owned subscription's period.
const monthHours = 720

// CompleteSetup is the browser confirming a SetupIntent with a new card: the
// card becomes a payment method of the SetupIntent's customer.
func (m *Mock) CompleteSetup(setupIntent string, c Card) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.setups[setupIntent]
	pm := m.id("pm")
	m.methods[pm] = Object{"object": "payment_method", "id": pm, "type": "card", "customer": s["customer"], "livemode": false,
		"card": Object{"brand": c.Brand, "last4": c.Last4, "exp_month": 12, "exp_year": 2035}}
	m.declines[pm] = c.Decline
	s["status"], s["payment_method"] = "succeeded", pm
	return pm
}

// Authenticate completes the issuer challenge on a PaymentIntent awaiting
// it, as the customer's browser does with its client secret.
func (m *Mock) Authenticate(paymentIntent string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	pi, ok := m.intents[paymentIntent]
	if !ok || pi["status"] != "requires_action" {
		return false
	}
	m.chargeLocked(pi)
	return true
}

// ReissueCard is Stripe's card updater giving payment method pm a new card.
func (m *Mock) ReissueCard(pm, brand, last4 string, month int, fingerprint string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.methods[pm]["card"] = Object{"brand": brand, "last4": last4, "exp_month": month, "exp_year": 2036, "fingerprint": fingerprint}
}

// Refund refunds amount (minor units; 0 is the rest) of a charge outside
// OpenRails, as a dashboard refund does, and returns the refund.
func (m *Mock) Refund(charge string, amount int64) (Object, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	form := url.Values{"charge": {charge}}
	if amount > 0 {
		form.Set("amount", fmt.Sprint(amount))
	}
	status, out := m.createRefund(form)
	if status != http.StatusOK {
		return nil, fmt.Errorf("stripemock: refund of %s answered %d", charge, status)
	}
	return clone(out.(Object)), nil
}

// SetLegacyPrice gives a legacy monthly price (id price_legacy_…) amount
// cents; one never set is 9.99.
func (m *Mock) SetLegacyPrice(id string, amount int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.amounts == nil {
		m.amounts = map[string]int64{}
	}
	m.amounts[id] = amount
}

// AddSubscription seeds a provider-owned subscription paid for its current
// period, in the pinned API version's shape (period on the item, charge under
// invoice.payments). It returns the subscription id.
func (m *Mock) AddSubscription(customer, method, price string, amount int64, start, end time.Time) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.customers[customer]; !ok {
		m.customers[customer] = Object{"object": "customer", "id": customer, "metadata": map[string]string{}, "livemode": false, "invoice_settings": Object{"default_payment_method": method}}
	}
	m.methods[method] = Object{"object": "payment_method", "id": method, "type": "card", "customer": customer, "livemode": false, "card": Object{"brand": "visa", "last4": "4242", "exp_month": 12, "exp_year": 2035}}
	id := m.id("sub")
	m.subs[id] = Object{"object": "subscription", "id": id, "customer": customer, "status": "active", "cancel_at_period_end": false, "canceled_at": nil, "livemode": false,
		"default_payment_method": method, "metadata": map[string]string{}, "currency": "usd",
		"items": Object{"object": "list", "data": []Object{{"id": m.id("si"), "price": Object{"id": price, "unit_amount": amount, "currency": "usd"}, "current_period_start": start.Unix(), "current_period_end": end.Unix()}}}}
	m.invoiceLocked(id, amount, true)
	return id
}

// invoiceLocked cuts the subscription's latest invoice; paid moves money.
func (m *Mock) invoiceLocked(sub string, amount int64, paid bool) Object {
	s := m.subs[sub]
	price := s["items"].(Object)["data"].([]Object)[0]["price"].(Object)["id"]
	inv := Object{"object": "invoice", "id": m.id("in"), "customer": s["customer"], "currency": "usd", "amount_due": amount, "created": m.now().Unix(), "billing_reason": "subscription_cycle",
		"parent": Object{"type": "subscription_details", "subscription_details": Object{"subscription": sub}},
		"lines":  Object{"object": "list", "data": []Object{{"object": "line_item", "amount": amount, "pricing": Object{"type": "price_details", "price_details": Object{"price": price}}}}}}
	if paid {
		pi := Object{"object": "payment_intent", "id": m.id("pi"), "amount": amount, "currency": "usd", "customer": s["customer"], "payment_method": s["default_payment_method"], "livemode": false, "metadata": map[string]string{}}
		m.intents[pi["id"].(string)] = pi
		m.order = append(m.order, pi["id"].(string))
		ch := m.chargeLocked(pi)
		inv["status"], inv["amount_paid"] = "paid", amount
		inv["payments"] = Object{"object": "list", "data": []Object{{"object": "invoice_payment", "status": "paid", "payment": Object{"type": "payment_intent", "payment_intent": pi["id"], "charge": ch["id"]}}}}
	} else {
		inv["status"], inv["amount_paid"], inv["attempt_count"] = "open", int64(0), int64(1)
		inv["next_payment_attempt"] = m.now().Add(72 * time.Hour).Unix()
		inv["payments"] = Object{"object": "list", "data": []Object{}}
	}
	s["latest_invoice"] = inv
	return inv
}

func (m *Mock) changePrice(sub, price string, amount, proration int64, paid bool) Object {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.subs[sub]
	s["items"].(Object)["data"].([]Object)[0]["price"] = Object{"id": price, "unit_amount": amount, "currency": "usd"}
	inv := m.invoiceLocked(sub, proration, paid)
	inv["billing_reason"] = "subscription_update"
	return clone(s)
}

// PortalPriceChange is the customer switching price in Stripe's portal: the
// item moves at once and Stripe invoices the proration, left open.
func (m *Mock) PortalPriceChange(sub, price string, amount, proration int64) Object {
	return m.changePrice(sub, price, amount, proration, false)
}

// PortalPriceChangePaid is a portal price switch whose proration invoice
// Stripe charged at once.
func (m *Mock) PortalPriceChangePaid(sub, price string, amount, proration int64) Object {
	return m.changePrice(sub, price, amount, proration, true)
}

// RenewSubscription is Stripe billing its subscription for the next period;
// it returns the invoice.
func (m *Mock) RenewSubscription(sub string, paid bool) Object {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.subs[sub]
	item := s["items"].(Object)["data"].([]Object)[0]
	start, end := item["current_period_end"].(int64), item["current_period_end"].(int64)+monthHours*3600
	item["current_period_start"], item["current_period_end"] = start, end
	amount := item["price"].(Object)["unit_amount"].(int64)
	s["status"] = "active"
	if !paid {
		s["status"] = "past_due"
	}
	return clone(m.invoiceLocked(sub, amount, paid))
}

// DraftRenewal is Stripe rolling the subscription into its next period with
// a draft invoice it has not finalized or tried to collect; it returns the
// subscription.
func (m *Mock) DraftRenewal(sub string) Object {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.subs[sub]
	item := s["items"].(Object)["data"].([]Object)[0]
	start := item["current_period_end"].(int64)
	item["current_period_start"], item["current_period_end"] = start, start+monthHours*3600
	s["latest_invoice"] = Object{"object": "invoice", "id": m.id("in"), "customer": s["customer"], "currency": "usd", "status": "draft", "amount_due": item["price"].(Object)["unit_amount"], "amount_paid": int64(0),
		"attempt_count": int64(0), "created": m.now().Unix(), "billing_reason": "subscription_cycle", "payments": Object{"object": "list", "data": []Object{}},
		"parent": Object{"type": "subscription_details", "subscription_details": Object{"subscription": sub}}}
	return clone(s)
}

// CollectDraft finalizes and pays the draft invoice; it returns the invoice.
func (m *Mock) CollectDraft(sub string) Object {
	m.mu.Lock()
	defer m.mu.Unlock()
	amount := m.subs[sub]["items"].(Object)["data"].([]Object)[0]["price"].(Object)["unit_amount"].(int64)
	return clone(m.invoiceLocked(sub, amount, true))
}

// CancelSubscription is the subscription ended at Stripe (dashboard or
// dunning); it returns the subscription.
func (m *Mock) CancelSubscription(sub string) Object {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.subs[sub]
	s["status"], s["canceled_at"] = "canceled", m.now().Unix()
	return clone(s)
}

// SetSubscriptionStatus moves a subscription to status; retrying says
// whether Stripe still has a payment attempt scheduled on its open invoice.
// It returns the subscription.
func (m *Mock) SetSubscriptionStatus(sub, status string, retrying bool) Object {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.subs[sub]
	s["status"] = status
	if inv, ok := s["latest_invoice"].(Object); ok && inv["status"] == "open" {
		inv["next_payment_attempt"] = nil
		if retrying {
			inv["next_payment_attempt"] = m.now().Add(72 * time.Hour).Unix()
		}
	}
	return clone(s)
}

// EditCard is a change to payment method pm's card through Stripe's API or
// portal (an expiry edit).
func (m *Mock) EditCard(pm string, edit func(card Object)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	edit(m.methods[pm]["card"].(Object))
}

// Detach detaches payment method pm from its customer at Stripe.
func (m *Mock) Detach(pm string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.methods[pm]["customer"] = nil
}
