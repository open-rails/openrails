package stripemock

import (
	"maps"
	"net/http"
	"net/url"
	"strings"
)

// webhookEndpoint serves /v1/webhook_endpoints: create (the only answer that
// carries the signing secret), list, update and delete.
func (m *Mock) webhookEndpoint(r *http.Request, form url.Values, seg []string) (int, any) {
	account := accountOf(r)
	switch {
	case r.Method == http.MethodPost && len(seg) == 1:
		if form.Get("url") == "" || len(form["enabled_events[]"]) == 0 {
			return 400, stripeErr("parameter_missing")
		}
		e := m.newWebhookEndpointLocked(account, form.Get("url"), form.Get("api_version"), form["enabled_events[]"], metadataOf(form))
		out := maps.Clone(e)
		out["secret"] = "whsec_" + e["id"].(string)
		return 200, out
	case r.Method == http.MethodGet && len(seg) == 1:
		data := []any{}
		for _, e := range m.webhookEndpoints[account] {
			data = append(data, e)
		}
		return 200, Object{"object": "list", "data": data, "has_more": false, "url": "/v1/webhook_endpoints"}
	}
	held := -1
	for i, e := range m.webhookEndpoints[account] {
		if e["id"] == seg[1] {
			held = i
		}
	}
	if held < 0 || len(seg) != 2 {
		return 404, stripeErr("resource_missing")
	}
	e := m.webhookEndpoints[account][held]
	switch r.Method {
	case http.MethodGet:
		return 200, e
	case http.MethodPost:
		if v := form.Get("url"); v != "" {
			e["url"] = v
		}
		if events := form["enabled_events[]"]; len(events) > 0 {
			e["enabled_events"] = append([]string(nil), events...)
		}
		switch form.Get("disabled") {
		case "true":
			e["status"] = "disabled"
		case "false":
			e["status"] = "enabled"
		}
		metadata := e["metadata"].(map[string]string)
		maps.Copy(metadata, metadataOf(form))
		return 200, e
	case http.MethodDelete:
		m.webhookEndpoints[account] = append(m.webhookEndpoints[account][:held], m.webhookEndpoints[account][held+1:]...)
		return 200, Object{"object": "webhook_endpoint", "id": e["id"], "deleted": true}
	}
	return 404, stripeErr("resource_missing")
}

func (m *Mock) newWebhookEndpointLocked(account, endpointURL, apiVersion string, events []string, metadata map[string]string) Object {
	e := Object{"object": "webhook_endpoint", "id": m.id("we"), "url": endpointURL, "api_version": apiVersion, "status": "enabled",
		"enabled_events": append([]string(nil), events...), "metadata": metadata, "created": m.now().Unix(), "livemode": false}
	m.webhookEndpoints[account] = append(m.webhookEndpoints[account], e)
	return e
}

// SeedWebhookEndpoint registers an endpoint as the Stripe Dashboard does: no
// OpenRails metadata. It returns the endpoint's id.
func (m *Mock) SeedWebhookEndpoint(account, endpointURL, apiVersion string, events []string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.newWebhookEndpointLocked(account, endpointURL, apiVersion, events, map[string]string{})["id"].(string)
}

// WebhookEndpoints is a copy of the account's webhook endpoints.
func (m *Mock) WebhookEndpoints(account string) []Object {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Object, 0, len(m.webhookEndpoints[account]))
	for _, e := range m.webhookEndpoints[account] {
		c := maps.Clone(e)
		c["enabled_events"] = strings.Join(e["enabled_events"].([]string), ",")
		c["metadata"] = maps.Clone(e["metadata"].(map[string]string))
		out = append(out, c)
	}
	return out
}
