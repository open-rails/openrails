package catalog

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/open-rails/openrails/internal/railresolve"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db/models"
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
					Created:       int64(1000 + f.n), // monotonic, like Stripe's `created`
					EnabledEvents: r.Form["enabled_events[]"],
					Metadata:      map[string]string{StripeMetadataOpenRailsManaged: r.Form.Get("metadata[openrails_managed]")},
				}
				f.n++
				f.endpoints[ep.ID] = ep
				secret := fmt.Sprintf("whsec_fake_%d", f.secretN)
				f.secretN++
				out := map[string]any{
					"id": ep.ID, "url": ep.URL, "status": ep.Status, "api_version": ep.APIVersion,
					"created": ep.Created, "enabled_events": ep.EnabledEvents,
					"metadata": ep.Metadata, "secret": secret,
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
				// Stripe rejects api_version here; the fake mirrors that so a
				// regression to in-place version updates fails loudly.
				if r.Form.Get("api_version") != "" {
					w.WriteHeader(http.StatusBadRequest)
					_, _ = w.Write([]byte(`{"error":{"message":"Received unknown parameter: api_version"}}`))
					return
				}
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
				for k, v := range r.Form {
					name, ok := strings.CutPrefix(k, "metadata[")
					if !ok || !strings.HasSuffix(name, "]") {
						continue
					}
					name = strings.TrimSuffix(name, "]")
					if ep.Metadata == nil {
						ep.Metadata = map[string]string{}
					}
					if len(v) > 0 && v[0] == "" {
						delete(ep.Metadata, name)
					} else if len(v) > 0 {
						ep.Metadata[name] = v[0]
					}
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
		Rails:   railresolve.FixedSet{"stripe": {Rail: models.RailStripe, Stripe: &config.StripeRailConfig{SecretKey: "sk_test_123"}}},
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

// #856: an api_version bump ROLLS OVER — the successor is created first, the
// predecessor is stamped superseded and left ENABLED. Zero deletes, so zero
// deliveries lost.
func TestReconcileWebhookEndpointRollsOverOnVersionDrift(t *testing.T) {
	ctx := context.Background()
	fake := newFakeStripeWebhooks()
	svc := newWebhookTestSvc(t, fake)
	events := []string{"invoice.paid"}
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)

	// Seed an existing managed endpoint pinned to an OLD api_version.
	fake.endpoints["we_old"] = &StripeWebhookEndpoint{
		ID: "we_old", URL: "https://a.example/wh", Status: "enabled",
		APIVersion:    "2020-01-01",
		Created:       1,
		EnabledEvents: events,
		Metadata:      map[string]string{StripeMetadataOpenRailsManaged: "true"},
	}

	res, err := svc.ReconcileWebhookEndpoint(ctx, DesiredWebhookEndpoint{
		URL: "https://a.example/wh", EnabledEvents: events, HaveSecret: true, Now: now,
	})
	require.NoError(t, err)
	require.Equal(t, WebhookRolledOver, res.Action)
	require.NotEmpty(t, res.Secret, "the successor mints a new secret")
	require.Zero(t, fake.deletes, "NOTHING is deleted on a version bump")
	require.Equal(t, 1, fake.creates)
	require.Equal(t, stripeapi.APIVersion, fake.endpoints[res.EndpointID].APIVersion)

	// The predecessor survives, still enabled, and is stamped.
	old := fake.endpoints["we_old"]
	require.NotNil(t, old, "the old endpoint is NOT deleted")
	require.Equal(t, "enabled", old.Status, "it keeps delivering through the overlap")
	require.Equal(t, now.Format(time.RFC3339), old.Metadata[StripeMetadataSupersededAt])
	require.Equal(t, []string{"we_old"}, res.Superseded)
	require.Len(t, res.Legacy, 1)
	require.Equal(t, now.Add(WebhookRolloverOverlap), res.Legacy[0].RetireAfter)

	// Idempotent: a second pass with the new secret held is a no-op, not a
	// second rollover.
	res2, err := svc.ReconcileWebhookEndpoint(ctx, DesiredWebhookEndpoint{
		URL: "https://a.example/wh", EnabledEvents: events, HaveSecret: true, Now: now,
	})
	require.NoError(t, err)
	require.Equal(t, WebhookUnchanged, res2.Action)
	require.Equal(t, res.EndpointID, res2.EndpointID)
	require.Equal(t, 1, fake.creates)
	require.Zero(t, fake.deletes)

	// Retirement is a separate, time-gated act. Inside the overlap: nothing.
	ret, err := svc.RetireSupersededWebhookEndpoints(ctx, RetireSupersededParams{Now: now.Add(time.Hour)})
	require.NoError(t, err)
	require.Empty(t, ret.Retired)
	require.Len(t, ret.Pending, 1)
	require.Zero(t, fake.deletes)

	// After the overlap the predecessor may go.
	ret, err = svc.RetireSupersededWebhookEndpoints(ctx, RetireSupersededParams{Now: now.Add(WebhookRolloverOverlap + time.Minute)})
	require.NoError(t, err)
	require.Equal(t, []string{"we_old"}, ret.Retired)
	require.Equal(t, 1, fake.deletes)
	require.NotNil(t, fake.endpoints[res.EndpointID], "the live endpoint stays")
}

// A local secret miss means UNKNOWN, never lost: the endpoint we cannot verify
// is replaced ALONGSIDE, not removed.
func TestReconcileWebhookEndpointRollsOverWhenSecretUnknown(t *testing.T) {
	ctx := context.Background()
	fake := newFakeStripeWebhooks()
	svc := newWebhookTestSvc(t, fake)
	events := []string{"invoice.paid"}

	// Existing endpoint at the CURRENT version, but caller holds no secret.
	fake.endpoints["we_x"] = &StripeWebhookEndpoint{
		ID: "we_x", URL: "https://a.example/wh", Status: "enabled",
		APIVersion:    stripeapi.APIVersion,
		Created:       1,
		EnabledEvents: events,
		Metadata:      map[string]string{StripeMetadataOpenRailsManaged: "true"},
	}

	res, err := svc.ReconcileWebhookEndpoint(ctx, DesiredWebhookEndpoint{URL: "https://a.example/wh", EnabledEvents: events, HaveSecret: false})
	require.NoError(t, err)
	require.Equal(t, WebhookRolledOver, res.Action)
	require.NotEmpty(t, res.Secret)
	require.Zero(t, fake.deletes, "the endpoint whose secret we lost is NOT deleted")
	require.NotNil(t, fake.endpoints["we_x"])
	require.Equal(t, "enabled", fake.endpoints["we_x"].Status)

	// The successor — not the stamped predecessor — is the endpoint the next
	// pass considers current, even though both sit at the pinned version.
	res2, err := svc.ReconcileWebhookEndpoint(ctx, DesiredWebhookEndpoint{URL: "https://a.example/wh", EnabledEvents: events, HaveSecret: true})
	require.NoError(t, err)
	require.Equal(t, WebhookUnchanged, res2.Action)
	require.Equal(t, res.EndpointID, res2.EndpointID)
	require.Equal(t, 1, fake.creates)
}

// Retirement refuses to leave the account unreachable.
func TestRetireSupersededRefusesWithoutALiveSuccessor(t *testing.T) {
	ctx := context.Background()
	fake := newFakeStripeWebhooks()
	svc := newWebhookTestSvc(t, fake)

	fake.endpoints["we_old"] = &StripeWebhookEndpoint{
		ID: "we_old", URL: "https://a.example/wh", Status: "enabled",
		APIVersion:    "2020-01-01",
		EnabledEvents: []string{"invoice.paid"},
		Metadata: map[string]string{
			StripeMetadataOpenRailsManaged: "true",
			StripeMetadataSupersededAt:     "2020-01-02T00:00:00Z",
		},
	}
	_, err := svc.RetireSupersededWebhookEndpoints(ctx, RetireSupersededParams{})
	require.Error(t, err)
	require.Contains(t, err.Error(), "no live endpoint at api_version")
	require.Zero(t, fake.deletes)
}

// The endpoint budget stops a runaway rollover before it can crowd out the
// operator's own endpoints against Stripe's per-account limit.
func TestReconcileWebhookEndpointBudget(t *testing.T) {
	ctx := context.Background()
	fake := newFakeStripeWebhooks()
	svc := newWebhookTestSvc(t, fake)
	for i := 0; i < maxManagedWebhookEndpoints; i++ {
		id := fmt.Sprintf("we_seed_%d", i)
		fake.endpoints[id] = &StripeWebhookEndpoint{
			ID: id, URL: "https://a.example/wh", Status: "enabled",
			APIVersion:    "2020-01-01",
			EnabledEvents: []string{"invoice.paid"},
			Metadata:      map[string]string{StripeMetadataOpenRailsManaged: "true"},
		}
	}
	_, err := svc.ReconcileWebhookEndpoint(ctx, DesiredWebhookEndpoint{
		URL: "https://a.example/wh", EnabledEvents: []string{"invoice.paid"}, HaveSecret: true,
	})
	require.ErrorIs(t, err, ErrWebhookEndpointBudgetExhausted)
	require.Zero(t, fake.creates)
	require.Zero(t, fake.deletes)
}

// ForbidCreate (#723) refuses BOTH create branches before any Stripe mutation:
// no endpoint → no create; drift/lost-secret → the existing endpoint survives
// (no delete).
func TestReconcileWebhookEndpointForbidCreate(t *testing.T) {
	ctx := context.Background()
	fake := newFakeStripeWebhooks()
	svc := newWebhookTestSvc(t, fake)
	events := []string{"invoice.paid"}

	// 1. From nothing: refused, nothing created.
	_, err := svc.ReconcileWebhookEndpoint(ctx, DesiredWebhookEndpoint{URL: "https://a.example/wh", EnabledEvents: events, ForbidCreate: true})
	require.ErrorIs(t, err, ErrWebhookCreateForbidden)
	require.Zero(t, fake.creates)

	// 2. Version drift with a held secret: rollover refused, nothing mutated.
	fake.endpoints["we_old"] = &StripeWebhookEndpoint{
		ID: "we_old", URL: "https://a.example/wh", Status: "enabled",
		APIVersion:    "2020-01-01",
		EnabledEvents: events,
		Metadata:      map[string]string{StripeMetadataOpenRailsManaged: "true"},
	}
	_, err = svc.ReconcileWebhookEndpoint(ctx, DesiredWebhookEndpoint{URL: "https://a.example/wh", EnabledEvents: events, HaveSecret: true, ForbidCreate: true})
	require.ErrorIs(t, err, ErrWebhookCreateForbidden)
	require.Zero(t, fake.deletes, "existing endpoint survives")
	require.Zero(t, fake.creates)

	// 3. Held secret + matching endpoint: in-place reconcile still works.
	fake.endpoints["we_old"].APIVersion = stripeapi.APIVersion
	res, err := svc.ReconcileWebhookEndpoint(ctx, DesiredWebhookEndpoint{URL: "https://a.example/wh", EnabledEvents: events, HaveSecret: true, ForbidCreate: true})
	require.NoError(t, err)
	require.Equal(t, WebhookUnchanged, res.Action)
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
