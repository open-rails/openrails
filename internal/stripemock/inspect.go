package stripemock

import (
	"fmt"
	"sort"
	"strings"
)

// LedgerEntry is the money one succeeded PaymentIntent moved, in minor units,
// and what of it was refunded.
type LedgerEntry struct {
	PaymentIntent, Charge, PaymentMethod string
	Amount, Refunded                     int64
}

// Ledger is the succeeded PaymentIntents of a Stripe customer ("" all), in
// creation order.
func (m *Mock) Ledger(customer string) []LedgerEntry {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []LedgerEntry
	for _, id := range m.order {
		pi := m.intents[id]
		if (customer != "" && pi["customer"] != customer) || pi["status"] != "succeeded" {
			continue
		}
		ch := m.charges[pi["latest_charge"].(string)]
		out = append(out, LedgerEntry{PaymentIntent: pi["id"].(string), Charge: ch["id"].(string), PaymentMethod: fmt.Sprint(pi["payment_method"]),
			Amount: ch["amount"].(int64), Refunded: ch["amount_refunded"].(int64)})
	}
	return out
}

// Attempts counts PaymentIntent creations for a Stripe customer ("" all).
func (m *Mock) Attempts(customer string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, id := range m.order {
		if customer == "" || m.intents[id]["customer"] == customer {
			n++
		}
	}
	return n
}

// CustomerOf is the Stripe customer that owns payment method pm.
func (m *Mock) CustomerOf(pm string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return fmt.Sprint(m.methods[pm]["customer"])
}

// CardOf is the last four of payment method pm's card.
func (m *Mock) CardOf(pm string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return fmt.Sprint(m.methods[pm]["card"].(Object)["last4"])
}

// PaymentIntent is a copy of one PaymentIntent, or nil.
func (m *Mock) PaymentIntent(id string) Object {
	m.mu.Lock()
	defer m.mu.Unlock()
	pi, ok := m.intents[id]
	if !ok {
		return nil
	}
	out := make(Object, len(pi))
	for k, v := range pi {
		out[k] = v
	}
	return out
}

// Charge is a copy of one charge, or nil.
func (m *Mock) Charge(id string) Object {
	m.mu.Lock()
	defer m.mu.Unlock()
	if ch, ok := m.charges[id]; ok {
		return clone(ch)
	}
	return nil
}

// Refunds are copies of every refund, oldest first.
func (m *Mock) Refunds() []Object {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Object, 0, len(m.refundOrder))
	for _, id := range m.refundOrder {
		out = append(out, clone(m.refunds[id]))
	}
	return out
}

// Subscription is a copy of one subscription, or nil.
func (m *Mock) Subscription(id string) Object {
	m.mu.Lock()
	defer m.mu.Unlock()
	if s, ok := m.subs[id]; ok {
		return clone(s)
	}
	return nil
}

// Subscriptions is how many subscriptions Stripe holds.
func (m *Mock) Subscriptions() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.subs)
}

// LatestCharge is the charge that paid a subscription's latest invoice.
func (m *Mock) LatestCharge(subscription string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	inv := m.subs[subscription]["latest_invoice"].(Object)
	return inv["payments"].(Object)["data"].([]Object)[0]["payment"].(Object)["charge"].(string)
}

// Mutations are the executed writes whose path starts with prefix.
func (m *Mock) Mutations(prefix string) []Call {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Call
	for _, c := range m.writes {
		if strings.HasPrefix(c.Path, prefix) {
			out = append(out, c)
		}
	}
	return out
}

// Submitted are the writes sent to path, including replays, conflicts and
// lost ones, so a recovery test can tell readback from a new submission.
func (m *Mock) Submitted(path string) []Call {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Call
	for _, c := range m.requests {
		if c.Path == path {
			out = append(out, c)
		}
	}
	return out
}

// Lost is how many PaymentIntent creates failed in transit.
func (m *Mock) Lost() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.lost
}

// Unexpected lists requests the mock does not model; a test asserts it
// empty.
func (m *Mock) Unexpected() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := append([]string(nil), m.odd...)
	sort.Strings(out)
	return out
}

// PaymentMethod is a copy of one payment method, or nil.
func (m *Mock) PaymentMethod(id string) Object {
	m.mu.Lock()
	defer m.mu.Unlock()
	if pm, ok := m.methods[id]; ok {
		return clone(pm)
	}
	return nil
}
