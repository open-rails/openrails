//go:build integration

package integrationharness

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/controlplane"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/modules/dashboard"
)

// #756 metrics Q&A: fail-closed consent gating (501 + config.json flag),
// tool-loop execution against the REAL metrics service on the caller's
// RLS-pinned merchant context (evidence == direct /query, cross-merchant
// isolation), and the per-merchant ask rate limit. The LLM is a deterministic
// scripted stub injected via SetLLM — no network ever.

// askScriptLLM scripts tool turns as a function of the conversation so far
// (deterministic across repeated asks) and records every conversation.
type askScriptLLM struct {
	script func(msgs []dashboard.ToolMessage) *dashboard.ToolTurn

	mu    sync.Mutex
	convs [][]dashboard.ToolMessage
}

func (f *askScriptLLM) Complete(context.Context, string, []dashboard.LLMMessage) (string, error) {
	return "", errors.New("askScriptLLM: text completion not scripted")
}

func (f *askScriptLLM) CompleteTools(_ context.Context, _ string, _ []dashboard.ToolDef, msgs []dashboard.ToolMessage, _ int) (*dashboard.ToolTurn, error) {
	f.mu.Lock()
	f.convs = append(f.convs, append([]dashboard.ToolMessage(nil), msgs...))
	f.mu.Unlock()
	return f.script(msgs), nil
}

// oneQueryThenAnswer scripts the canonical ask: one run_metrics_query call,
// then a prose answer.
func oneQueryThenAnswer(query, answer string) func(msgs []dashboard.ToolMessage) *dashboard.ToolTurn {
	return func(msgs []dashboard.ToolMessage) *dashboard.ToolTurn {
		if len(msgs) == 1 {
			return &dashboard.ToolTurn{ToolCalls: []dashboard.ToolCall{{
				ID: "call-1", Name: "run_metrics_query", Input: json.RawMessage(query),
			}}}
		}
		return &dashboard.ToolTurn{Text: answer}
	}
}

func askOnce(t *testing.T, baseURL, token, question string) (int, []byte) {
	t.Helper()
	return requestJSON(t, http.MethodPost, baseURL+"/v1/merchant/metrics/ask", token,
		map[string]string{"question": question})
}

type askEvidenceResp struct {
	Answer   string `json:"answer"`
	Evidence []struct {
		Query   json.RawMessage `json:"query"`
		Grain   string          `json:"grain"`
		Columns json.RawMessage `json:"columns"`
		Rows    json.RawMessage `json:"rows"`
	} `json:"evidence"`
}

func TestMerchantMetricsAsk(t *testing.T) {
	ctx := context.Background()
	h := New(t, ctx)

	t.Run("keyless fails closed naming both knobs", func(t *testing.T) {
		surface := h.StartStandalone("usd")
		token := surface.MintAPIKey(dbtest.TestMerchantSlug, "ask-keyless-"+uuid.NewString(),
			[]string{controlplane.PermMerchantMetricsRead})
		status, body := askOnce(t, surface.BaseURL, token, "how is revenue?")
		require.Equal(t, http.StatusNotImplemented, status)
		require.Contains(t, string(body), "LLM_API_KEY")
		require.Contains(t, string(body), "LLM_ASK_ENABLED")
	})

	t.Run("key without ask consent fails closed naming the consent flag", func(t *testing.T) {
		surface := h.StartStandalone("usd",
			WithConsoleAssets(fixtureConsoleAssets()),
			WithConfig(func(cfg *config.Config) {
				cfg.AdminConsole = &config.AdminConsoleConfig{Enabled: true}
				cfg.LLM = &config.LLMConfig{APIKey: "test-key-never-used"} // ask_enabled NOT set
			}))
		token := surface.MintAPIKey(dbtest.TestMerchantSlug, "ask-noconsent-"+uuid.NewString(),
			[]string{controlplane.PermMerchantMetricsRead})
		status, body := askOnce(t, surface.BaseURL, token, "how is revenue?")
		require.Equal(t, http.StatusNotImplemented, status)
		require.Contains(t, string(body), "LLM_ASK_ENABLED")
		require.Contains(t, string(body), "RESULTS", "the message must state the data-flow difference")

		// config.json: NL widgets armed, ask not — distinct consents.
		status, cfgBody, _ := getRaw(t, surface.BaseURL+"/admin/config.json")
		require.Equal(t, http.StatusOK, status)
		require.Contains(t, cfgBody, `"nl_widgets_enabled":true`)
		require.Contains(t, cfgBody, `"ask_enabled":false`)
	})

	// One armed surface for the live flows.
	surface := h.StartStandalone("usd",
		WithConsoleAssets(fixtureConsoleAssets()),
		WithConfig(func(cfg *config.Config) {
			cfg.AdminConsole = &config.AdminConsoleConfig{Enabled: true}
			cfg.LLM = &config.LLMConfig{APIKey: "test-key-never-used", AskEnabled: true}
		}))
	svc := surface.App().Runtime.DashboardService
	require.True(t, svc.AskConfigured())

	t.Run("config.json advertises ask when armed", func(t *testing.T) {
		status, cfgBody, _ := getRaw(t, surface.BaseURL+"/admin/config.json")
		require.Equal(t, http.StatusOK, status)
		require.Contains(t, cfgBody, `"ask_enabled":true`)
	})

	// Two merchants with DIFFERENT seeded usage so isolation is observable.
	a := surface.ProvisionOwnedMerchant("aska" + strings.ReplaceAll(uuid.NewString(), "-", ""))
	b := surface.ProvisionOwnedMerchant("askb" + strings.ReplaceAll(uuid.NewString(), "-", ""))
	aToken := surface.MintAPIKey(a.MerchantSlug, "ask-a-"+uuid.NewString(),
		[]string{controlplane.PermMerchantMetricsRead})
	bToken := surface.MintAPIKey(b.MerchantSlug, "ask-b-"+uuid.NewString(),
		[]string{controlplane.PermMerchantMetricsRead})

	pool := h.sharedPool()
	// usage_units = COUNT(usage_events): seed a DIFFERENT event count per
	// merchant so cross-merchant leakage would be visible in the numbers.
	seedUsage := func(merchantID uuid.UUID, events int) {
		customerID := uuid.New()
		_, err := pool.Exec(ctx,
			`INSERT INTO openrails.customers (id, merchant_id, subject) VALUES ($1, $2, $3)`,
			customerID, merchantID, "ask-usage-"+uuid.NewString())
		require.NoError(t, err)
		for i := 0; i < events; i++ {
			_, err = pool.Exec(ctx,
				`INSERT INTO openrails.usage_events (merchant_id, customer_id, invoker_id, currency, resource, event_type, amount, source, source_id)
				 VALUES ($1, $2, 'ask-test', 'usd', 'api', 'call', 1000, 'test', $3)`,
				merchantID, customerID, uuid.NewString())
			require.NoError(t, err)
		}
	}
	const aUnits, bUnits = 3, 5
	seedUsage(uuid.UUID(a.MerchantID), aUnits)
	seedUsage(uuid.UUID(b.MerchantID), bUnits)

	unitsOf := func(t *testing.T, rows json.RawMessage) float64 {
		t.Helper()
		var parsed [][]any
		require.NoError(t, json.Unmarshal(rows, &parsed))
		require.Len(t, parsed, 1)
		require.Len(t, parsed[0], 1)
		n, ok := parsed[0][0].(float64)
		require.True(t, ok, "usage_units cell must be numeric, got %T", parsed[0][0])
		return n
	}

	const usageQuery = `{"measures":["usage_units"],"range":{"last":"7d"}}`

	t.Run("tool loop executes on the caller's merchant; evidence is the verbatim query result", func(t *testing.T) {
		svc.SetLLM(&askScriptLLM{script: oneQueryThenAnswer(usageQuery, "You used 3 units in the past 7 days.")})

		status, body := askOnce(t, surface.BaseURL, aToken, "how many usage units this week?")
		require.Equalf(t, http.StatusOK, status, "ask: %s", string(body))
		var res askEvidenceResp
		require.NoError(t, json.Unmarshal(body, &res))
		require.Equal(t, "You used 3 units in the past 7 days.", res.Answer)
		require.Len(t, res.Evidence, 1)

		// Evidence must match a DIRECT /query call for merchant A exactly.
		status, direct := requestJSON(t, http.MethodPost, surface.BaseURL+"/v1/merchant/metrics/query", aToken,
			json.RawMessage(usageQuery))
		require.Equalf(t, http.StatusOK, status, "direct query: %s", string(direct))
		var directRes struct {
			Columns json.RawMessage `json:"columns"`
			Rows    json.RawMessage `json:"rows"`
		}
		require.NoError(t, json.Unmarshal(direct, &directRes))
		require.JSONEq(t, string(directRes.Columns), string(res.Evidence[0].Columns))
		require.JSONEq(t, string(directRes.Rows), string(res.Evidence[0].Rows))

		// RLS isolation: A sees A's count, never B's.
		require.EqualValues(t, aUnits, unitsOf(t, res.Evidence[0].Rows))

		// The same question as B sees B's numbers — same query, different rows.
		svc.SetLLM(&askScriptLLM{script: oneQueryThenAnswer(usageQuery, "5 units.")})
		status, bBody := askOnce(t, surface.BaseURL, bToken, "how many usage units this week?")
		require.Equalf(t, http.StatusOK, status, "ask as B: %s", string(bBody))
		var bRes askEvidenceResp
		require.NoError(t, json.Unmarshal(bBody, &bRes))
		require.Len(t, bRes.Evidence, 1)
		require.EqualValues(t, bUnits, unitsOf(t, bRes.Evidence[0].Rows))
	})

	t.Run("invalid tool args get corrective errors fed back, corrected call executes", func(t *testing.T) {
		invalid := `{"measures":["usage_unitz"],"range":{"last":"7d"}}`
		fake := &askScriptLLM{script: func(msgs []dashboard.ToolMessage) *dashboard.ToolTurn {
			switch len(msgs) {
			case 1:
				return &dashboard.ToolTurn{ToolCalls: []dashboard.ToolCall{{ID: "bad", Name: "run_metrics_query", Input: json.RawMessage(invalid)}}}
			case 3:
				return &dashboard.ToolTurn{ToolCalls: []dashboard.ToolCall{{ID: "good", Name: "run_metrics_query", Input: json.RawMessage(usageQuery)}}}
			default:
				return &dashboard.ToolTurn{Text: "1111 units."}
			}
		}}
		svc.SetLLM(fake)
		status, body := askOnce(t, surface.BaseURL, aToken, "usage units, week")
		require.Equalf(t, http.StatusOK, status, "ask: %s", string(body))
		var res askEvidenceResp
		require.NoError(t, json.Unmarshal(body, &res))
		require.Len(t, res.Evidence, 1, "only the corrected call may produce evidence")

		// The corrective errors (did-you-mean) rode back into the conversation.
		fake.mu.Lock()
		defer fake.mu.Unlock()
		require.GreaterOrEqual(t, len(fake.convs), 2)
		second := fake.convs[1]
		feedback := second[len(second)-1].ToolResults[0]
		require.True(t, feedback.IsError)
		require.Contains(t, feedback.Content, "unknown_measure")
		require.Contains(t, feedback.Content, "usage_units", "did-you-mean must ride back")
	})

	t.Run("can't-answer path returns prose with no evidence", func(t *testing.T) {
		svc.SetLLM(&askScriptLLM{script: func([]dashboard.ToolMessage) *dashboard.ToolTurn {
			return &dashboard.ToolTurn{Text: "The metrics schema has no page-view data, so I cannot answer that."}
		}})
		status, body := askOnce(t, surface.BaseURL, aToken, "how many page views yesterday?")
		require.Equal(t, http.StatusOK, status)
		var res askEvidenceResp
		require.NoError(t, json.Unmarshal(body, &res))
		require.Empty(t, res.Evidence)
		require.Contains(t, res.Answer, "no page-view data")
	})

	t.Run("auth gates", func(t *testing.T) {
		status, _ := askOnce(t, surface.BaseURL, "", "anything")
		require.Equal(t, http.StatusUnauthorized, status)
		// Ask is gated on metrics:read only (#733 gating): the read-only viewer
		// role can ask — every merchant catalog role carries metrics:read, so
		// there is no mintable merchant key without it to probe a 403 here (the
		// permission middleware's deny path is covered by the dashboard tests).
		viewer := surface.MintAPIKey(a.MerchantSlug, "ask-viewer-"+uuid.NewString(),
			[]string{controlplane.PermMerchantMetricsRead, controlplane.PermMerchantCatalogRead})
		svc.SetLLM(&askScriptLLM{script: func([]dashboard.ToolMessage) *dashboard.ToolTurn {
			return &dashboard.ToolTurn{Text: "viewer answer"}
		}})
		status, body := askOnce(t, surface.BaseURL, viewer, "anything")
		require.Equalf(t, http.StatusOK, status, "viewer (metrics:read) must be allowed to ask: %s", string(body))
	})

	t.Run("per-merchant rate limit trips with Retry-After", func(t *testing.T) {
		// Fresh merchant so earlier subtests' asks don't count against it.
		c := surface.ProvisionOwnedMerchant("askc" + strings.ReplaceAll(uuid.NewString(), "-", ""))
		cToken := surface.MintAPIKey(c.MerchantSlug, "ask-c-"+uuid.NewString(),
			[]string{controlplane.PermMerchantMetricsRead})
		svc.SetLLM(&askScriptLLM{script: func([]dashboard.ToolMessage) *dashboard.ToolTurn {
			return &dashboard.ToolTurn{Text: "quick answer"}
		}})

		// 10/minute per merchant: fire enough requests that at least one 429
		// must occur even if a fixed-window boundary rolls mid-test.
		var limited *http.Response
		okCount := 0
		for i := 0; i < 25; i++ {
			req, err := http.NewRequestWithContext(ctx, http.MethodPost, surface.BaseURL+"/v1/merchant/metrics/ask",
				bytes.NewReader([]byte(`{"question":"q`+fmt.Sprint(i)+`"}`)))
			require.NoError(t, err)
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Authorization", "Bearer "+cToken)
			resp, err := http.DefaultClient.Do(req)
			require.NoError(t, err)
			if resp.StatusCode == http.StatusTooManyRequests {
				limited = resp
				break
			}
			require.Equal(t, http.StatusOK, resp.StatusCode)
			okCount++
			resp.Body.Close()
		}
		require.NotNil(t, limited, "ask rate limit never tripped in 25 requests (allowed %d)", okCount)
		require.NotEmpty(t, limited.Header.Get("Retry-After"))
		limited.Body.Close()
		// Merchant A is unaffected by C's exhaustion.
		status, body := askOnce(t, surface.BaseURL, aToken, "still fine?")
		require.Equalf(t, http.StatusOK, status, "merchant A must not share C's budget: %s", string(body))
	})
}
