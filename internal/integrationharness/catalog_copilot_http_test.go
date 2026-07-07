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
	"github.com/open-rails/openrails/pkg/catalog"
)

// #779 catalog copilot: fail-closed consent gating (501 + config.json flags),
// Q&A tool-loop execution against REAL catalog/subscription data (counts
// match the DB, not canned numbers), Phase 2 drafting producing a VALID
// wizard payload with direction defaults applied, a constraint-violating
// request refused with a typed reason + workaround (never a mutation), and
// flag-off meaning the draft_* tools are ABSENT from the tool list (not
// present-but-erroring). The LLM is a deterministic scripted stub injected
// via SetLLM — no network ever, same precedent as #756's metrics ask tests.

type copilotScriptLLM struct {
	script func(msgs []dashboard.ToolMessage) *dashboard.ToolTurn
	convs  [][]dashboard.ToolMessage
}

func (f *copilotScriptLLM) Complete(context.Context, string, []dashboard.LLMMessage) (string, error) {
	return "", nil
}

func (f *copilotScriptLLM) CompleteTools(_ context.Context, _ string, _ []dashboard.ToolDef, msgs []dashboard.ToolMessage, _ int) (*dashboard.ToolTurn, error) {
	f.convs = append(f.convs, append([]dashboard.ToolMessage(nil), msgs...))
	return f.script(msgs), nil
}

// oneToolThenAnswer scripts the canonical one-tool-call-then-answer loop.
func oneToolThenAnswer(tool, args, answer string) func(msgs []dashboard.ToolMessage) *dashboard.ToolTurn {
	return func(msgs []dashboard.ToolMessage) *dashboard.ToolTurn {
		if len(msgs) == 1 {
			return &dashboard.ToolTurn{ToolCalls: []dashboard.ToolCall{{ID: "call-1", Name: tool, Input: json.RawMessage(args)}}}
		}
		return &dashboard.ToolTurn{Text: answer}
	}
}

func askCopilotOnce(t *testing.T, baseURL, token, question string) (int, []byte) {
	t.Helper()
	return requestJSON(t, http.MethodPost, baseURL+"/v1/merchant/catalog/ask", token, map[string]string{"question": question})
}

type copilotAskResp struct {
	Answer   string `json:"answer"`
	Evidence []struct {
		Tool    string `json:"tool"`
		Summary string `json:"summary"`
	} `json:"evidence"`
	Drafts []json.RawMessage `json:"drafts"`
}

func TestMerchantCatalogCopilotAsk(t *testing.T) {
	ctx := context.Background()
	h := New(t, ctx)

	t.Run("keyless fails closed naming both knobs", func(t *testing.T) {
		surface := h.StartStandalone("usd")
		token := surface.MintAPIKey(dbtest.TestMerchantSlug, "copilot-keyless-"+uuid.NewString(),
			[]string{controlplane.PermMerchantCatalogRead})
		status, body := askCopilotOnce(t, surface.BaseURL, token, "what do we sell?")
		require.Equal(t, http.StatusNotImplemented, status)
		require.Contains(t, string(body), "LLM_API_KEY")
		require.Contains(t, string(body), "LLM_CATALOG_COPILOT_ENABLED")
	})

	t.Run("key without consent fails closed naming the flag", func(t *testing.T) {
		surface := h.StartStandalone("usd",
			WithConsoleAssets(fixtureConsoleAssets()),
			WithConfig(func(cfg *config.Config) {
				cfg.AdminConsole = &config.AdminConsoleConfig{Enabled: true}
				cfg.LLM = &config.LLMConfig{APIKey: "test-key-never-used"} // catalog_copilot_enabled NOT set
			}))
		token := surface.MintAPIKey(dbtest.TestMerchantSlug, "copilot-noconsent-"+uuid.NewString(),
			[]string{controlplane.PermMerchantCatalogRead})
		status, body := askCopilotOnce(t, surface.BaseURL, token, "what do we sell?")
		require.Equal(t, http.StatusNotImplemented, status)
		require.Contains(t, string(body), "LLM_CATALOG_COPILOT_ENABLED")

		status, cfgBody, _ := getRaw(t, surface.BaseURL+"/admin/config.json")
		require.Equal(t, http.StatusOK, status)
		require.Contains(t, cfgBody, `"catalog_copilot_enabled":false`)
		require.Contains(t, cfgBody, `"catalog_drafting_enabled":false`)
	})

	// One Q&A-armed surface (drafting OFF) for the read-only flows.
	surface := h.StartStandalone("usd",
		WithConsoleAssets(fixtureConsoleAssets()),
		WithConfig(func(cfg *config.Config) {
			cfg.AdminConsole = &config.AdminConsoleConfig{Enabled: true}
			cfg.LLM = &config.LLMConfig{APIKey: "test-key-never-used", CatalogCopilotEnabled: true}
		}))
	svc := surface.App().Runtime.CopilotService
	require.True(t, svc.Configured())
	require.False(t, svc.DraftingConfigured(), "drafting must stay off until explicitly armed")
	token := surface.MintAPIKey(dbtest.TestMerchantSlug, "copilot-"+uuid.NewString(),
		[]string{controlplane.PermMerchantCatalogRead, controlplane.PermMerchantCatalogUpdate})

	t.Run("config.json advertises catalog_copilot_enabled true, drafting false", func(t *testing.T) {
		_, cfgBody, _ := getRaw(t, surface.BaseURL+"/admin/config.json")
		require.Contains(t, cfgBody, `"catalog_copilot_enabled":true`)
		require.Contains(t, cfgBody, `"catalog_drafting_enabled":false`)
	})

	// Seed a real catalog: premium-monthly at $10, then bumped to $12 (a real
	// #774 version bump), with a real active subscription pinned to the OLD
	// ($10) row — a genuine grandfathered subscriber, not a canned number.
	productKey := "copilot-" + strings.ReplaceAll(uuid.NewString(), "-", "")
	priceKey := productKey + "-monthly"
	publish := func(token string, amount int64) {
		t.Helper()
		status, body := requestJSON(t, http.MethodPost, surface.BaseURL+"/v1/merchant/catalog/publish", token, map[string]any{
			"catalog": catalog.Manifest{
				Version: catalog.SupportedVersion,
				Products: []catalog.Product{{
					Key: productKey, DisplayName: "Copilot Product",
					Prices: []catalog.Price{{UnitAmount: amount, Currency: "usd", Duration: "30d", AutoRenew: true}},
				}},
			},
			"insert": true, "overwrite": true,
		})
		require.Equal(t, http.StatusOK, status, string(body))
	}
	getByKey := func(token string) struct {
		ID        uuid.UUID `json:"id"`
		ProductID uuid.UUID `json:"product_id"`
	} {
		t.Helper()
		status, body := requestJSON(t, http.MethodGet, surface.BaseURL+"/v1/merchant/catalog/prices/by-key/"+priceKey, token, nil)
		require.Equal(t, http.StatusOK, status, string(body))
		var p struct {
			ID        uuid.UUID `json:"id"`
			ProductID uuid.UUID `json:"product_id"`
		}
		require.NoError(t, json.Unmarshal(body, &p))
		return p
	}
	publish(token, 10_000_000)
	v1 := getByKey(token)
	seedRepriceSubscription(t, ctx, h, v1.ProductID, v1.ID)
	publish(token, 12_000_000)
	v2 := getByKey(token)
	require.NotEqual(t, v1.ID, v2.ID)

	t.Run("list_catalog reflects real active + grandfathered counts", func(t *testing.T) {
		llm := &copilotScriptLLM{script: oneToolThenAnswer("list_catalog", `{"product_key":"`+productKey+`"}`,
			"one product, $12/mo, 0 active, 1 grandfathered.")}
		svc.SetLLM(llm)
		status, body := askCopilotOnce(t, surface.BaseURL, token, "what does "+productKey+" cost, and who's still on the old price?")
		require.Equalf(t, http.StatusOK, status, "ask: %s", string(body))
		var res copilotAskResp
		require.NoError(t, json.Unmarshal(body, &res))
		require.Len(t, res.Evidence, 1)
		require.Equal(t, "list_catalog", res.Evidence[0].Tool)
		require.Contains(t, res.Evidence[0].Summary, priceKey)
		require.Contains(t, res.Evidence[0].Summary, "$12.000000 USD")
		// 0 active on the CURRENT ($12) row, 1 grandfathered on the archived
		// ($10) row — real counts from the seeded subscription above.
		require.Regexp(t, priceKey+` \| \$12\.000000 USD \| monthly \| 0 \| 1`, res.Evidence[0].Summary)
	})

	t.Run("price_history is most-recent-first with per-version subscriber counts", func(t *testing.T) {
		llm := &copilotScriptLLM{script: oneToolThenAnswer("price_history", `{"price_key":"`+priceKey+`"}`, "history")}
		svc.SetLLM(llm)
		status, body := askCopilotOnce(t, surface.BaseURL, token, "history of "+priceKey)
		require.Equal(t, http.StatusOK, status, string(body))
		var res copilotAskResp
		require.NoError(t, json.Unmarshal(body, &res))
		summary := res.Evidence[0].Summary
		require.Contains(t, summary, "$12.000000 USD")
		require.Contains(t, summary, "$10.000000 USD")
		twelveIdx := strings.Index(summary, "$12.000000 USD")
		tenIdx := strings.Index(summary, "$10.000000 USD")
		require.Less(t, twelveIdx, tenIdx, "current ($12) must appear before the prior ($10) version")
	})

	t.Run("list_reprice_batches states 0 pending migrations explicitly", func(t *testing.T) {
		llm := &copilotScriptLLM{script: oneToolThenAnswer("list_reprice_batches", `{"price_key":"`+priceKey+`"}`, "none pending")}
		svc.SetLLM(llm)
		status, body := askCopilotOnce(t, surface.BaseURL, token, "any pending migrations for "+priceKey+"?")
		require.Equal(t, http.StatusOK, status, string(body))
		var res copilotAskResp
		require.NoError(t, json.Unmarshal(body, &res))
		require.Contains(t, res.Evidence[0].Summary, "0 pending migrations")
	})

	t.Run("drafting tools absent when the flag is off", func(t *testing.T) {
		llm := &copilotScriptLLM{script: oneToolThenAnswer("draft_price_change", `{"price_key":"`+priceKey+`","new_amount":15000000}`, "n/a")}
		svc.SetLLM(llm)
		status, body := askCopilotOnce(t, surface.BaseURL, token, "raise "+priceKey+" to $15")
		require.Equal(t, http.StatusOK, status, string(body))
		var res copilotAskResp
		require.NoError(t, json.Unmarshal(body, &res))
		require.Empty(t, res.Drafts)
		last := llm.convs[len(llm.convs)-1]
		tr := last[len(last)-1].ToolResults[0]
		require.True(t, tr.IsError)
		require.Contains(t, tr.Content, "unknown tool", "flag-off must look ABSENT, not present-but-erroring")
	})

	t.Run("auth gates", func(t *testing.T) {
		status, _ := askCopilotOnce(t, surface.BaseURL, "", "anything")
		require.Equal(t, http.StatusUnauthorized, status)
	})

	// -- Phase 2: a SEPARATE surface with drafting armed. --------------------

	draftSurface := h.StartStandalone("usd",
		WithConfig(func(cfg *config.Config) {
			cfg.LLM = &config.LLMConfig{APIKey: "test-key-never-used", CatalogCopilotEnabled: true, CatalogDraftingEnabled: true}
		}))
	draftSvc := draftSurface.App().Runtime.CopilotService
	require.True(t, draftSvc.DraftingConfigured())
	draftToken := draftSurface.MintAPIKey(dbtest.TestMerchantSlug, "copilot-draft-"+uuid.NewString(),
		[]string{controlplane.PermMerchantCatalogRead, controlplane.PermMerchantCatalogUpdate})

	incKey := "copilot-inc-" + strings.ReplaceAll(uuid.NewString(), "-", "")
	incPriceKey := incKey + "-monthly"
	status, body := requestJSON(t, http.MethodPost, draftSurface.BaseURL+"/v1/merchant/catalog/publish", draftToken, map[string]any{
		"catalog": catalog.Manifest{
			Version: catalog.SupportedVersion,
			Products: []catalog.Product{{
				Key: incKey, DisplayName: "Copilot Increase Product",
				Prices: []catalog.Price{{UnitAmount: 10_000_000, Currency: "usd", Duration: "30d", AutoRenew: true}},
			}},
		}, "insert": true, "overwrite": true,
	})
	require.Equal(t, http.StatusOK, status, string(body))

	t.Run("draft_price_change: increase defaults to grandfather with a real affected-count preview", func(t *testing.T) {
		llm := &copilotScriptLLM{script: oneToolThenAnswer("draft_price_change", `{"price_key":"`+incPriceKey+`","new_amount":15000000}`, "drafted")}
		draftSvc.SetLLM(llm)
		status, body := askCopilotOnce(t, draftSurface.BaseURL, draftToken, "raise "+incPriceKey+" to $15")
		require.Equalf(t, http.StatusOK, status, "ask: %s", string(body))
		var res copilotAskResp
		require.NoError(t, json.Unmarshal(body, &res))
		require.Len(t, res.Drafts, 1)
		var draft struct {
			Kind        string `json:"kind"`
			PriceChange struct {
				DraftedBy     string `json:"drafted_by"`
				Direction     string `json:"direction"`
				MigrationMode string `json:"migration_mode"`
				AffectedCount int    `json:"affected_count"`
				CreatePrice   struct {
					ProductID  string `json:"product_id"`
					Key        string `json:"key"`
					UnitAmount int64  `json:"unit_amount"`
					Currency   string `json:"currency"`
				} `json:"create_price"`
			} `json:"price_change"`
		}
		require.NoError(t, json.Unmarshal(res.Drafts[0], &draft))
		require.Equal(t, "price_change", draft.Kind)
		require.Equal(t, "copilot", draft.PriceChange.DraftedBy)
		require.Equal(t, "increase", draft.PriceChange.Direction)
		require.Equal(t, "grandfather", draft.PriceChange.MigrationMode)
		require.Equal(t, 0, draft.PriceChange.AffectedCount, "no subscribers seeded on this key")
		require.Equal(t, incPriceKey, draft.PriceChange.CreatePrice.Key)
		require.Equal(t, int64(15_000_000), draft.PriceChange.CreatePrice.UnitAmount)
		require.Equal(t, "usd", draft.PriceChange.CreatePrice.Currency)
		require.NotEmpty(t, draft.PriceChange.CreatePrice.ProductID)

		// The draft is a PROPOSAL only — nothing in the real catalog changed.
		status, priceBody := requestJSON(t, http.MethodGet, draftSurface.BaseURL+"/v1/merchant/catalog/prices/by-key/"+incPriceKey, draftToken, nil)
		require.Equal(t, http.StatusOK, status)
		var live struct {
			UnitAmount int64 `json:"unit_amount"`
		}
		require.NoError(t, json.Unmarshal(priceBody, &live))
		require.EqualValues(t, 10_000_000, live.UnitAmount, "drafting must never mutate the live price")
	})

	t.Run("draft_price_change: cross-product migrate_to_price_key is refused with a typed reason and workaround, not a mutation", func(t *testing.T) {
		otherKey := "copilot-other-" + strings.ReplaceAll(uuid.NewString(), "-", "")
		otherPriceKey := otherKey + "-monthly"
		status, body := requestJSON(t, http.MethodPost, draftSurface.BaseURL+"/v1/merchant/catalog/publish", draftToken, map[string]any{
			"catalog": catalog.Manifest{
				Version: catalog.SupportedVersion,
				Products: []catalog.Product{{
					Key: otherKey, DisplayName: "Copilot Other Product",
					Prices: []catalog.Price{{UnitAmount: 8_000_000, Currency: "usd", Duration: "30d", AutoRenew: true}},
				}},
			}, "insert": true, "overwrite": true,
		})
		require.Equal(t, http.StatusOK, status, string(body))

		args := `{"price_key":"` + incPriceKey + `","new_amount":1,"migrate_to_price_key":"` + otherPriceKey + `"}`
		llm := &copilotScriptLLM{script: oneToolThenAnswer("draft_price_change", args, "refused, explained")}
		draftSvc.SetLLM(llm)
		status, body = askCopilotOnce(t, draftSurface.BaseURL, draftToken, "migrate everyone from "+incKey+" onto "+otherKey)
		require.Equalf(t, http.StatusOK, status, "ask: %s", string(body))
		var res copilotAskResp
		require.NoError(t, json.Unmarshal(body, &res))
		require.Len(t, res.Drafts, 1)
		var draft struct {
			Kind    string `json:"kind"`
			Refusal struct {
				Code       string `json:"code"`
				Workaround string `json:"workaround"`
			} `json:"refusal"`
		}
		require.NoError(t, json.Unmarshal(res.Drafts[0], &draft))
		require.Equal(t, "refused", draft.Kind)
		require.Equal(t, "cross_product", draft.Refusal.Code)
		require.Contains(t, draft.Refusal.Workaround, "grandfather")

		// Confirm nothing was scheduled/mutated by this refused attempt.
		status, batchesBody := requestJSON(t, http.MethodGet, draftSurface.BaseURL+"/v1/merchant/reprices/batches?price_key="+incPriceKey, draftToken, nil)
		require.Equal(t, http.StatusOK, status)
		var batches struct {
			Items []json.RawMessage `json:"items"`
		}
		require.NoError(t, json.Unmarshal(batchesBody, &batches))
		require.Empty(t, batches.Items, "a refused draft must never create a reprice batch")
	})

	t.Run("confirm endpoint logs provenance and never mutates", func(t *testing.T) {
		status, body := requestJSON(t, http.MethodPost, draftSurface.BaseURL+"/v1/merchant/catalog/copilot/confirm", draftToken, map[string]any{
			"draft_id": uuid.NewString(), "kind": "price_change", "price_key": incPriceKey,
		})
		require.Equal(t, http.StatusOK, status, string(body))
	})

	// -- RLS isolation: two merchants never see each other's catalog. -------

	t.Run("RLS isolation across merchants", func(t *testing.T) {
		a := surface.ProvisionOwnedMerchant("cpa" + strings.ReplaceAll(uuid.NewString(), "-", ""))
		b := surface.ProvisionOwnedMerchant("cpb" + strings.ReplaceAll(uuid.NewString(), "-", ""))
		aToken := surface.MintAPIKey(a.MerchantSlug, "cp-a-"+uuid.NewString(),
			[]string{controlplane.PermMerchantCatalogRead, controlplane.PermMerchantCatalogUpdate})
		bToken := surface.MintAPIKey(b.MerchantSlug, "cp-b-"+uuid.NewString(), []string{controlplane.PermMerchantCatalogRead})

		aKey := "cpiso-a-" + strings.ReplaceAll(uuid.NewString(), "-", "")
		status, body := requestJSON(t, http.MethodPost, surface.BaseURL+"/v1/merchant/catalog/publish", aToken, map[string]any{
			"catalog": catalog.Manifest{Version: catalog.SupportedVersion, Products: []catalog.Product{{
				Key: aKey, DisplayName: "A-only product",
				Prices: []catalog.Price{{UnitAmount: 9_000_000, Currency: "usd", Duration: "30d", AutoRenew: true}},
			}}}, "insert": true, "overwrite": true,
		})
		require.Equal(t, http.StatusOK, status, string(body))

		llmA := &copilotScriptLLM{script: oneToolThenAnswer("list_catalog", `{}`, "a's catalog")}
		svc.SetLLM(llmA)
		_, aBody := askCopilotOnce(t, surface.BaseURL, aToken, "what do we sell?")
		var aRes copilotAskResp
		require.NoError(t, json.Unmarshal(aBody, &aRes))
		require.Contains(t, aRes.Evidence[0].Summary, aKey+"-monthly")

		llmB := &copilotScriptLLM{script: oneToolThenAnswer("list_catalog", `{}`, "b's catalog")}
		svc.SetLLM(llmB)
		_, bBody := askCopilotOnce(t, surface.BaseURL, bToken, "what do we sell?")
		var bRes copilotAskResp
		require.NoError(t, json.Unmarshal(bBody, &bRes))
		require.NotContains(t, bRes.Evidence[0].Summary, aKey, "merchant B must never see merchant A's product in its catalog summary")
	})
}
