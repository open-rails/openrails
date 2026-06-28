package catalog

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/integrations/stripeapi"
	"github.com/stretchr/testify/require"
)

// fakeStripeWebhooks is a stateful stand-in for Stripe's webhook_endpoints API.
type fakeStripeWebhooks struct {
	mu        sync.Mutex
	endpoints map[string]*StripeWebhookEndpoint
	n         int
	secretN   int

	creates int
	updates int
	deletes int
}

func newFakeStripeWebhooks() *fakeStripeWebhooks {
	return &fakeStripeWebhooks{endpoints: map[string]*StripeWebhookEndpoint{}}
}

func (f *fakeStripeWebhooks) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		_ = r.ParseForm()
		w.Header().Set("Content-Type", "application/json")
		parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/") // v1 / webhook_endpoints [/ id]

		// /v1/webhook_endpoints
		if len(parts) == 2 && parts[1] == "webhook_endpoints" {
			if r.Method == http.MethodPost {
				f.creates++
				ep := &StripeWebhookEndpoint{
					ID:            fmt.Sprintf("we_%d", f.n),
					URL:           r.Form.Get("url"),
					Status:        "enabled",
					APIVersion:    r.Form.Get("api_version"),
					EnabledEvents: r.Form["enabled_events[]"],
					Metadata:      map[string]string{StripeMetadataOpenRailsManaged: r.Form.Get("metadata[openrails_managed]")},
				}
				f.n++
				f.endpoints[ep.ID] = ep
				secret := fmt.Sprintf("whsec_fake_%d", f.secretN)
				f.secretN++
				out := map[string]any{
					"id": ep.ID, "url": ep.URL, "status": ep.Status, "api_version": ep.APIVersion,
					"enabled_events": ep.EnabledEvents, "metadata": ep.Metadata, "secret": secret,
				}
				_ = json.NewEncoder(w).Encode(out)
				return
			}
			// GET list
			data := make([]*StripeWebhookEndpoint, 0, len(f.endpoints))
			for _, e := range f.endpoints {
				data = append(data, e)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": data, "has_more": false})
			return
		}

		// /v1/webhook_endpoints/{id}
		if len(parts) == 3 && parts[1] == "webhook_endpoints" {
			id := parts[2]
			ep := f.endpoints[id]
			if ep == nil {
				w.WriteHeader(http.StatusNotFound)
				_, _ = w.Write([]byte(`{"error":{"message":"no such endpoint"}}`))
				return
			}
			switch r.Method {
			case http.MethodPost:
				f.updates++
				if v := r.Form.Get("url"); v != "" {
					ep.URL = v
				}
				if evs, ok := r.Form["enabled_events[]"]; ok {
					ep.EnabledEvents = evs
				}
				if d := r.Form.Get("disabled"); d == "false" {
					ep.Status = "enabled"
				} else if d == "true" {
					ep.Status = "disabled"
				}
				_ = json.NewEncoder(w).Encode(ep)
			case http.MethodDelete:
				f.deletes++
				delete(f.endpoints, id)
				_ = json.NewEncoder(w).Encode(map[string]any{"id": id, "deleted": true})
			}
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}
}

func newWebhookTestSvc(t *testing.T, fake *fakeStripeWebhooks) *StripeCatalogService {
	t.Helper()
	srv := httptest.NewServer(fake.handler())
	t.Cleanup(srv.Close)
	return &StripeCatalogService{
		Rails:   config.RailSet{"stripe": {Type: config.RailTypeStripe, SecretKey: "sk_test_123"}},
		BaseURL: srv.URL,
	}
}

func TestReconcileWebhookEndpoint(t *testing.T) {
	ctx := context.Background()
	fake := newFakeStripeWebhooks()
	svc := newWebhookTestSvc(t, fake)
	events := []string{"invoice.paid", "checkout.session.completed"}

	// 1. From nothing: create, returns a secret.
	res, err := svc.ReconcileWebhookEndpoint(ctx, DesiredWebhookEndpoint{URL: "https://a.example/wh", EnabledEvents: events})
	require.NoError(t, err)
	require.Equal(t, WebhookCreated, res.Action)
	require.NotEmpty(t, res.Secret)
	require.Equal(t, 1, fake.creates)
	id := res.EndpointID

	// 2. Idempotent: same desired + we hold the secret -> unchanged, no secret returned.
	res, err = svc.ReconcileWebhookEndpoint(ctx, DesiredWebhookEndpoint{URL: "https://a.example/wh", EnabledEvents: events, HaveSecret: true})
	require.NoError(t, err)
	require.Equal(t, WebhookUnchanged, res.Action)
	require.Empty(t, res.Secret)
	require.Equal(t, id, res.EndpointID)
	require.Equal(t, 1, fake.creates)
	require.Equal(t, 0, fake.updates)

	// 3. URL drift (redeploy): in-place patch, secret survives (no recreate).
	res, err = svc.ReconcileWebhookEndpoint(ctx, DesiredWebhookEndpoint{URL: "https://b.example/wh", EnabledEvents: events, HaveSecret: true})
	require.NoError(t, err)
	require.Equal(t, WebhookUpdated, res.Action)
	require.Empty(t, res.Secret)
	require.Equal(t, 1, fake.creates)
	require.Equal(t, 1, fake.updates)
	require.Equal(t, 0, fake.deletes)

	// 4. Events drift: in-place patch.
	res, err = svc.ReconcileWebhookEndpoint(ctx, DesiredWebhookEndpoint{URL: "https://b.example/wh", EnabledEvents: append(events, "charge.refunded"), HaveSecret: true})
	require.NoError(t, err)
	require.Equal(t, WebhookUpdated, res.Action)
	require.Equal(t, 2, fake.updates)
	require.Equal(t, 0, fake.deletes)
}

func TestReconcileWebhookEndpointRecreatesOnVersionDrift(t *testing.T) {
	ctx := context.Background()
	fake := newFakeStripeWebhooks()
	svc := newWebhookTestSvc(t, fake)
	events := []string{"invoice.paid"}

	// Seed an existing managed endpoint pinned to an OLD api_version.
	fake.endpoints["we_old"] = &StripeWebhookEndpoint{
		ID: "we_old", URL: "https://a.example/wh", Status: "enabled",
		APIVersion:    "2020-01-01",
		EnabledEvents: events,
		Metadata:      map[string]string{StripeMetadataOpenRailsManaged: "true"},
	}

	res, err := svc.ReconcileWebhookEndpoint(ctx, DesiredWebhookEndpoint{URL: "https://a.example/wh", EnabledEvents: events, HaveSecret: true})
	require.NoError(t, err)
	require.Equal(t, WebhookRecreated, res.Action)
	require.NotEmpty(t, res.Secret) // recreate rotates the secret
	require.Equal(t, 1, fake.deletes)
	require.Equal(t, 1, fake.creates)
	// The recreated endpoint carries the pinned version.
	require.Equal(t, stripeapi.APIVersion, fake.endpoints[res.EndpointID].APIVersion)
}

func TestReconcileWebhookEndpointRecreatesWhenSecretLost(t *testing.T) {
	ctx := context.Background()
	fake := newFakeStripeWebhooks()
	svc := newWebhookTestSvc(t, fake)
	events := []string{"invoice.paid"}

	// Existing endpoint at the CURRENT version, but caller no longer holds the secret.
	fake.endpoints["we_x"] = &StripeWebhookEndpoint{
		ID: "we_x", URL: "https://a.example/wh", Status: "enabled",
		APIVersion:    stripeapi.APIVersion,
		EnabledEvents: events,
		Metadata:      map[string]string{StripeMetadataOpenRailsManaged: "true"},
	}

	res, err := svc.ReconcileWebhookEndpoint(ctx, DesiredWebhookEndpoint{URL: "https://a.example/wh", EnabledEvents: events, HaveSecret: false})
	require.NoError(t, err)
	require.Equal(t, WebhookRecreated, res.Action)
	require.NotEmpty(t, res.Secret)
	require.Equal(t, 1, fake.deletes)
}

func TestReconcileWebhookEndpointIgnoresUnmanaged(t *testing.T) {
	ctx := context.Background()
	fake := newFakeStripeWebhooks()
	svc := newWebhookTestSvc(t, fake)
	events := []string{"invoice.paid"}

	// An operator-created (unmanaged) endpoint exists.
	fake.endpoints["we_op"] = &StripeWebhookEndpoint{
		ID: "we_op", URL: "https://operator.example/wh", Status: "enabled",
		APIVersion:    "2020-01-01",
		EnabledEvents: []string{"customer.created"},
		Metadata:      map[string]string{},
	}

	// Reconcile must NOT adopt/modify it — it creates its own managed endpoint.
	res, err := svc.ReconcileWebhookEndpoint(ctx, DesiredWebhookEndpoint{URL: "https://ours.example/wh", EnabledEvents: events})
	require.NoError(t, err)
	require.Equal(t, WebhookCreated, res.Action)
	require.Equal(t, 0, fake.deletes)
	require.Equal(t, 0, fake.updates)
	// Operator endpoint untouched.
	require.Equal(t, "https://operator.example/wh", fake.endpoints["we_op"].URL)
}
