//go:build integration

package integrationharness

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/controlplane"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/modules/dashboard"
)

// #741 configurable dashboard: default-template seeding (usage-gated), PUT
// round-trip with compiler validation, RLS isolation, permission gates, and
// the NL widget generator against a deterministic fake LLM (no network).

type dashboardResp struct {
	Widgets []struct {
		ID    string          `json:"id"`
		Title string          `json:"title"`
		Viz   string          `json:"viz"`
		Query json.RawMessage `json:"query"`
		Grid  struct {
			X, Y, W, H int
		} `json:"grid"`
	} `json:"widgets"`
	IsDefault bool    `json:"is_default"`
	UpdatedBy *string `json:"updated_by"`
}

func widgetIDs(d dashboardResp) []string {
	ids := make([]string, 0, len(d.Widgets))
	for _, w := range d.Widgets {
		ids = append(ids, w.ID)
	}
	return ids
}

func getDashboard(t *testing.T, baseURL, token string) dashboardResp {
	t.Helper()
	status, body := requestJSON(t, http.MethodGet, baseURL+"/v1/merchant/dashboard", token, nil)
	require.Equalf(t, http.StatusOK, status, "GET dashboard: %s", string(body))
	var out dashboardResp
	require.NoError(t, json.Unmarshal(body, &out))
	return out
}

func errorParams(t *testing.T, body []byte) []string {
	t.Helper()
	var envelope struct {
		Error struct {
			Metadata struct {
				Errors []struct {
					Code    string   `json:"code"`
					Param   string   `json:"param"`
					Message string   `json:"message"`
					Valid   []string `json:"valid"`
				} `json:"errors"`
			} `json:"metadata"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(body, &envelope))
	var params []string
	for _, fe := range envelope.Error.Metadata.Errors {
		params = append(params, fe.Param)
	}
	return params
}

func TestMerchantDashboard(t *testing.T) {
	ctx := context.Background()
	h := New(t, ctx)
	surface := h.StartStandalone("usd")

	aRW := surface.MintAPIKey(dbtest.TestMerchantSlug, "dash-a-rw-"+uuid.NewString(),
		[]string{controlplane.PermMerchantMetricsRead, controlplane.PermMerchantDashboardUpdate})
	// metrics:read + catalog:read maps onto the VIEWER catalog role (support
	// carries dashboard:update; viewer must not).
	aRO := surface.MintAPIKey(dbtest.TestMerchantSlug, "dash-a-ro-"+uuid.NewString(),
		[]string{controlplane.PermMerchantMetricsRead, controlplane.PermMerchantCatalogRead})
	b := surface.ProvisionOwnedMerchant("dashb" + strings.ReplaceAll(uuid.NewString(), "-", ""))
	bRW := surface.MintAPIKey(b.MerchantSlug, "dash-b-rw-"+uuid.NewString(),
		[]string{controlplane.PermMerchantMetricsRead, controlplane.PermMerchantDashboardUpdate})

	pool := h.sharedPool()

	t.Run("default template seeds without usage widgets", func(t *testing.T) {
		d := getDashboard(t, surface.BaseURL, aRW)
		require.True(t, d.IsDefault)
		ids := widgetIDs(d)
		for _, want := range []string{"mrr", "net-revenue", "churn-rate", "dunning", "revenue-by-stream", "payment-health", "new-subscriptions"} {
			require.Contains(t, ids, want)
		}
		require.NotContains(t, ids, "usage-vs-credits", "no usage activity → no usage widgets")
		require.NotContains(t, ids, "top-payers")
		// KPI tiles carry compare for deltas.
		for _, w := range d.Widgets {
			if w.ID == "mrr" {
				require.Contains(t, string(w.Query), `"previous_period"`)
			}
		}
	})

	t.Run("usage activity adds usage widgets", func(t *testing.T) {
		// Seed one metered usage event for merchant B (super pool bypasses RLS).
		customerID := uuid.New()
		_, err := pool.Exec(ctx,
			`INSERT INTO openrails.customers (id, merchant_id, subject) VALUES ($1, $2, $3)`,
			customerID, uuid.UUID(b.MerchantID), "dash-usage-"+uuid.NewString())
		require.NoError(t, err)
		_, err = pool.Exec(ctx,
			`INSERT INTO openrails.usage_events (merchant_id, customer_id, invoker_id, currency, resource, event_type, amount, source, source_id)
			 VALUES ($1, $2, 'dash-test', 'usd', 'api', 'call', 1000, 'test', $3)`,
			uuid.UUID(b.MerchantID), customerID, uuid.NewString())
		require.NoError(t, err)

		ids := widgetIDs(getDashboard(t, surface.BaseURL, bRW))
		require.Contains(t, ids, "usage-vs-credits")
		require.Contains(t, ids, "top-payers")
		// A (no usage) is unaffected.
		require.NotContains(t, widgetIDs(getDashboard(t, surface.BaseURL, aRW)), "usage-vs-credits")
	})

	customLayout := map[string]any{
		"widgets": []map[string]any{
			{
				"id": "failed-payments", "title": "Failed payments per day", "viz": "bar",
				"query": map[string]any{
					"measures": []string{"payment_failures"},
					"by":       []string{"time"},
					"grain":    "day",
					"range":    map[string]string{"last": "14d"},
				},
				"grid": map[string]int{"x": 0, "y": 0, "w": 6, "h": 4},
			},
			{
				"id": "revenue", "title": "Net revenue", "viz": "stat",
				"query": map[string]any{
					"measures": []string{"net_revenue"},
					"range":    map[string]string{"last": "30d"},
					"compare":  "previous_period",
				},
				"grid": map[string]int{"x": 6, "y": 0, "w": 3, "h": 2},
			},
		},
	}

	t.Run("put round trip", func(t *testing.T) {
		status, body := requestJSON(t, http.MethodPut, surface.BaseURL+"/v1/merchant/dashboard", aRW, customLayout)
		require.Equalf(t, http.StatusOK, status, "PUT dashboard: %s", string(body))

		d := getDashboard(t, surface.BaseURL, aRW)
		require.False(t, d.IsDefault)
		require.Equal(t, []string{"failed-payments", "revenue"}, widgetIDs(d))
		require.Equal(t, "bar", d.Widgets[0].Viz)
		require.Contains(t, string(d.Widgets[0].Query), `"14d"`)

		// Saved queries execute as-is through the normal metrics endpoint.
		var q map[string]any
		require.NoError(t, json.Unmarshal(d.Widgets[0].Query, &q))
		status, body = requestJSON(t, http.MethodPost, surface.BaseURL+"/v1/merchant/metrics/query", aRW, q)
		require.Equalf(t, http.StatusOK, status, "saved widget query must execute: %s", string(body))
	})

	t.Run("put rejects invalid widget queries with widget-indexed compiler errors", func(t *testing.T) {
		bad := map[string]any{
			"widgets": []map[string]any{
				customLayout["widgets"].([]map[string]any)[0],
				{
					"id": "broken", "title": "Broken", "viz": "sparkline",
					"query": map[string]any{
						"measures": []string{"cancelations"},
						"by":       []string{"time"},
						"range":    map[string]string{"last": "7d"},
					},
					"grid": map[string]int{"x": 0, "y": 4, "w": 6, "h": 4},
				},
			},
		}
		status, body := requestJSON(t, http.MethodPut, surface.BaseURL+"/v1/merchant/dashboard", aRW, bad)
		require.Equal(t, http.StatusBadRequest, status)
		params := errorParams(t, body)
		require.Contains(t, params, "widgets[1].viz")
		require.Contains(t, params, "widgets[1].query.measures[0]")
		require.Contains(t, string(body), "did you mean", "compiler corrective errors must ride through")
		// Nothing persisted: layout unchanged.
		require.Equal(t, []string{"failed-payments", "revenue"}, widgetIDs(getDashboard(t, surface.BaseURL, aRW)))
	})

	t.Run("rls isolation", func(t *testing.T) {
		// B never sees A's saved layout — gets its own default.
		d := getDashboard(t, surface.BaseURL, bRW)
		require.True(t, d.IsDefault, "merchant B must not see merchant A's saved dashboard")

		// B saves its own; A's stays intact.
		bLayout := map[string]any{
			"widgets": []map[string]any{{
				"id": "b-widget", "title": "B active subs", "viz": "stat",
				"query": map[string]any{
					"measures": []string{"subscriptions"},
					"filters":  map[string][]string{"status": {"active"}},
					"range":    map[string]string{"last": "7d"},
				},
				"grid": map[string]int{"x": 0, "y": 0, "w": 3, "h": 2},
			}},
		}
		status, body := requestJSON(t, http.MethodPut, surface.BaseURL+"/v1/merchant/dashboard", bRW, bLayout)
		require.Equalf(t, http.StatusOK, status, "B PUT: %s", string(body))
		require.Equal(t, []string{"b-widget"}, widgetIDs(getDashboard(t, surface.BaseURL, bRW)))
		require.Equal(t, []string{"failed-payments", "revenue"}, widgetIDs(getDashboard(t, surface.BaseURL, aRW)))

		// Two isolated rows exist for real (super pool sees both).
		var rows int
		require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM openrails.dashboard_configs`).Scan(&rows))
		require.Equal(t, 2, rows)
	})

	t.Run("permission gates", func(t *testing.T) {
		status, _ := requestJSON(t, http.MethodGet, surface.BaseURL+"/v1/merchant/dashboard", "", nil)
		require.Equal(t, http.StatusUnauthorized, status)

		// metrics:read suffices to view…
		status, _ = requestJSON(t, http.MethodGet, surface.BaseURL+"/v1/merchant/dashboard", aRO, nil)
		require.Equal(t, http.StatusOK, status)
		// …but not to rewrite the shared layout or spend LLM tokens.
		status, _ = requestJSON(t, http.MethodPut, surface.BaseURL+"/v1/merchant/dashboard", aRO, customLayout)
		require.Equal(t, http.StatusForbidden, status)
		status, _ = requestJSON(t, http.MethodPost, surface.BaseURL+"/v1/merchant/dashboard/widgets/generate", aRO,
			map[string]string{"prompt": "anything"})
		require.Equal(t, http.StatusForbidden, status)
	})
}

// scriptedLLM is the deterministic fake: responses play in order; every
// conversation is recorded so tests can assert the fed-back errors.
type scriptedLLM struct {
	responses []string
	calls     [][]dashboard.LLMMessage
}

func (f *scriptedLLM) Complete(_ context.Context, _ string, msgs []dashboard.LLMMessage) (string, error) {
	f.calls = append(f.calls, append([]dashboard.LLMMessage(nil), msgs...))
	i := len(f.calls) - 1
	if i >= len(f.responses) {
		i = len(f.responses) - 1
	}
	return f.responses[i], nil
}

const goldenWidgetJSON = `{"query":{"measures":["cancellations"],"by":["time"],"grain":"day","range":{"last":"7d"}},"title":"Cancellations per day","viz":"line"}`

func TestDashboardWidgetGenerate(t *testing.T) {
	ctx := context.Background()
	h := New(t, ctx)

	t.Run("unconfigured fails closed", func(t *testing.T) {
		surface := h.StartStandalone("usd",
			WithConsoleAssets(fixtureConsoleAssets()), // #754: enabled console requires assets
			WithConfig(func(cfg *config.Config) {
				cfg.AdminConsole = &config.AdminConsoleConfig{Enabled: true}
			}))
		token := surface.MintAPIKey(dbtest.TestMerchantSlug, "gen-off-"+uuid.NewString(),
			[]string{controlplane.PermMerchantMetricsRead, controlplane.PermMerchantDashboardUpdate})

		status, body := requestJSON(t, http.MethodPost, surface.BaseURL+"/v1/merchant/dashboard/widgets/generate",
			token, map[string]string{"prompt": "cancellations per day"})
		require.Equal(t, http.StatusNotImplemented, status)
		require.Contains(t, string(body), "LLM_API_KEY")

		// The console bootstrap hides the NL box.
		status, cfgBody, _ := getRaw(t, surface.BaseURL+"/admin/config.json")
		require.Equal(t, http.StatusOK, status)
		require.Contains(t, cfgBody, `"nl_widgets_enabled":false`)
	})

	// One config-armed surface for the fake-LLM flows: config.json advertises
	// the feature; the deterministic stub replaces the real Anthropic client
	// before any generate call (no network ever).
	surface := h.StartStandalone("usd",
		WithConsoleAssets(fixtureConsoleAssets()), // #754: enabled console requires assets
		WithConfig(func(cfg *config.Config) {
			cfg.AdminConsole = &config.AdminConsoleConfig{Enabled: true}
			cfg.LLM = &config.LLMConfig{APIKey: "test-key-never-used"}
		}))
	token := surface.MintAPIKey(dbtest.TestMerchantSlug, "gen-on-"+uuid.NewString(),
		[]string{controlplane.PermMerchantMetricsRead, controlplane.PermMerchantDashboardUpdate})
	svc := surface.App().Runtime.DashboardService
	require.True(t, svc.NLConfigured())

	t.Run("config.json advertises nl widgets when configured", func(t *testing.T) {
		status, cfgBody, _ := getRaw(t, surface.BaseURL+"/admin/config.json")
		require.Equal(t, http.StatusOK, status)
		require.Contains(t, cfgBody, `"nl_widgets_enabled":true`)
	})

	t.Run("golden happy path", func(t *testing.T) {
		fake := &scriptedLLM{responses: []string{goldenWidgetJSON}}
		svc.SetLLM(fake)
		status, body := requestJSON(t, http.MethodPost, surface.BaseURL+"/v1/merchant/dashboard/widgets/generate",
			token, map[string]string{"prompt": "count of users who cancelled per day, for the past 7 days"})
		require.Equalf(t, http.StatusOK, status, "generate: %s", string(body))

		var res struct {
			Query struct {
				Measures []string `json:"measures"`
				By       []string `json:"by"`
				Grain    string   `json:"grain"`
				Range    struct {
					Last string `json:"last"`
				} `json:"range"`
			} `json:"query"`
			Title string `json:"title"`
			Viz   string `json:"viz"`
		}
		require.NoError(t, json.Unmarshal(body, &res))
		require.Equal(t, []string{"cancellations"}, res.Query.Measures)
		require.Equal(t, []string{"time"}, res.Query.By)
		require.Equal(t, "day", res.Query.Grain)
		require.Equal(t, "7d", res.Query.Range.Last)
		require.Equal(t, "line", res.Viz)
		require.NotEmpty(t, res.Title)
		require.Len(t, fake.calls, 1)

		// The returned query is directly executable (preview contract).
		status, body = requestJSON(t, http.MethodPost, surface.BaseURL+"/v1/merchant/metrics/query", token,
			json.RawMessage(`{"measures":["cancellations"],"by":["time"],"grain":"day","range":{"last":"7d"}}`))
		require.Equalf(t, http.StatusOK, status, "generated query must execute: %s", string(body))
	})

	t.Run("retry loop feeds compiler errors back verbatim", func(t *testing.T) {
		invalid := `{"query":{"measures":["cancelations"],"by":["time"],"grain":"day","range":{"last":"7d"}},"title":"Cancellations","viz":"line"}`
		fake := &scriptedLLM{responses: []string{invalid, goldenWidgetJSON}}
		svc.SetLLM(fake)
		status, body := requestJSON(t, http.MethodPost, surface.BaseURL+"/v1/merchant/dashboard/widgets/generate",
			token, map[string]string{"prompt": "cancelled per day past week"})
		require.Equalf(t, http.StatusOK, status, "generate retry: %s", string(body))
		require.Contains(t, string(body), `"cancellations"`)
		require.Len(t, fake.calls, 2)
		feedback := fake.calls[1][2].Content
		require.Contains(t, feedback, "unknown_measure")
		require.Contains(t, feedback, "cancellations", "did-you-mean must be fed back")
	})

	t.Run("exhausted retries return the corrective errors", func(t *testing.T) {
		invalid := `{"query":{"measures":["nope"],"range":{"last":"7d"}},"title":"x","viz":"stat"}`
		fake := &scriptedLLM{responses: []string{invalid}}
		svc.SetLLM(fake)
		status, body := requestJSON(t, http.MethodPost, surface.BaseURL+"/v1/merchant/dashboard/widgets/generate",
			token, map[string]string{"prompt": "whatever"})
		require.Equal(t, http.StatusUnprocessableEntity, status)
		require.Contains(t, string(body), "unknown_measure")
		require.Len(t, fake.calls, 3, "initial + exactly 2 retries")
	})

	t.Run("empty prompt is a 400", func(t *testing.T) {
		status, _ := requestJSON(t, http.MethodPost, surface.BaseURL+"/v1/merchant/dashboard/widgets/generate",
			token, map[string]string{"prompt": "  "})
		require.Equal(t, http.StatusBadRequest, status)
	})
}
