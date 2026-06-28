//go:build stripelive

// Live Stripe tests for #590 — they hit the REAL Stripe test account using
// STRIPE_SECRET_KEY. Run with:
//
//	export $(grep -E '^STRIPE_SECRET_KEY=' .env | xargs)
//	go test -tags=stripelive ./internal/modules/catalog/ -run Live -v
//
// They are skipped unless STRIPE_SECRET_KEY is a TEST-mode key (rk_test_/sk_test_),
// and self-clean any endpoint they create (only ones at the openrails-livetest URL).
package catalog

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/integrations/stripeapi"
	"github.com/stretchr/testify/require"
)

const (
	liveTestURL1 = "https://openrails-livetest-1.example.com/webhooks/stripe"
	liveTestURL2 = "https://openrails-livetest-2.example.com/webhooks/stripe"
)

func liveStripeSvc(t *testing.T) *StripeCatalogService {
	t.Helper()
	key := strings.TrimSpace(os.Getenv("STRIPE_SECRET_KEY"))
	if key == "" {
		t.Skip("STRIPE_SECRET_KEY not set; skipping live Stripe test")
	}
	if !strings.HasPrefix(key, "rk_test_") && !strings.HasPrefix(key, "sk_test_") {
		t.Skipf("STRIPE_SECRET_KEY is not a test-mode key (prefix %.7s); refusing to run live test", key)
	}
	return &StripeCatalogService{Rails: config.RailSet{"stripe": {Type: config.RailTypeStripe, SecretKey: key}}}
}

// deleteLiveTestEndpoints removes any managed endpoint at one of our test URLs.
func deleteLiveTestEndpoints(t *testing.T, svc *StripeCatalogService) {
	t.Helper()
	ctx := context.Background()
	all, err := svc.ListWebhookEndpoints(ctx)
	require.NoError(t, err)
	for _, e := range all {
		if e.managed() && strings.Contains(e.URL, "openrails-livetest") {
			require.NoError(t, svc.DeleteWebhookEndpoint(ctx, e.ID))
		}
	}
}

func TestLiveWebhookEndpointReconcile(t *testing.T) {
	svc := liveStripeSvc(t)
	ctx := context.Background()
	deleteLiveTestEndpoints(t, svc) // start clean
	t.Cleanup(func() { deleteLiveTestEndpoints(t, svc) })

	events := []string{"invoice.paid", "checkout.session.completed"}

	// CREATE.
	res, err := svc.ReconcileWebhookEndpoint(ctx, DesiredWebhookEndpoint{URL: liveTestURL1, EnabledEvents: events})
	require.NoError(t, err)
	require.Equal(t, WebhookCreated, res.Action)
	require.NotEmpty(t, res.EndpointID)
	require.True(t, strings.HasPrefix(res.Secret, "whsec_"), "create must return a signing secret")
	t.Logf("created endpoint %s with secret prefix %.10s", res.EndpointID, res.Secret)

	// Verify on Stripe: it really exists, is managed, pinned to our version, has our events.
	created := findLiveManaged(t, svc, liveTestURL1)
	require.Equal(t, stripeapi.APIVersion, created.APIVersion, "endpoint must be pinned to our API version")
	require.ElementsMatch(t, events, created.EnabledEvents)
	require.Equal(t, "enabled", created.Status)

	// IDEMPOTENT: same desired + we hold the secret -> unchanged.
	res2, err := svc.ReconcileWebhookEndpoint(ctx, DesiredWebhookEndpoint{URL: liveTestURL1, EnabledEvents: events, HaveSecret: true})
	require.NoError(t, err)
	require.Equal(t, WebhookUnchanged, res2.Action)
	require.Equal(t, res.EndpointID, res2.EndpointID)

	// URL DRIFT -> in-place patch, SAME endpoint id (secret survives, no recreate).
	res3, err := svc.ReconcileWebhookEndpoint(ctx, DesiredWebhookEndpoint{URL: liveTestURL2, EnabledEvents: events, HaveSecret: true})
	require.NoError(t, err)
	require.Equal(t, WebhookUpdated, res3.Action)
	require.Equal(t, res.EndpointID, res3.EndpointID, "url change must patch in place, not recreate")
	moved := findLiveManaged(t, svc, liveTestURL2)
	require.Equal(t, liveTestURL2, moved.URL)
	require.Equal(t, res.EndpointID, moved.ID)

	// EVENTS DRIFT -> in-place patch.
	more := append([]string{}, events...)
	more = append(more, "charge.refunded")
	res4, err := svc.ReconcileWebhookEndpoint(ctx, DesiredWebhookEndpoint{URL: liveTestURL2, EnabledEvents: more, HaveSecret: true})
	require.NoError(t, err)
	require.Equal(t, WebhookUpdated, res4.Action)
	require.Equal(t, res.EndpointID, res4.EndpointID)
	patched := findLiveManaged(t, svc, liveTestURL2)
	require.ElementsMatch(t, more, patched.EnabledEvents)
}

func findLiveManaged(t *testing.T, svc *StripeCatalogService, wantURL string) StripeWebhookEndpoint {
	t.Helper()
	all, err := svc.ListWebhookEndpoints(context.Background())
	require.NoError(t, err)
	for _, e := range all {
		if e.managed() && e.URL == wantURL {
			return e
		}
	}
	t.Fatalf("no managed endpoint found at %s", wantURL)
	return StripeWebhookEndpoint{}
}
