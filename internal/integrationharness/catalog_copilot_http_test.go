//go:build integration

package integrationharness

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/controlplane"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/modules/copilot"
	"github.com/open-rails/openrails/internal/modules/dashboard"
	"github.com/open-rails/openrails/pkg/catalog"
)

// #779 catalog copilot: fail-closed consent gating (absent route + config.json flags),
// Q&A tool-loop execution against REAL catalog/subscription data (counts
// match the DB, not canned numbers), Phase 2 drafting producing a VALID
// wizard payload with direction defaults applied, a constraint-violating
// request refused with a typed reason + workaround (never a mutation), and
// flag-off meaning the draft_* tools are ABSENT from the tool list (not
// present-but-erroring). The LLM is a deterministic scripted stub injected
// via SetLLM — no network ever, same precedent as #756's metrics ask tests.

type copilotScriptLLM struct {
	script  func(msgs []dashboard.ToolMessage) *dashboard.ToolTurn
	convs   [][]dashboard.ToolMessage
	systems []string
}

func (f *copilotScriptLLM) Complete(context.Context, string, []dashboard.LLMMessage) (string, error) {
	return "", nil
}

func (f *copilotScriptLLM) CompleteTools(_ context.Context, system string, _ []dashboard.ToolDef, msgs []dashboard.ToolMessage, _ int) (*dashboard.ToolTurn, error) {
	f.convs = append(f.convs, append([]dashboard.ToolMessage(nil), msgs...))
	f.systems = append(f.systems, system)
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

	t.Run("keyless deployments do not advertise the route", func(t *testing.T) {
		surface := h.StartStandalone("usd")
		token := surface.MintAPIKey(dbtest.TestMerchantSlug, "copilot-keyless-"+uuid.NewString(),
			[]string{controlplane.PermMerchantCatalogRead})
		status, _ := askCopilotOnce(t, surface.BaseURL, token, "what do we sell?")
		require.Equal(t, http.StatusNotFound, status, "an unconfigured capability is an absent route, never a 501")
		status, _ = requestJSON(t, http.MethodPost, surface.BaseURL+"/v1/merchant/catalog/copilot/confirm", token, map[string]string{})
		require.Equal(t, http.StatusNotFound, status)
	})

	t.Run("key without consent does not advertise the route", func(t *testing.T) {
		surface := h.StartStandalone("usd",
			WithConsoleAssets(fixtureConsoleAssets()),
			WithConfig(func(cfg *config.Config) {
				cfg.AdminConsole = &config.AdminConsoleConfig{Enabled: true}
				cfg.LLM = &config.LLMConfig{APIKey: "test-key-never-used"} // catalog_copilot_enabled NOT set
			}))
		token := surface.MintAPIKey(dbtest.TestMerchantSlug, "copilot-noconsent-"+uuid.NewString(),
			[]string{controlplane.PermMerchantCatalogRead})
		status, _ := askCopilotOnce(t, surface.BaseURL, token, "what do we sell?")
		require.Equal(t, http.StatusNotFound, status)

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

	// Publish two real price versions. Two subscriptions keep the old price;
	// three use the new price, so both cohort counts come from PostgreSQL.
	productKey := "copilot-" + strings.ReplaceAll(uuid.NewString(), "-", "")
	priceKey := productKey + "-monthly"
	publish := func(token string, amount int64) {
		t.Helper()
		status, body := requestJSON(t, http.MethodPost, surface.BaseURL+"/v1/merchant/catalog/publish", token, map[string]any{
			"catalog": catalog.Manifest{
				Version: catalog.SupportedVersion,
				Products: []catalog.Product{{
					Key: productKey, DisplayName: "Copilot Product",
					Prices: []catalog.Price{{UnitAmount: amount, Currency: "USD", Duration: "30d", AutoRenew: true}},
				}},
			},
			"insert": true, "overwrite": true,
		})
		require.Equal(t, http.StatusOK, status, string(body))
	}
	getByKey := func(token string) struct {
		ID        openrails.PriceID   `json:"id"`
		ProductID openrails.ProductID `json:"product_id"`
	} {
		t.Helper()
		status, body := requestJSON(t, http.MethodGet, surface.BaseURL+"/v1/merchant/catalog/prices/by-key/"+priceKey, token, nil)
		require.Equal(t, http.StatusOK, status, string(body))
		var p struct {
			ID        openrails.PriceID   `json:"id"`
			ProductID openrails.ProductID `json:"product_id"`
		}
		require.NoError(t, json.Unmarshal(body, &p))
		return p
	}
	publish(token, 10_000_000)
	v1 := getByKey(token)
	oldSubscriptions := []uuid.UUID{seedRepriceSubscription(t, ctx, h, v1.ProductID, v1.ID), seedRepriceSubscription(t, ctx, h, v1.ProductID, v1.ID)}
	publish(token, 12_000_000)
	v2 := getByKey(token)
	require.NotEqual(t, v1.ID, v2.ID)
	for range 3 {
		seedRepriceSubscription(t, ctx, h, v2.ProductID, v2.ID)
	}

	t.Run("catalog counts history and empty migration list in one question", func(t *testing.T) {
		llm := &copilotScriptLLM{script: func(msgs []dashboard.ToolMessage) *dashboard.ToolTurn {
			if len(msgs) > 1 {
				return &dashboard.ToolTurn{Text: "one product, $12/mo, 3 active, 2 grandfathered."}
			}
			return &dashboard.ToolTurn{ToolCalls: []dashboard.ToolCall{
				{ID: "catalog", Name: "list_catalog", Input: json.RawMessage(fmt.Sprintf(`{"product_key":%q}`, productKey))},
				{ID: "history", Name: "price_history", Input: json.RawMessage(fmt.Sprintf(`{"price_key":%q}`, priceKey))},
				{ID: "batches", Name: "list_reprice_batches", Input: json.RawMessage(fmt.Sprintf(`{"price_key":%q}`, priceKey))},
			}}
		}}
		svc.SetLLM(llm)
		status, body := askCopilotOnce(t, surface.BaseURL, token, "catalog, history and pending migrations")
		require.Equal(t, http.StatusOK, status, string(body))
		var res copilotAskResp
		require.NoError(t, json.Unmarshal(body, &res))
		require.Len(t, res.Evidence, 3)
		require.Equal(t, "one product, $12/mo, 3 active, 2 grandfathered.", res.Answer)
		require.Equal(t, "list_catalog", res.Evidence[0].Tool)
		require.Regexp(t, priceKey+` \| 12\.00 USD \| monthly \| 3 \| 2`, res.Evidence[0].Summary)
		require.Len(t, strings.Split(res.Evidence[0].Summary, "\n"), 2, "product filter must exclude every other product")
		require.Greater(t, strings.Index(llm.systems[0], priceKey), 0)
		require.Less(t, strings.Index(llm.systems[0], priceKey), strings.Index(llm.systems[0], "Catalog doctrine"))
		require.Contains(t, llm.systems[0], time.Now().UTC().Format("2006-01-02"))
		history := res.Evidence[1].Summary
		require.Equal(t, "price_history", res.Evidence[1].Tool)
		require.Contains(t, history, "12.00 USD")
		require.Contains(t, history, "10.00 USD")
		require.Less(t, strings.Index(history, "12.00 USD"), strings.Index(history, "10.00 USD"))
		require.Contains(t, history, "current")
		require.Contains(t, history, "prior")
		require.Equal(t, "list_reprice_batches", res.Evidence[2].Tool)
		require.Contains(t, res.Evidence[2].Summary, "0 pending migrations")
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

	t.Run("price lookup corrects unknown keys without inventing evidence", func(t *testing.T) {
		llm := &copilotScriptLLM{script: func(msgs []dashboard.ToolMessage) *dashboard.ToolTurn {
			key := "missing-copilot-price"
			if len(msgs) == 3 {
				key = priceKey
			}
			if len(msgs) > 3 {
				return &dashboard.ToolTurn{Text: "found it"}
			}
			calls := []dashboard.ToolCall{{ID: fmt.Sprint(len(msgs)), Name: "get_price", Input: json.RawMessage(fmt.Sprintf(`{"price_key":%q}`, key))}}
			if len(msgs) == 1 {
				calls = append(calls, dashboard.ToolCall{ID: "unknown-arg", Name: "get_price", Input: json.RawMessage(fmt.Sprintf(`{"price_key":%q,"ignored":true}`, priceKey))})
			}
			return &dashboard.ToolTurn{ToolCalls: calls}
		}}
		svc.SetLLM(llm)
		status, body := askCopilotOnce(t, surface.BaseURL, token, "inspect price")
		require.Equal(t, http.StatusOK, status, string(body))
		var res copilotAskResp
		require.NoError(t, json.Unmarshal(body, &res))
		require.Len(t, res.Evidence, 1)
		require.Contains(t, res.Evidence[0].Summary, "active_subscribers: 3")
		require.Contains(t, res.Evidence[0].Summary, "grandfathered (on prior versions): 2")
		require.Contains(t, res.Evidence[0].Summary, "no pending migration")
		refusal := llm.convs[1][2].ToolResults[0]
		require.True(t, refusal.IsError)
		require.Contains(t, refusal.Content, "not found")
		require.Contains(t, refusal.Content, "list_catalog")
		unknown := llm.convs[1][2].ToolResults[1]
		require.True(t, unknown.IsError)
		require.Contains(t, unknown.Content, "unknown field")
	})
	t.Run("batch progress comes from persisted subscription reprices", func(t *testing.T) {
		ids := append(oldSubscriptions, seedRepriceSubscription(t, ctx, h, v1.ProductID, v1.ID))
		batch := uuid.New()
		_, err := h.Pool().Exec(ctx, `INSERT INTO openrails.reprice_batches(id,merchant_id,price_key,to_price_id,effective_at,subscriptions_matched) VALUES($1,$2,$3,$4,now(),3)`, batch, dbtest.TestMerchantID.UUID(), priceKey, v2.ID.UUID())
		require.NoError(t, err)
		for i, id := range ids {
			status := "scheduled"
			var applied *time.Time
			if i == 0 {
				status = "applied"
				now := time.Now()
				applied = &now
			}
			_, err = h.Pool().Exec(ctx, `INSERT INTO openrails.subscription_reprices(merchant_id,subscription_id,from_price_id,to_price_id,effective_at,status,reprice_batch_id,applied_at) VALUES($1,$2,$3,$4,now(),$5,$6,$7)`, dbtest.TestMerchantID.UUID(), id, v1.ID.UUID(), v2.ID.UUID(), status, batch, applied)
			require.NoError(t, err)
		}
		svc.SetLLM(&copilotScriptLLM{script: oneToolThenAnswer("list_reprice_batches", fmt.Sprintf(`{"price_key":%q}`, priceKey), "progress")})
		status, body := askCopilotOnce(t, surface.BaseURL, token, "progress")
		require.Equal(t, http.StatusOK, status, string(body))
		var res copilotAskResp
		require.NoError(t, json.Unmarshal(body, &res))
		require.Contains(t, res.Evidence[0].Summary, "1 applied / 2 scheduled / 0 canceled")
		require.Contains(t, res.Evidence[0].Summary, "3 matched")
	})

	t.Run("tool budget forces an answer after six real reads", func(t *testing.T) {
		llm := &copilotScriptLLM{script: func(msgs []dashboard.ToolMessage) *dashboard.ToolTurn {
			if len(msgs) == 15 {
				return &dashboard.ToolTurn{Text: "bounded answer"}
			}
			return &dashboard.ToolTurn{ToolCalls: []dashboard.ToolCall{{ID: fmt.Sprint(len(msgs)), Name: "list_catalog", Input: json.RawMessage(fmt.Sprintf(`{"product_key":%q}`, productKey))}}}
		}}
		svc.SetLLM(llm)
		status, body := askCopilotOnce(t, surface.BaseURL, token, "inspect repeatedly")
		require.Equal(t, http.StatusOK, status, string(body))
		var res copilotAskResp
		require.NoError(t, json.Unmarshal(body, &res))
		require.Equal(t, "bounded answer", res.Answer)
		require.Len(t, res.Evidence, 6)
		require.Len(t, llm.convs, 8)
		refusal := llm.convs[7][14].ToolResults[0]
		require.True(t, refusal.IsError)
		require.Contains(t, refusal.Content, "budget exhausted")
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
				Prices: []catalog.Price{{UnitAmount: 10_000_000, Currency: "USD", Duration: "30d", AutoRenew: true}},
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
					UnitAmount int64  `json:"unit_amount,string"`
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
		// CUR-6: UPPER is the canonical internal form, and the draft copies the
		// existing price row's currency verbatim. Lowercase survives only on the
		// three rail wires that demand it (Stripe catalog/invoice, FX) — this is
		// an OpenRails surface, so it must read back exactly what is stored.
		require.Equal(t, "USD", draft.PriceChange.CreatePrice.Currency)
		require.NotEmpty(t, draft.PriceChange.CreatePrice.ProductID)

		// The draft is a PROPOSAL only — nothing in the real catalog changed.
		status, priceBody := requestJSON(t, http.MethodGet, draftSurface.BaseURL+"/v1/merchant/catalog/prices/by-key/"+incPriceKey, draftToken, nil)
		require.Equal(t, http.StatusOK, status)
		var live struct {
			UnitAmount int64 `json:"unit_amount,string"`
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
					Prices: []catalog.Price{{UnitAmount: 8_000_000, Currency: "USD", Duration: "30d", AutoRenew: true}},
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
		require.Contains(t, draft.Refusal.Workaround, "archive")
		refusal := llm.convs[1][2].ToolResults[0]
		require.False(t, refusal.IsError)
		require.Contains(t, refusal.Content, "cross_product")
		require.Contains(t, refusal.Content, "no draft was created")

		// Confirm nothing was scheduled/mutated by this refused attempt.
		status, batchesBody := requestJSON(t, http.MethodGet, draftSurface.BaseURL+"/v1/merchant/reprices/batches?price_key="+incPriceKey, draftToken, nil)
		require.Equal(t, http.StatusOK, status)
		var batches struct {
			Items []json.RawMessage `json:"items"`
		}
		require.NoError(t, json.Unmarshal(batchesBody, &batches))
		require.Empty(t, batches.Items, "a refused draft must never create a reprice batch")
	})

	t.Run("draft defaults explicit date new tier and key correction", func(t *testing.T) {
		future := time.Now().UTC().AddDate(0, 0, 40).Format("2006-01-02")
		stripe := dbtest.EnsureTestPSP(ctx, t, h.Pool(), dbtest.TestMerchantID.UUID(), "stripe")
		_, err := h.Pool().Exec(ctx, `INSERT INTO openrails.price_psp_bindings(merchant_id,price_id,psp_id,price_ref) VALUES($1,$2,$3,'price-copilot')`, dbtest.TestMerchantID.UUID(), v2.ID.UUID(), stripe)
		require.NoError(t, err)
		cases := []struct {
			name, tool, args string
			amount           int64
			mode             string
		}{
			{"increase", "draft_price_change", fmt.Sprintf(`{"price_key":%q,"new_amount":15000000}`, priceKey), 15_000_000, "grandfather"},
			{"decrease", "draft_price_change", fmt.Sprintf(`{"price_key":%q,"new_amount":8000000}`, priceKey), 8_000_000, "migrate"},
			{"explicit date", "draft_price_change", fmt.Sprintf(`{"price_key":%q,"new_amount":15000000,"migration_mode":"migrate","effective_date":%q}`, priceKey, future), 15_000_000, "migrate"},
			{"new tier", "draft_catalog_diff", fmt.Sprintf(`{"product_key":%q,"new_price_key":%q,"unit_amount":6000000}`, productKey, priceKey+"-ads"), 6_000_000, ""},
		}
		var calls []dashboard.ToolCall
		for _, tc := range cases {
			calls = append(calls, dashboard.ToolCall{ID: tc.name, Name: tc.tool, Input: json.RawMessage(tc.args)})
		}
		before := time.Now().UTC()
		draftSvc.SetLLM(&copilotScriptLLM{script: func(msgs []dashboard.ToolMessage) *dashboard.ToolTurn {
			if len(msgs) == 1 {
				return &dashboard.ToolTurn{ToolCalls: calls}
			}
			return &dashboard.ToolTurn{Text: "drafted"}
		}})
		status, body := askCopilotOnce(t, draftSurface.BaseURL, draftToken, "compare pricing proposals")
		require.Equal(t, http.StatusOK, status, string(body))
		var proposals copilot.AskResult
		require.NoError(t, json.Unmarshal(body, &proposals))
		require.Len(t, proposals.Drafts, len(cases))
		for i, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				d := proposals.Drafts[i]
				if tc.mode == "" {
					require.Equal(t, "catalog_diff", d.Kind)
					require.NotNil(t, d.CatalogDiff)
					require.Equal(t, priceKey+"-ads", d.CatalogDiff.CreatePrice.Key)
					require.Equal(t, tc.amount, d.CatalogDiff.CreatePrice.UnitAmount)
					require.Equal(t, "USD", d.CatalogDiff.CreatePrice.Currency)
					require.Equal(t, v2.ProductID.UUID().String(), d.CatalogDiff.CreatePrice.ProductID)
				} else {
					require.Equal(t, "price_change", d.Kind)
					require.NotNil(t, d.PriceChange)
					require.Equal(t, tc.mode, d.PriceChange.MigrationMode)
					require.Equal(t, tc.amount, d.PriceChange.NewAmount)
					require.EqualValues(t, 12_000_000, d.PriceChange.CurrentAmount)
					require.Equal(t, 6, d.PriceChange.AffectedCount)
					require.Equal(t, "copilot", d.PriceChange.DraftedBy)
					require.NotEmpty(t, d.PriceChange.DraftID)
					require.Equal(t, v2.ProductID.UUID().String(), d.PriceChange.CreatePrice.ProductID)
					require.Equal(t, priceKey, d.PriceChange.CreatePrice.Key)
					require.Contains(t, d.PriceChange.CreatePrice.Providers, "stripe")
					if tc.name == "increase" {
						require.Equal(t, "increase", d.PriceChange.Direction)
						require.Nil(t, d.PriceChange.Reprice)
					} else {
						require.NotNil(t, d.PriceChange.Reprice)
						require.Equal(t, priceKey, d.PriceChange.Reprice.PriceKey)
						if tc.name == "decrease" {
							require.Equal(t, "decrease", d.PriceChange.Direction)
							require.False(t, d.PriceChange.Reprice.EffectiveAt.Before(before))
							require.False(t, d.PriceChange.Reprice.EffectiveAt.After(time.Now().UTC()))
						} else {
							require.Equal(t, future, d.PriceChange.Reprice.EffectiveAt.Format("2006-01-02"))
							require.Contains(t, d.PriceChange.ReviewText, d.PriceChange.Reprice.EffectiveAt.Format("Jan 2, 2006"))
						}
					}
				}
			})
		}
		require.Equal(t, v2.ID, getByKey(token).ID, "proposing must not change the catalog")
		llm := &copilotScriptLLM{script: func(msgs []dashboard.ToolMessage) *dashboard.ToolTurn {
			key := priceKey
			if len(msgs) == 3 {
				key = priceKey + "-ads"
			}
			if len(msgs) > 3 {
				return &dashboard.ToolTurn{Text: "fixed"}
			}
			return &dashboard.ToolTurn{ToolCalls: []dashboard.ToolCall{{ID: fmt.Sprint(len(msgs)), Name: "draft_catalog_diff", Input: json.RawMessage(fmt.Sprintf(`{"product_key":%q,"new_price_key":%q,"unit_amount":6000000}`, productKey, key))}}}
		}}
		draftSvc.SetLLM(llm)
		status, body = askCopilotOnce(t, draftSurface.BaseURL, draftToken, "correct collision")
		require.Equal(t, http.StatusOK, status, string(body))
		var res copilot.AskResult
		require.NoError(t, json.Unmarshal(body, &res))
		require.Len(t, res.Drafts, 1)
		refusal := llm.convs[1][2].ToolResults[0]
		require.True(t, refusal.IsError)
		require.Contains(t, refusal.Content, "already in use")
	})
	t.Run("cross currency is refused independently of cross product", func(t *testing.T) {
		eurKey := priceKey + "-eur"
		status, body := requestJSON(t, http.MethodPost, draftSurface.BaseURL+"/v1/merchant/catalog/prices", draftToken, map[string]any{"product_id": v2.ProductID.String(), "key": eurKey, "unit_amount": "11000000", "currency": "EUR"})
		require.Equal(t, http.StatusCreated, status, string(body))
		draftSvc.SetLLM(&copilotScriptLLM{script: oneToolThenAnswer("draft_price_change", fmt.Sprintf(`{"price_key":%q,"new_amount":1,"migrate_to_price_key":%q}`, priceKey, eurKey), "refused")})
		status, body = askCopilotOnce(t, draftSurface.BaseURL, draftToken, "cross currency")
		require.Equal(t, http.StatusOK, status, string(body))
		var res copilot.AskResult
		require.NoError(t, json.Unmarshal(body, &res))
		require.Len(t, res.Drafts, 1)
		require.Equal(t, "refused", res.Drafts[0].Kind)
		require.Equal(t, "cross_currency", res.Drafts[0].Refusal.Code)
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
				Prices: []catalog.Price{{UnitAmount: 9_000_000, Currency: "USD", Duration: "30d", AutoRenew: true}},
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
