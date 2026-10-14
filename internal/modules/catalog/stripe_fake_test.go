package catalog

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/railresolve"
)

// fakeStripe is a stateful stand-in for Stripe's webhook_endpoints API at the
// HTTP boundary.
type fakeStripe struct {
	mu        sync.Mutex
	url       string
	endpoints map[string]*StripeWebhookEndpoint
	n         int

	creates, updates, deletes int
}

func newFakeStripe(t *testing.T) *fakeStripe {
	f := &fakeStripe{endpoints: map[string]*StripeWebhookEndpoint{}}
	srv := httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(srv.Close)
	f.url = srv.URL
	return f
}

func (f *fakeStripe) service() *StripeCatalogService {
	return &StripeCatalogService{
		Config:  &config.Config{ProviderWriteMode: config.ProviderWriteModeFull},
		Rails:   railresolve.FixedSet{"stripe": {Rail: models.RailStripe, Stripe: &config.StripeRailConfig{SecretKey: "sk_test_123"}}},
		BaseURL: f.url,
	}
}

func (f *fakeStripe) seed(id, version string, created int64, meta map[string]string) {
	if meta == nil {
		meta = map[string]string{StripeMetadataOpenRailsManaged: "true"}
	}
	f.endpoints[id] = &StripeWebhookEndpoint{ID: id, URL: "https://a.example/wh", Status: "enabled", APIVersion: version, Created: created, EnabledEvents: []string{"invoice.paid"}, Metadata: meta}
}

func (f *fakeStripe) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	_ = r.ParseForm()
	w.Header().Set("Content-Type", "application/json")
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	write := func(v any) { _ = json.NewEncoder(w).Encode(v) }
	list := func(data any) { write(map[string]any{"object": "list", "data": data, "has_more": false}) }

	switch {
	case len(parts) == 2 && parts[1] == "webhook_endpoints" && r.Method == http.MethodPost:
		f.creates++
		ep := &StripeWebhookEndpoint{
			ID: fmt.Sprintf("we_%d", f.n), URL: r.Form.Get("url"), Status: "enabled", APIVersion: r.Form.Get("api_version"),
			Created: int64(1000 + f.n), EnabledEvents: r.Form["enabled_events[]"],
			Metadata: map[string]string{StripeMetadataOpenRailsManaged: r.Form.Get("metadata[openrails_managed]")},
		}
		f.endpoints[ep.ID] = ep
		write(map[string]any{"id": ep.ID, "url": ep.URL, "status": ep.Status, "api_version": ep.APIVersion, "created": ep.Created,
			"enabled_events": ep.EnabledEvents, "metadata": ep.Metadata, "secret": fmt.Sprintf("whsec_fake_%d", f.n)})
		f.n++
	case len(parts) == 2 && parts[1] == "webhook_endpoints":
		data := make([]*StripeWebhookEndpoint, 0, len(f.endpoints))
		for _, e := range f.endpoints {
			data = append(data, e)
		}
		list(data)
	case len(parts) == 3 && parts[1] == "webhook_endpoints" && f.endpoints[parts[2]] != nil:
		ep := f.endpoints[parts[2]]
		if r.Method == http.MethodDelete {
			f.deletes++
			delete(f.endpoints, ep.ID)
			write(map[string]any{"id": ep.ID, "deleted": true})
			return
		}
		f.updates++
		if r.Form.Get("api_version") != "" { // Stripe refuses in-place version changes
			w.WriteHeader(http.StatusBadRequest)
			write(map[string]any{"error": map[string]string{"message": "Received unknown parameter: api_version"}})
			return
		}
		if v := r.Form.Get("url"); v != "" {
			ep.URL = v
		}
		if evs, ok := r.Form["enabled_events[]"]; ok {
			ep.EnabledEvents = evs
		}
		if r.Form.Get("disabled") == "false" {
			ep.Status = "enabled"
		}
		for k, v := range r.Form {
			if name, ok := strings.CutPrefix(k, "metadata["); ok && len(v) > 0 {
				ep.Metadata[strings.TrimSuffix(name, "]")] = v[0]
			}
		}
		write(ep)
	default:
		w.WriteHeader(http.StatusNotFound)
		write(map[string]any{"error": map[string]string{"message": "unhandled"}})
	}
}
