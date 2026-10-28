package stripemock

import (
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// Price is a Stripe price an account already has. Amount is in minor units
// (cents); Custom is a price with no fixed unit_amount. Interval is day,
// week, month or year (Count 1 when zero); "" is a one-time price. Inline is
// a price created on a subscription: readable by id, never listed.
type Price struct {
	ID, Product, Nickname, Currency string
	Amount                          int64
	Custom                          bool
	Interval                        string
	Count                           int
	Inactive                        bool
	Inline                          bool
}

// AddProduct seeds an active product and returns its id (id when set).
func (m *Mock) AddProduct(id, name string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if id == "" {
		id = m.id("prod")
	}
	m.products[id] = Object{"object": "product", "id": id, "name": name, "active": true, "livemode": false, "metadata": map[string]string{}, "created": m.now().Unix()}
	return id
}

// SetProductActive archives or restores a product at Stripe.
func (m *Mock) SetProductActive(id string, active bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.products[id]["active"] = active
}

// DeleteProduct deletes a product at Stripe: reads of it answer 404.
func (m *Mock) DeleteProduct(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.products, id)
}

// AddPrice seeds a price and returns its id (p.ID when set).
func (m *Mock) AddPrice(p Price) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if p.ID == "" {
		p.ID = m.id("price")
	}
	if p.Currency == "" {
		p.Currency = "usd"
	}
	price := Object{"object": "price", "id": p.ID, "product": p.Product, "active": !p.Inactive, "currency": strings.ToLower(p.Currency), "nickname": nilIfEmpty(p.Nickname),
		"unit_amount": p.Amount, "unit_amount_decimal": strconv.FormatInt(p.Amount, 10), "type": "one_time", "recurring": nil, "livemode": false, "metadata": map[string]string{}, "created": m.now().Unix()}
	if p.Custom {
		price["unit_amount"], price["unit_amount_decimal"] = nil, nil
	}
	if p.Interval != "" {
		count := max(p.Count, 1)
		price["type"], price["recurring"] = "recurring", Object{"interval": p.Interval, "interval_count": count}
	}
	m.prices[p.ID] = price
	if p.Inline {
		m.inline[p.ID] = true
	}
	return p.ID
}

// SetPriceActive archives or restores a price at Stripe. Stripe refuses to
// move a subscription onto an inactive price.
func (m *Mock) SetPriceActive(id string, active bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.prices[id]["active"] = active
}

// Reads counts the GET requests whose path starts with prefix.
func (m *Mock) Reads(prefix string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, path := range m.reads {
		if strings.HasPrefix(path, prefix) {
			n++
		}
	}
	return n
}

// catalogLocked serves product and price reads. Lists filter by active as
// Stripe does: products list both unless asked, prices only active ones.
func (m *Mock) catalogLocked(r *http.Request, seg []string, q url.Values) (int, any, bool) {
	if r.Method != http.MethodGet || seg[0] != "products" && seg[0] != "prices" || len(seg) > 2 {
		return 0, nil, false
	}
	store := m.products
	if seg[0] == "prices" {
		store = m.prices
	}
	if len(seg) == 2 {
		if o, ok := store[seg[1]]; ok {
			return 200, o, true
		}
		return 404, stripeErr("resource_missing"), true
	}
	want := q.Get("active")
	if want == "" && seg[0] == "prices" {
		want = "true"
	}
	matching := map[string]Object{}
	for id, o := range store {
		if m.inline[id] || want != "" && (o["active"] == true) != (want == "true") {
			continue
		}
		if product := q.Get("product"); product != "" && o["product"] != product {
			continue
		}
		matching[id] = o
	}
	return 200, stripeList(matching, q), true
}

// priceAmountLocked is a stored price's fixed amount in minor units.
func (m *Mock) priceAmountLocked(id string) (int64, bool) {
	p, ok := m.prices[id]
	if !ok {
		return 0, false
	}
	amount, ok := p["unit_amount"].(int64)
	return amount, ok
}
