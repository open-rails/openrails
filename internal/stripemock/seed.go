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

// NewPaymentMethod is Stripe Elements creating a payment method from a card
// the browser entered: it belongs to no customer until a PaymentIntent that
// saves it (setup_future_usage) succeeds.
func (m *Mock) NewPaymentMethod(c Card) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	pm := m.id("pm")
	m.methods[pm] = Object{"object": "payment_method", "id": pm, "type": "card", "customer": nil, "livemode": false,
		"card": Object{"brand": c.Brand, "last4": c.Last4, "exp_month": 12, "exp_year": 2035, "fingerprint": "fp_" + c.Brand + c.Last4}}
	m.declines[pm] = c.Decline
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

// AuthenticateSetup completes the bank's challenge on a SetupIntent awaiting
// it: the card is saved to the intent's customer.
func (m *Mock) AuthenticateSetup(setupIntent string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.setups[setupIntent]
	if !ok || s["status"] != "requires_action" {
		return false
	}
	s["status"] = "succeeded"
	if method, ok := m.methods[fmt.Sprint(s["payment_method"])]; ok && method["customer"] == nil {
		method["customer"] = s["customer"]
	}
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

// AddCustomer seeds a customer an account already has and returns its id.
func (m *Mock) AddCustomer(email, name string, metadata map[string]string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if metadata == nil {
		metadata = map[string]string{}
	}
	id := m.id("cus")
	m.customers[id] = Object{"object": "customer", "id": id, "email": email, "name": name, "metadata": metadata, "livemode": false, "created": m.now().Unix(),
		"invoice_settings": Object{"default_payment_method": nil}}
	return id
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
	item := Object{"id": price, "unit_amount": amount, "currency": "usd"}
	if stored, ok := m.prices[price]; ok {
		item["object"], item["product"], item["recurring"] = "price", stored["product"], stored["recurring"]
	}
	m.subs[id] = Object{"object": "subscription", "id": id, "customer": customer, "status": "active", "cancel_at_period_end": false, "canceled_at": nil, "livemode": false,
		"created": start.Unix(), "start_date": start.Unix(),
		"default_payment_method": method, "metadata": map[string]string{}, "currency": "usd",
		"items": Object{"object": "list", "data": []Object{{"id": m.id("si"), "price": item, "current_period_start": start.Unix(), "current_period_end": end.Unix()}}}}
	m.invoiceLocked(id, amount, true)["billing_reason"] = "subscription_create"
	return id
}

// invoiceLocked cuts the subscription's latest invoice; paid moves money.
func (m *Mock) invoiceLocked(sub string, amount int64, paid bool) Object {
	inv := m.newInvoiceLocked(sub, amount)
	inv["status"], inv["attempt_count"] = "open", int64(1)
	inv["status_transitions"].(Object)["finalized_at"] = m.now().Unix()
	if paid {
		m.payInvoiceLocked(inv)
	} else {
		inv["next_payment_attempt"] = m.now().Add(72 * time.Hour).Unix()
	}
	return inv
}

func (m *Mock) newInvoiceLocked(sub string, amount int64) Object {
	s := m.subs[sub]
	item := s["items"].(Object)["data"].([]Object)[0]
	price := item["price"].(Object)["id"]
	id := m.id("in")
	inv := Object{"object": "invoice", "id": id, "customer": s["customer"], "currency": "usd", "total": amount, "amount_due": amount,
		"amount_paid": int64(0), "amount_remaining": amount, "amount_paid_off_stripe": int64(0), "starting_balance": int64(0),
		"pre_payment_credit_notes_amount": int64(0), "post_payment_credit_notes_amount": int64(0), "created": m.now().Unix(),
		"billing_reason": "subscription_cycle", "status": "draft", "attempt_count": int64(0), "metadata": map[string]string{}, "livemode": false,
		"status_transitions": Object{"finalized_at": nil, "paid_at": nil},
		"parent":             Object{"type": "subscription_details", "subscription_details": Object{"subscription": sub}},
		"lines": Object{"object": "list", "has_more": false, "url": "/v1/invoices/" + id + "/lines", "data": []Object{{"object": "line_item", "id": m.id("il"),
			"invoice": id, "description": "Subscription " + fmt.Sprint(price), "currency": "usd", "amount": amount, "quantity": int64(1),
			"period":  Object{"start": item["current_period_start"], "end": item["current_period_end"]},
			"pricing": Object{"type": "price_details", "price_details": Object{"price": price}}}}},
		"payments": Object{"object": "list", "data": []Object{}, "has_more": false, "url": "/v1/invoice_payments"}}
	m.invoices[id] = inv
	s["latest_invoice"] = inv
	return inv
}

func (m *Mock) payInvoiceLocked(inv Object) {
	sub := inv["parent"].(Object)["subscription_details"].(Object)["subscription"].(string)
	s := m.subs[sub]
	amount := inv["amount_remaining"].(int64)
	if amount > 0 {
		pi := Object{"object": "payment_intent", "id": m.id("pi"), "amount": amount, "currency": inv["currency"], "customer": s["customer"], "payment_method": s["default_payment_method"], "livemode": false, "metadata": map[string]string{}, "created": m.now().Unix()}
		m.intents[pi["id"].(string)] = pi
		m.order = append(m.order, pi["id"].(string))
		m.chargeLocked(pi)["created"] = m.now().Unix()
		inv["payments"] = Object{"object": "list", "has_more": false, "url": "/v1/invoice_payments", "data": []Object{{"object": "invoice_payment", "id": m.id("inpay"),
			"invoice": inv["id"], "status": "paid", "currency": inv["currency"], "amount_paid": amount, "amount_requested": amount, "created": m.now().Unix(), "is_default": true, "livemode": false,
			"status_transitions": Object{"paid_at": m.now().Unix(), "canceled_at": nil}, "payment": Object{"type": "payment_intent", "payment_intent": pi["id"]}}}}
	}
	inv["status"], inv["amount_paid"], inv["amount_remaining"], inv["next_payment_attempt"] = "paid", inv["amount_due"], int64(0), nil
	inv["status_transitions"].(Object)["paid_at"] = m.now().Unix()
}

func (m *Mock) changePrice(sub, price string, amount, proration int64, paid bool) Object {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.subs[sub]
	s["items"].(Object)["data"].([]Object)[0]["price"] = Object{"id": price, "unit_amount": amount, "currency": "usd"}
	inv := m.invoiceLocked(sub, proration, paid)
	inv["billing_reason"] = "subscription_update"
	period := inv["lines"].(Object)["data"].([]Object)[0]["period"].(Object)
	if now := m.now().Unix(); now > period["start"].(int64) && now < period["end"].(int64) {
		period["start"] = now
	}
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
	if price, ok := m.schedulePriceLocked(s, start); ok {
		amount, _ := m.priceAmountLocked(price)
		item["price"] = Object{"id": price, "unit_amount": amount, "currency": "usd"}
	}
	amount := item["price"].(Object)["unit_amount"].(int64)
	s["status"] = "active"
	if !paid {
		s["status"] = "past_due"
	}
	return clone(m.invoiceLocked(sub, amount, paid))
}

// ChargeInvoiceItem is Stripe invoicing a one-off item on price to the
// subscription's customer, paid at once; it returns the invoice.
func (m *Mock) ChargeInvoiceItem(sub, price string, amount int64) Object {
	m.mu.Lock()
	defer m.mu.Unlock()
	inv := m.newInvoiceLocked(sub, amount)
	line := inv["lines"].(Object)["data"].([]Object)[0]
	line["description"], line["pricing"] = "Invoice item "+price, Object{"type": "price_details", "price_details": Object{"price": price}}
	inv["billing_reason"], inv["status"], inv["attempt_count"] = "manual", "open", int64(1)
	inv["status_transitions"].(Object)["finalized_at"] = m.now().Unix()
	m.payInvoiceLocked(inv)
	return clone(inv)
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
	m.newInvoiceLocked(sub, item["price"].(Object)["unit_amount"].(int64))
	return clone(s)
}

// CollectDraft finalizes and pays the draft invoice; it returns the invoice.
func (m *Mock) CollectDraft(sub string) Object {
	m.mu.Lock()
	defer m.mu.Unlock()
	inv := m.subs[sub]["latest_invoice"].(Object)
	if inv["status"] == "draft" {
		inv["status_transitions"].(Object)["finalized_at"] = m.now().Unix()
		inv["attempt_count"] = int64(1)
		m.payInvoiceLocked(inv)
	}
	return clone(inv)
}

// CancelSubscription is the subscription ended at Stripe at someone's
// request (the customer's portal or the dashboard); it returns the
// subscription.
func (m *Mock) CancelSubscription(sub string) Object {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.subs[sub]
	m.endLocked(s, "cancellation_requested")
	return clone(s)
}

// endLocked ends s now, for reason (Stripe's cancellation_details.reason).
func (m *Mock) endLocked(s Object, reason string) {
	s["status"], s["canceled_at"], s["ended_at"] = "canceled", m.now().Unix(), m.now().Unix()
	s["cancellation_details"] = Object{"reason": reason, "comment": nil, "feedback": nil}
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

// TagSubscription sets a subscription's metadata key and creation time, as an
// object another system made carries them.
func (m *Mock) TagSubscription(id, key, value string, created time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.subs[id]
	s["metadata"] = map[string]string{key: value}
	s["created"] = created.Unix()
}
