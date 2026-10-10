package stripemock

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/open-rails/openrails/internal/integrations/stripeapi"
)

// CheckoutURL is where a Checkout Session's page lives.
const CheckoutURL = "https://checkout.stripe.test/c/pay/"

func (m *Mock) createCheckoutSession(form url.Values) (int, any) {
	mode := form.Get("mode")
	if mode != "payment" && mode != "subscription" || form.Get("success_url") == "" {
		return http.StatusBadRequest, stripeErr("parameter_missing")
	}
	var total int64
	currency := form.Get("currency")
	for i := 0; ; i++ {
		item := fmt.Sprintf("line_items[%d]", i)
		if !form.Has(item+"[price_data][unit_amount]") && !form.Has(item+"[price]") {
			break
		}
		quantity := int64(1)
		if raw := form.Get(item + "[quantity]"); raw != "" {
			n, err := strconv.ParseInt(raw, 10, 64)
			if err != nil || n < 1 {
				return http.StatusBadRequest, stripeErr("parameter_invalid_integer")
			}
			quantity = n
		}
		amount, err := strconv.ParseInt(form.Get(item+"[price_data][unit_amount]"), 10, 64)
		if price := form.Get(item + "[price]"); price != "" {
			amount, err = m.priceAmount(price), nil
		}
		if err != nil || amount < 0 {
			return http.StatusBadRequest, stripeErr("parameter_invalid_integer")
		}
		total += amount * quantity
		if c := form.Get(item + "[price_data][currency]"); c != "" {
			currency = c
		}
	}
	if currency == "" {
		currency = "usd"
	}
	id := m.id("cs_test")
	s := Object{"object": "checkout.session", "id": id, "url": CheckoutURL + id, "mode": mode, "status": "open", "payment_status": "unpaid",
		"amount_total": total, "currency": currency, "customer": nilIfEmpty(form.Get("customer")), "customer_email": nilIfEmpty(form.Get("customer_email")),
		"client_reference_id": nilIfEmpty(form.Get("client_reference_id")), "success_url": form.Get("success_url"), "cancel_url": nilIfEmpty(form.Get("cancel_url")),
		"payment_intent": nil, "subscription": nil, "metadata": metadataOf(form), "livemode": false,
		"created": m.now().Unix(), "expires_at": m.now().Add(24 * time.Hour).Unix()}
	m.sessions[id] = s
	m.sessionOrder = append(m.sessionOrder, id)
	return 200, s
}

func (m *Mock) priceAmount(id string) int64 {
	if amount, ok := m.amounts[id]; ok {
		return amount
	}
	return 999
}

func nilIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// CheckoutSessions are copies of every Checkout Session, oldest first.
func (m *Mock) CheckoutSessions() []Object {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Object, 0, len(m.sessionOrder))
	for _, id := range m.sessionOrder {
		out = append(out, clone(m.sessions[id]))
	}
	return out
}

// ErrNoWebhook is an event sent before SendWebhooksTo named an endpoint.
var ErrNoWebhook = errors.New("stripemock: no webhook endpoint; call SendWebhooksTo")

// CompleteCheckoutSession is the customer paying an open payment-mode
// Checkout Session: Stripe charges amount_total (a PaymentIntent with its
// charge; a zero total charges nothing), completes the session and sends
// checkout.session.completed. It returns the event's id.
func (m *Mock) CompleteCheckoutSession(ctx context.Context, id string) (string, error) {
	m.mu.Lock()
	s, ok := m.sessions[id]
	switch {
	case !ok:
		m.mu.Unlock()
		return "", fmt.Errorf("stripemock: no Checkout Session %s", id)
	case s["status"] != "open" || s["mode"] != "payment":
		m.mu.Unlock()
		return "", fmt.Errorf("stripemock: Checkout Session %s is %v in %v mode; only an open payment session completes", id, s["status"], s["mode"])
	}
	s["status"] = "complete"
	s["payment_status"] = "no_payment_required"
	if total := s["amount_total"].(int64); total > 0 {
		customer, _ := s["customer"].(string)
		pm := m.id("pm")
		m.methods[pm] = Object{"object": "payment_method", "id": pm, "type": "card", "customer": s["customer"], "livemode": false,
			"card": Object{"brand": "visa", "last4": "4242", "exp_month": 12, "exp_year": 2035}}
		pi := Object{"object": "payment_intent", "id": m.id("pi"), "amount": total, "currency": s["currency"], "customer": customer, "payment_method": pm,
			"livemode": false, "metadata": s["metadata"], "created": m.now().Unix()}
		m.intents[pi["id"].(string)] = pi
		m.order = append(m.order, pi["id"].(string))
		m.chargeLocked(pi)["created"] = m.now().Unix()
		s["payment_status"], s["payment_intent"] = "paid", pi["id"]
	}
	session := clone(s)
	m.mu.Unlock()
	return m.SendEvent(ctx, "checkout.session.completed", session)
}

type webhookTarget struct{ url, secret string }

// Event is one webhook event Stripe sent.
type Event struct {
	ID, Type string
	// Payload is the event as it was signed.
	Payload []byte
}

// SendWebhooksTo names where Stripe sends events (the host's mounted
// /v1/webhooks/stripe/{account_id}) and the endpoint's signing secret.
func (m *Mock) SendWebhooksTo(endpoint, secret string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.webhook = webhookTarget{url: endpoint, secret: secret}
}

// SendEvent sends a new event of type kind about object, signed as Stripe
// signs it, and returns its id. A refusal is an error; the event is kept for
// Redeliver either way.
func (m *Mock) SendEvent(ctx context.Context, kind string, object Object) (string, error) {
	m.mu.Lock()
	id := m.id("evt")
	payload, err := json.Marshal(Object{"id": id, "object": "event", "type": kind, "created": m.now().Unix(), "livemode": false,
		"api_version": stripeapi.APIVersion, "pending_webhooks": 1, "data": Object{"object": object}})
	if err != nil {
		m.mu.Unlock()
		return "", err
	}
	m.events = append(m.events, Event{ID: id, Type: kind, Payload: payload})
	m.mu.Unlock()
	return id, m.deliver(ctx, payload)
}

// Redeliver sends a sent event again, as Stripe retries one.
func (m *Mock) Redeliver(ctx context.Context, id string) error {
	for _, e := range m.Events() {
		if e.ID == id {
			return m.deliver(ctx, e.Payload)
		}
	}
	return fmt.Errorf("stripemock: no event %s", id)
}

// Events are the events Stripe sent, oldest first.
func (m *Mock) Events() []Event {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]Event(nil), m.events...)
}

// deliver posts payload to the webhook endpoint with a Stripe-Signature
// header. Its timestamp is the wall clock, which the receiver checks.
func (m *Mock) deliver(ctx context.Context, payload []byte) error {
	m.mu.Lock()
	target := m.webhook
	m.mu.Unlock()
	if target.url == "" {
		return ErrNoWebhook
	}
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	mac := hmac.New(sha256.New, []byte(target.secret))
	mac.Write([]byte(ts + "." + string(payload)))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target.url, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Stripe-Signature", "t="+ts+",v1="+hex.EncodeToString(mac.Sum(nil)))
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode/100 != 2 {
		body, _ := io.ReadAll(io.LimitReader(res.Body, 4096))
		return fmt.Errorf("stripemock: webhook answered %d: %s", res.StatusCode, body)
	}
	return nil
}
