//go:build e2e && integration

package ci_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/internal/engine"
	"github.com/open-rails/openrails/internal/modules/copilot"
)

type featureLimitLLM struct{ asks int }

func (l *featureLimitLLM) CompleteTools(context.Context, string, []copilot.ToolDef, []copilot.ToolMessage, int) (*copilot.ToolTurn, error) {
	l.asks++
	return &copilot.ToolTurn{Text: "No further lookups needed."}, nil
}

// Real feature handlers accept two requests in total across host mounts,
// then refuse before calling the model. The merchant directory, catalog and
// request scopes use PostgreSQL; only authentication and the LLM are supplied
// by this host fixture. The assistant has no merchant Client operation.
func TestHTTPFeatureRateLimitsAreSharedAcrossMounts(t *testing.T) {
	f := newFixture(t)
	cfg := f.config()
	cfg.Merchants = map[string]openrails.MerchantDeclaration{"feature-limits-" + uuid.NewString()[:8]: {}}
	cfg.RateLimits = &openrails.RateLimitsConfig{
		"catalog-ask": {RequestsPerMinute: 2},
	}
	cfg.Captcha = &openrails.CaptchaConfig{SiteKey: "test-site", SecretKey: "test-secret"}
	cfg.LLM = &openrails.LLMConfig{APIKey: "test", CatalogCopilotEnabled: true, CatalogDraftingEnabled: true}
	bill, _ := f.boot(t, cfg, openrails.Deps{FXTransport: testFX.Transport(), Postgres: f.pool})
	catalogLLM := &featureLimitLLM{}
	graph := engine.Graph(bill)
	graph.Runtime.CopilotService.SetLLM(catalogLLM)
	mount := func(prefix string) http.Handler {
		t.Helper()
		routes, err := bill.Routes(openrails.Routes{Auth: hostKey{}, RouteGroups: staffGroups})
		require.NoError(t, err)
		mux := http.NewServeMux()
		for _, route := range routes {
			mux.Handle(route.Method+" "+prefix+route.Path, route.Handler)
		}
		return mux
	}
	first, second := mount("/first"), mount("/second")
	for _, removedPath := range []string{"/v1/admin/dashboard/widgets/generate", "/v1/metrics/dashboard/widgets/generate", "/v1/admin/metrics/ask", "/v1/metrics/ask"} {
		removed := httptest.NewRecorder()
		first.ServeHTTP(removed, httptest.NewRequest(http.MethodPost, "/first"+removedPath, strings.NewReader(`{"question":"Summarize","prompt":"Add a chart"}`)))
		require.Equal(t, http.StatusNotFound, removed.Code, "AI metrics and dashboard endpoints stay absent even with an LLM configured: %s", removedPath)
	}
	require.Zero(t, catalogLLM.asks, "removed features never call the model")
	for _, feature := range []struct {
		path, body string
		calls      *int
	}{
		{"/v1/console/catalog/ask", `{"question":"Summarize"}`, &catalogLLM.asks},
	} {
		t.Run(feature.path, func(t *testing.T) {
			oneMinute(t, 5*time.Second)
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
		})
	}
}
