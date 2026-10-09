//go:build e2e && integration

package ci_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/engine"
	"github.com/open-rails/openrails/internal/modules/dashboard"
)

type featureLimitLLM struct{ asks, generations int }

func (l *featureLimitLLM) Complete(context.Context, string, []dashboard.LLMMessage) (string, error) {
	l.generations++
	return `{"query":{"measures":["cancellations"],"by":["time"],"grain":"day","range":{"last":"7d"}},"title":"Cancellations per day","viz":"line"}`, nil
}

func (l *featureLimitLLM) CompleteTools(context.Context, string, []dashboard.ToolDef, []dashboard.ToolMessage, int) (*dashboard.ToolTurn, error) {
	l.asks++
	return &dashboard.ToolTurn{Text: "No further lookups needed."}, nil
}

// hostKey admits every request as the host backend's API key.
type hostKey struct{}

func pass(next http.Handler) http.Handler { return next }

func (hostKey) Required() func(http.Handler) http.Handler                { return pass }
func (hostKey) RequirePermission(string) func(http.Handler) http.Handler { return pass }
func (hostKey) Sensitive() func(http.Handler) http.Handler               { return pass }
func (hostKey) Identity(context.Context) (openrails.Identity, bool) {
	return openrails.Identity{Issuer: "test", Subject: "test-host", SubjectKind: openrails.SubjectApplication,
		Invoker: openrails.Invoker{Issuer: "test", ID: "test-host"}, Credential: openrails.Credential{Kind: openrails.CredentialAPIKey, ID: "k_test"}}, true
}

// Real feature handlers accept two requests in total across host mounts,
// then refuse before calling the model. The merchant directory, catalog and
// request scopes use PostgreSQL; only authentication and the LLM are supplied
// by this host fixture. Trusted Client calls keep the ordinary host boundary.
func TestHTTPFeatureRateLimitsAreSharedAcrossMounts(t *testing.T) {
	f := newFixture(t)
	cfg := f.config()
	cfg.Merchant = openrails.MerchantDeclaration{Slug: "feature-limits-" + uuid.NewString()[:8]}
	cfg.RateLimits = &openrails.RateLimitsConfig{
		"metrics-ask":        {RequestsPerMinute: 2},
		"catalog-ask":        {RequestsPerMinute: 2},
		"dashboard-generate": {RequestsPerMinute: 2},
	}
	cfg.Captcha = &openrails.CaptchaConfig{SiteKey: "test-site", SecretKey: "test-secret"}
	cfg.LLM = &openrails.LLMConfig{APIKey: "test", AskEnabled: true, CatalogCopilotEnabled: true, CatalogDraftingEnabled: true}
	client, err := openrails.New(t.Context(), cfg, openrails.Deps{Postgres: f.pool})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, client.Close(context.Background())) })
	dashboardLLM, catalogLLM := &featureLimitLLM{}, &featureLimitLLM{}
	graph := engine.Graph(client)
	graph.Runtime.DashboardService.SetLLM(dashboardLLM)
	graph.Runtime.CopilotService.SetLLM(catalogLLM)
	mount := func(prefix string) http.Handler {
		t.Helper()
		routes, err := client.Routes(openrails.Routes{Auth: hostKey{}, Merchant: true, MerchantConfig: true, Guards: staffGuards})
		require.NoError(t, err)
		mux := http.NewServeMux()
		for _, route := range routes {
			mux.Handle(route.Method+" "+prefix+route.Path, route.Handler)
		}
		return mux
	}
	first, second := mount("/first"), mount("/second")
	for _, feature := range []struct {
		path, body string
		calls      *int
		trusted    func() error
	}{
		{"/v1/merchant/metrics/ask", `{"question":"Summarize"}`, &dashboardLLM.asks, func() error {
			_, err := client.AskMetrics(t.Context(), billing.AskMetricsParams{Question: "Summarize"})
			return err
		}},
		{"/v1/merchant/catalog/ask", `{"question":"Summarize"}`, &catalogLLM.asks, func() error {
			_, err := client.AskCatalog(t.Context(), billing.AskCatalogParams{Question: "Summarize"})
			return err
		}},
		{"/v1/merchant/dashboard/widgets/generate", `{"prompt":"Cancellations per day"}`, &dashboardLLM.generations, func() error {
			_, err := client.GenerateDashboardWidget(t.Context(), billing.GenerateDashboardWidgetParams{Prompt: "Cancellations per day"})
			return err
		}},
	} {
		t.Run(feature.path, func(t *testing.T) {
			for i, mounted := range []struct {
				handler http.Handler
				prefix  string
			}{{first, "/first"}, {second, "/second"}, {first, "/first"}} {
				req := httptest.NewRequest(http.MethodPost, mounted.prefix+feature.path, strings.NewReader(feature.body))
				req.RemoteAddr = "203.0.113.94:1234"
				req.Header.Set("Content-Type", "application/json")
				rec := httptest.NewRecorder()
				mounted.handler.ServeHTTP(rec, req)
				if i < 2 {
					require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
					require.Equal(t, i+1, *feature.calls, "count each admitted request once")
				} else {
					require.Equal(t, http.StatusTooManyRequests, rec.Code, rec.Body.String())
					require.Contains(t, rec.Body.String(), "rate_limit_exceeded")
					require.NotEmpty(t, rec.Header().Get("Retry-After"))
					require.Empty(t, rec.Header().Get("X-Captcha-Required"))
					require.Equal(t, 2, *feature.calls, "a refusal never calls the model")
				}
			}
			// Direct Client calls remain trusted host operations, as for other
			// merchant features, without a separate AI-only usage quota.
			for range 3 {
				require.NoError(t, feature.trusted())
			}
			require.Equal(t, 5, *feature.calls)
		})
	}
}
