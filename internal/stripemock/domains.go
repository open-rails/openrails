package stripemock

import (
	"net/http"
	"net/url"
	"strings"
)

// accountOf is the account a request's key belongs to: sk_test_acct_<name>
// is acct_<name>'s, any other acct_e2e's.
func accountOf(r *http.Request) string {
	if name, ok := strings.CutPrefix(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "), "sk_test_acct_"); ok {
		return "acct_" + name
	}
	return "acct_e2e"
}

// paymentMethodDomain serves /v1/payment_method_domains: register, list (by
// domain_name), retrieve, update (enabled) and validate.
func (m *Mock) paymentMethodDomain(r *http.Request, form url.Values, seg []string) (int, any) {
	account := accountOf(r)
	switch {
	case r.Method == http.MethodPost && len(seg) == 1:
		name := strings.ToLower(strings.TrimSpace(form.Get("domain_name")))
		if name == "" {
			return 400, stripeErr("parameter_missing")
		}
		for _, d := range m.domains[account] {
			if d["domain_name"] == name {
				return 400, stripeErr("payment_method_domain_already_exists")
			}
		}
		d := m.newDomainLocked(account, name, true)
		if form.Get("enabled") == "false" {
			d["enabled"] = false
		}
		return 200, d
	case r.Method == http.MethodGet && len(seg) == 1:
		name := strings.ToLower(r.URL.Query().Get("domain_name"))
		data := []any{}
		for _, d := range m.domains[account] {
			if name == "" || d["domain_name"] == name {
				data = append(data, d)
			}
		}
		return 200, Object{"object": "list", "data": data, "has_more": false, "url": "/v1/payment_method_domains"}
	}
	var d Object
	for _, held := range m.domains[account] {
		if held["id"] == seg[1] {
			d = held
		}
	}
	if d == nil {
		return 404, stripeErr("resource_missing")
	}
	switch {
	case r.Method == http.MethodGet && len(seg) == 2:
		return 200, d
	case r.Method == http.MethodPost && len(seg) == 2:
		if v := form.Get("enabled"); v != "" {
			d["enabled"] = v == "true"
		}
		return 200, d
	case r.Method == http.MethodPost && len(seg) == 3 && seg[2] == "validate":
		d["apple_pay"] = Object{"status": "active"}
		return 200, d
	}
	return 404, stripeErr("resource_missing")
}

func (m *Mock) newDomainLocked(account, name string, applePay bool) Object {
	status := Object{"status": "active"}
	apple := status
	if !applePay {
		apple = Object{"status": "inactive", "status_details": Object{"error_message": "the domain association file was not found"}}
	}
	d := Object{"object": "payment_method_domain", "id": m.id("pmd"), "domain_name": name, "enabled": true, "livemode": false,
		"created": m.now().Unix(), "apple_pay": apple, "google_pay": status, "link": status, "paypal": status}
	m.domains[account] = append(m.domains[account], d)
	return d
}

// SeedPaymentMethodDomain registers name on account as the merchant would in
// Stripe's dashboard; applePay false leaves its Apple Pay inactive.
func (m *Mock) SeedPaymentMethodDomain(account, name string, applePay bool) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.newDomainLocked(account, strings.ToLower(name), applePay)["id"].(string)
}

// PaymentMethodDomains are account's payment method domains.
func (m *Mock) PaymentMethodDomains(account string) []Object {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Object, 0, len(m.domains[account]))
	for _, d := range m.domains[account] {
		c := Object{}
		for k, v := range d {
			c[k] = v
		}
		out = append(out, c)
	}
	return out
}
