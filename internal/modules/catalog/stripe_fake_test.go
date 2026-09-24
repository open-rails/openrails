package catalog

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/railresolve"
)

// fakeStripe is a stateful stand-in for Stripe's webhook_endpoints and
// entitlements/product-features APIs at the HTTP boundary.
type fakeStripe struct {
	mu        sync.Mutex
	url       string
	endpoints map[string]*StripeWebhookEndpoint
	features  map[string]StripeFeature     // lookup_key -> feature
	attached  map[string]map[string]string // product -> product_feature id -> lookup_key
	n         int

	creates, updates, deletes          int
	featureCreates, attaches, detaches int
}

func newFakeStripe(t *testing.T) *fakeStripe {
	f := &fakeStripe{endpoints: map[string]*StripeWebhookEndpoint{}, features: map[string]StripeFeature{}, attached: map[string]map[string]string{}}
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
	featureJSON := func(ft StripeFeature) map[string]any {
		return map[string]any{"id": ft.ID, "lookup_key": ft.LookupKey, "name": ft.Name, "metadata": ft.Metadata}
	}

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
	case len(parts) == 3 && parts[1] == "entitlements" && r.Method == http.MethodPost:
		f.featureCreates++
		ft := StripeFeature{ID: fmt.Sprintf("feat_%d", f.n), LookupKey: r.Form.Get("lookup_key"), Name: r.Form.Get("name"),
			Metadata: map[string]string{StripeMetadataOpenRailsManaged: r.Form.Get("metadata[openrails_managed]")}}
		f.n++
		f.features[ft.LookupKey] = ft
		write(featureJSON(ft))
	case len(parts) == 3 && parts[1] == "entitlements":
		var data []map[string]any
		for _, ft := range f.features {
			data = append(data, featureJSON(ft))
		}
		list(data)
	case len(parts) == 4 && parts[1] == "products" && parts[3] == "features" && r.Method == http.MethodPost:
		f.attaches++
		pf := fmt.Sprintf("pf_%d", f.n)
		f.n++
		for _, ft := range f.features {
			if ft.ID == r.Form.Get("entitlement_feature") {
				if f.attached[parts[2]] == nil {
					f.attached[parts[2]] = map[string]string{}
				}
				f.attached[parts[2]][pf] = ft.LookupKey
			}
		}
		write(map[string]any{"id": pf, "object": "product_feature"})
	case len(parts) == 4 && parts[1] == "products" && parts[3] == "features":
		var data []map[string]any
		for pf, key := range f.attached[parts[2]] {
			data = append(data, map[string]any{"id": pf, "entitlement_feature": featureJSON(f.features[key])})
		}
		list(data)
	case len(parts) == 5 && parts[1] == "products" && r.Method == http.MethodDelete:
		f.detaches++
		delete(f.attached[parts[2]], parts[4])
		write(map[string]any{"id": parts[4], "deleted": true})
	default:
		w.WriteHeader(http.StatusNotFound)
		write(map[string]any{"error": map[string]string{"message": "unhandled"}})
	}
}

func (f *fakeStripe) attachedKeys(product string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, key := range f.attached[product] {
		out = append(out, key)
	}
	sort.Strings(out)
	return out
}
