package copilot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jonboulle/clockwork"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/modules/dashboard"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/pkg/merchant"
	"github.com/open-rails/openrails/pkg/query"
)

// -- fakes ---------------------------------------------------------------------

// scriptLLM plays ToolTurns in order (last repeats) and records every
// conversation + system prompt — mirrors dashboard's toolScriptLLM.
type scriptLLM struct {
	turns   []*dashboard.ToolTurn
	convs   [][]dashboard.ToolMessage
	systems []string
}

func (f *scriptLLM) Complete(context.Context, string, []dashboard.LLMMessage) (string, error) {
	return "", errors.New("scriptLLM: text completion not scripted")
}

func (f *scriptLLM) CompleteTools(_ context.Context, system string, _ []dashboard.ToolDef, msgs []dashboard.ToolMessage, _ int) (*dashboard.ToolTurn, error) {
	f.systems = append(f.systems, system)
	f.convs = append(f.convs, append([]dashboard.ToolMessage(nil), msgs...))
	i := len(f.convs) - 1
	if i >= len(f.turns) {
		i = len(f.turns) - 1
	}
	return f.turns[i], nil
}

func toolCall(id, name, args string) *dashboard.ToolTurn {
	return &dashboard.ToolTurn{ToolCalls: []dashboard.ToolCall{{ID: id, Name: name, Input: json.RawMessage(args)}}}
}

type fakeProducts struct {
	byKey map[string]*models.Product
	byID  map[uuid.UUID]*models.Product
	list  []*models.Product
}

func (f *fakeProducts) GetActive(context.Context) ([]*models.Product, error) { return f.list, nil }
func (f *fakeProducts) GetByKey(_ context.Context, key string) (*models.Product, error) {
	if p, ok := f.byKey[key]; ok {
		return p, nil
	}
	return nil, fmt.Errorf("product %q not found", key)
}
func (f *fakeProducts) GetByID(_ context.Context, id uuid.UUID) (*models.Product, error) {
	if p, ok := f.byID[id]; ok {
		return p, nil
	}
	return nil, fmt.Errorf("product %s not found", id)
}

type fakePrices struct {
	current   map[string]*models.Price // key -> current row
	byID      map[uuid.UUID]*models.Price
	active    map[uuid.UUID][]*models.Price // productID -> active prices
	movements map[string][]*models.PriceKeyMovement
}

func (f *fakePrices) GetActiveByProductID(_ context.Context, productID uuid.UUID) ([]*models.Price, error) {
	return f.active[productID], nil
}
func (f *fakePrices) GetCurrentByKey(_ context.Context, _ uuid.UUID, key string) (*models.Price, error) {
	if p, ok := f.current[key]; ok {
		return p, nil
	}
	return nil, fmt.Errorf("price key %q not found", key)
}
func (f *fakePrices) ListChainByKey(context.Context, uuid.UUID, string) ([]*models.Price, error) {
	return nil, nil
}
func (f *fakePrices) ListKeyMovements(_ context.Context, _ uuid.UUID, key string) ([]*models.PriceKeyMovement, error) {
	return f.movements[key], nil
}
func (f *fakePrices) GetByID(_ context.Context, id uuid.UUID) (*models.Price, error) {
	if p, ok := f.byID[id]; ok {
		return p, nil
	}
	return nil, fmt.Errorf("price %s not found", id)
}

type fakeSubs struct{ counts map[uuid.UUID]int64 }

func (f *fakeSubs) GetSubscribers(_ context.Context, params query.QueryOptions[subscriptions.GetSubscriptionsFilters]) ([]*models.Subscription, int64, error) {
	return nil, f.counts[params.Filters.PriceID], nil
}

type fakeReprices struct {
	previews map[string]*subscriptions.RepricePreviewResult
	batches  map[string][]*models.RepriceBatch
	repRows  map[uuid.UUID][]*models.SubscriptionReprice
}

func (f *fakeReprices) PreviewAllPriorVersions(_ context.Context, key string) (*subscriptions.RepricePreviewResult, error) {
	if p, ok := f.previews[key]; ok {
		return p, nil
	}
	return &subscriptions.RepricePreviewResult{PriceKey: key}, nil
}
func (f *fakeReprices) ListBatchesForKey(_ context.Context, key string, _, _ int) ([]*models.RepriceBatch, error) {
	return f.batches[key], nil
}
func (f *fakeReprices) List(_ context.Context, filter subscriptions.SubscriptionRepriceFilter, _, _ int) ([]*models.SubscriptionReprice, error) {
	if filter.RepriceBatchID == nil {
		return nil, nil
	}
	return f.repRows[*filter.RepriceBatchID], nil
}

// testFixture is a small two-product catalog shared by most tests:
// - premium-monthly: $10, product "premium", 3 active subscribers, 2 grandfathered.
// - basic-monthly: $5, product "basic", eur currency (for cross-currency tests).
type testFixture struct {
	premiumProduct, basicProduct *models.Product
	premiumV1, premiumV2, basic  *models.Price
	products                     *fakeProducts
	prices                       *fakePrices
	subs                         *fakeSubs
	reprices                     *fakeReprices
}

func newFixture() *testFixture {
	premiumProduct := &models.Product{ID: uuid.New(), Key: "premium", DisplayName: "Premium"}
	basicProduct := &models.Product{ID: uuid.New(), Key: "basic", DisplayName: "Basic"}
	dur := 720

	premiumV1 := &models.Price{ID: uuid.New(), ProductID: premiumProduct.ID, Key: "premium-monthly",
		Amount: 10_000_000, Currency: "usd", AccessDurationHours: &dur, AutoRenew: true, Archived: true}
	premiumV2 := &models.Price{ID: uuid.New(), ProductID: premiumProduct.ID, Key: "premium-monthly",
		Amount: 12_000_000, Currency: "usd", AccessDurationHours: &dur, AutoRenew: true,
		PSPLinks: map[string]map[string]string{"stripe": {"price_id": "price_123"}}}
	basic := &models.Price{ID: uuid.New(), ProductID: basicProduct.ID, Key: "basic-monthly",
		Amount: 5_000_000, Currency: "eur", AccessDurationHours: &dur, AutoRenew: true}

	f := &testFixture{
		premiumProduct: premiumProduct, basicProduct: basicProduct,
		premiumV1: premiumV1, premiumV2: premiumV2, basic: basic,
	}
	f.products = &fakeProducts{
		byKey: map[string]*models.Product{"premium": premiumProduct, "basic": basicProduct},
		byID:  map[uuid.UUID]*models.Product{premiumProduct.ID: premiumProduct, basicProduct.ID: basicProduct},
		list:  []*models.Product{premiumProduct, basicProduct},
	}
	f.prices = &fakePrices{
		current: map[string]*models.Price{"premium-monthly": premiumV2, "basic-monthly": basic},
		byID:    map[uuid.UUID]*models.Price{premiumV1.ID: premiumV1, premiumV2.ID: premiumV2, basic.ID: basic},
		active:  map[uuid.UUID][]*models.Price{premiumProduct.ID: {premiumV2}, basicProduct.ID: {basic}},
		movements: map[string][]*models.PriceKeyMovement{
			"premium-monthly": {
				{Key: "premium-monthly", PriceID: premiumV2.ID, EffectiveAt: time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)},
				{Key: "premium-monthly", PriceID: premiumV1.ID, EffectiveAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)},
			},
		},
	}
	f.subs = &fakeSubs{counts: map[uuid.UUID]int64{premiumV2.ID: 3, premiumV1.ID: 2, basic.ID: 1}}
	f.reprices = &fakeReprices{
		previews: map[string]*subscriptions.RepricePreviewResult{
			"premium-monthly": {PriceKey: "premium-monthly", ToPriceID: premiumV2.ID, Matched: 5},
		},
		batches: map[string][]*models.RepriceBatch{},
		repRows: map[uuid.UUID][]*models.SubscriptionReprice{},
	}
	return f
}

func (f *testFixture) service(llm dashboard.LLM, drafting bool) *Service {
	return NewService(Deps{
		Products: f.products, Prices: f.prices, Subs: f.subs, Reprices: f.reprices,
		LLM: llm, Enabled: true, Drafting: drafting,
		Clock: clockwork.NewFakeClockAt(time.Date(2026, 7, 7, 0, 0, 0, 0, time.UTC)),
	})
}

var testMerchantID = merchant.ID(uuid.New())

// ctxWithMerchant is the ambient context every real HTTP request carries
// (merchant resolved by middleware before any merchant-owned code runs) —
// the tool handlers call merchant.Require(ctx) just like the production
// catalog/reprice services do.
func ctxWithMerchant() context.Context {
	return merchant.WithID(context.Background(), testMerchantID)
}

func mustAsk(t *testing.T, s *Service, q string) *AskResult {
	t.Helper()
	res, err := s.Ask(ctxWithMerchant(), q)
	require.NoError(t, err)
	return res
}

// -- Phase 1 Q&A tests -----------------------------------------------------------

func TestAsk_ListCatalogInlineAggregates(t *testing.T) {
	f := newFixture()
	llm := &scriptLLM{turns: []*dashboard.ToolTurn{
		toolCall("t1", toolListCatalog, `{}`),
		{Text: "You sell Premium at $12/mo (3 active, 2 grandfathered) and Basic at €5/mo."},
	}}
	res := mustAsk(t, f.service(llm, false), "what do we sell?")
	require.Equal(t, "You sell Premium at $12/mo (3 active, 2 grandfathered) and Basic at €5/mo.", res.Answer)
	require.Len(t, res.Evidence, 1)
	require.Equal(t, toolListCatalog, res.Evidence[0].Tool)
	require.Contains(t, res.Evidence[0].Summary, "premium-monthly")
	require.Contains(t, res.Evidence[0].Summary, "3") // active subs
	require.Contains(t, res.Evidence[0].Summary, "2") // grandfathered
}

func TestAsk_ListCatalogEmptyStateIsExplicit(t *testing.T) {
	f := newFixture()
	f.products.list = nil
	llm := &scriptLLM{turns: []*dashboard.ToolTurn{
		toolCall("t1", toolListCatalog, `{}`),
		{Text: "nothing"},
	}}
	res := mustAsk(t, f.service(llm, false), "what do we sell?")
	require.Equal(t, "0 active products/prices in the catalog.", res.Evidence[0].Summary)
}

func TestAsk_ListCatalogScopedToProduct(t *testing.T) {
	f := newFixture()
	llm := &scriptLLM{turns: []*dashboard.ToolTurn{
		toolCall("t1", toolListCatalog, `{"product_key":"basic"}`),
		{Text: "just basic"},
	}}
	res := mustAsk(t, f.service(llm, false), "what does basic cost?")
	require.Contains(t, res.Evidence[0].Summary, "basic-monthly")
	require.NotContains(t, res.Evidence[0].Summary, "premium-monthly")
}

func TestAsk_GetPriceDrillDown(t *testing.T) {
	f := newFixture()
	llm := &scriptLLM{turns: []*dashboard.ToolTurn{
		toolCall("t1", toolGetPrice, `{"price_key":"premium-monthly"}`),
		{Text: "premium-monthly is $12/mo, 3 active, 2 grandfathered, no pending migration."},
	}}
	res := mustAsk(t, f.service(llm, false), "tell me about premium-monthly")
	require.Contains(t, res.Evidence[0].Summary, "active_subscribers: 3")
	require.Contains(t, res.Evidence[0].Summary, "grandfathered (on prior versions): 2")
	require.Contains(t, res.Evidence[0].Summary, "no pending migration")
}

func TestAsk_GetPriceUnknownKeyIsCorrective(t *testing.T) {
	f := newFixture()
	llm := &scriptLLM{turns: []*dashboard.ToolTurn{
		toolCall("bad", toolGetPrice, `{"price_key":"nope"}`),
		toolCall("good", toolGetPrice, `{"price_key":"premium-monthly"}`),
		{Text: "found it"},
	}}
	res := mustAsk(t, f.service(llm, false), "tell me about nope")
	require.Len(t, res.Evidence, 1, "the failed lookup must not produce evidence")
	second := llm.convs[1]
	tr := second[len(second)-1].ToolResults[0]
	require.True(t, tr.IsError)
	require.Contains(t, tr.Content, "not found")
	require.Contains(t, tr.Content, "list_catalog", "the corrective next step must be named")
}

func TestAsk_PriceHistoryMostRecentFirstWithPerVersionCounts(t *testing.T) {
	f := newFixture()
	llm := &scriptLLM{turns: []*dashboard.ToolTurn{
		toolCall("t1", toolPriceHistory, `{"price_key":"premium-monthly"}`),
		{Text: "history"},
	}}
	res := mustAsk(t, f.service(llm, false), "who's still on the old premium price?")
	lines := res.Evidence[0].Summary
	require.Contains(t, lines, "2026-06-01")
	require.Contains(t, lines, "2026-01-01")
	// Most-recent-first: the June row appears before the January row.
	require.Less(t, indexOf(lines, "2026-06-01"), indexOf(lines, "2026-01-01"))
	require.Contains(t, lines, "current")
	require.Contains(t, lines, "prior")
}

func TestAsk_ListRepriceBatchesEmptyStateIsExplicit(t *testing.T) {
	f := newFixture()
	llm := &scriptLLM{turns: []*dashboard.ToolTurn{
		toolCall("t1", toolListRepriceBatches, `{"price_key":"premium-monthly"}`),
		{Text: "none pending"},
	}}
	res := mustAsk(t, f.service(llm, false), "any pending migrations for premium?")
	require.Equal(t, `0 pending migrations for price key "premium-monthly".`, res.Evidence[0].Summary)
}

func TestAsk_ListRepriceBatchesProgress(t *testing.T) {
	f := newFixture()
	batchID := uuid.New()
	f.reprices.batches["premium-monthly"] = []*models.RepriceBatch{
		{ID: batchID, EffectiveAt: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC), SubscriptionsMatched: 3, CreatedAt: time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)},
	}
	f.reprices.repRows[batchID] = []*models.SubscriptionReprice{
		{Status: models.RepriceStatusApplied}, {Status: models.RepriceStatusScheduled}, {Status: models.RepriceStatusScheduled},
	}
	llm := &scriptLLM{turns: []*dashboard.ToolTurn{
		toolCall("t1", toolListRepriceBatches, `{"price_key":"premium-monthly"}`),
		{Text: "one batch"},
	}}
	res := mustAsk(t, f.service(llm, false), "is there a migration running for premium?")
	require.Contains(t, res.Evidence[0].Summary, "1 applied / 2 scheduled / 0 canceled")
	require.Contains(t, res.Evidence[0].Summary, "3 matched")
}

func indexOf(s, substr string) int {
	for i := 0; i+len(substr) <= len(s); i++ {
		if s[i:i+len(substr)] == substr {
			return i
		}
	}
	return -1
}

// -- Loop mechanics (mirrors dashboard's #756 test suite) ------------------------

func TestAsk_ToolCallCapForcesAnswer(t *testing.T) {
	f := newFixture()
	turns := make([]*dashboard.ToolTurn, 0, askMaxToolCalls+1)
	for i := 0; i < askMaxToolCalls+1; i++ {
		turns = append(turns, toolCall(fmt.Sprintf("t%d", i), toolListCatalog, `{}`))
	}
	turns = append(turns, &dashboard.ToolTurn{Text: "answering with what I have"})
	llm := &scriptLLM{turns: turns}
	res := mustAsk(t, f.service(llm, false), "dig deep")
	require.Equal(t, "answering with what I have", res.Answer)
	require.Len(t, res.Evidence, askMaxToolCalls)
	last := llm.convs[len(llm.convs)-1]
	tr := last[len(last)-1].ToolResults[0]
	require.True(t, tr.IsError)
	require.Contains(t, tr.Content, "budget exhausted")
}

func TestAsk_ModelNeverAnswersIsAnError(t *testing.T) {
	f := newFixture()
	llm := &scriptLLM{turns: []*dashboard.ToolTurn{toolCall("t", toolListCatalog, `{}`)}} // repeats forever
	_, err := f.service(llm, false).Ask(ctxWithMerchant(), "loop forever")
	var noAnswer *NoAnswerError
	require.ErrorAs(t, err, &noAnswer)
	require.Equal(t, askMaxToolCalls, noAnswer.ToolCalls)
}

func TestAsk_UnknownToolNameIsRejectedWithoutExecuting(t *testing.T) {
	f := newFixture()
	llm := &scriptLLM{turns: []*dashboard.ToolTurn{
		{ToolCalls: []dashboard.ToolCall{{ID: "x", Name: "delete_everything", Input: json.RawMessage(`{}`)}}},
		{Text: "ok"},
	}}
	res := mustAsk(t, f.service(llm, false), "hi")
	require.Empty(t, res.Evidence)
	tr := llm.convs[1][len(llm.convs[1])-1].ToolResults[0]
	require.True(t, tr.IsError)
	require.Contains(t, tr.Content, "unknown tool")
}

func TestAsk_NotConfigured(t *testing.T) {
	_, err := NewService(Deps{}).Ask(ctxWithMerchant(), "anything")
	require.ErrorIs(t, err, ErrNotConfigured)

	svc := NewService(Deps{LLM: &scriptLLM{turns: []*dashboard.ToolTurn{{Text: "x"}}}})
	require.False(t, svc.Configured(), "LLM alone, without the consent flag, must not arm Q&A")
	_, err = svc.Ask(ctxWithMerchant(), "anything")
	require.ErrorIs(t, err, ErrNotConfigured)
}

type denyLimiter struct{ retry time.Duration }

func (d denyLimiter) AllowAsk(context.Context, string) (bool, time.Duration, error) {
	return false, d.retry, nil
}

func TestAsk_RateLimited(t *testing.T) {
	f := newFixture()
	svc := NewService(Deps{
		Products: f.products, Prices: f.prices, Subs: f.subs, Reprices: f.reprices,
		LLM: &scriptLLM{turns: []*dashboard.ToolTurn{{Text: "never reached"}}}, Enabled: true,
		Limiter: denyLimiter{retry: 30 * time.Second},
	})
	_, err := svc.Ask(ctxWithMerchant(), "anything")
	var limited *RateLimitedError
	require.ErrorAs(t, err, &limited)
	require.Equal(t, 30*time.Second, limited.RetryAfter)
}

func TestSystemPrompt_ContentFirstBeforeDoctrine(t *testing.T) {
	f := newFixture()
	s := f.service(&scriptLLM{}, false)
	prompt := s.systemPrompt(ctxWithMerchant(), time.Date(2026, 7, 7, 0, 0, 0, 0, time.UTC))
	summaryIdx := indexOf(prompt, "premium-monthly")
	doctrineIdx := indexOf(prompt, "Catalog doctrine")
	require.Greater(t, summaryIdx, 0)
	require.Greater(t, doctrineIdx, 0)
	require.Less(t, summaryIdx, doctrineIdx, "content (live catalog) must precede doctrine per the AXI content-first design")
	require.Contains(t, prompt, "2026-07-07")
}

func TestSystemPrompt_DraftingDoctrineOnlyWhenArmed(t *testing.T) {
	f := newFixture()
	off := f.service(&scriptLLM{}, false).systemPrompt(context.Background(), time.Now())
	require.NotContains(t, off, "Drafting:")
	on := f.service(&scriptLLM{}, true).systemPrompt(context.Background(), time.Now())
	require.Contains(t, on, "Drafting:")
}

// -- Phase 2 drafting tests -------------------------------------------------------

func TestAsk_ToolListExcludesDraftToolsWhenFlagOff(t *testing.T) {
	f := newFixture()
	off := f.service(&scriptLLM{}, false)
	names := toolNames(off.toolDefs())
	require.NotContains(t, names, toolDraftPriceChange)
	require.NotContains(t, names, toolDraftCatalogDiff)

	on := f.service(&scriptLLM{}, true)
	names = toolNames(on.toolDefs())
	require.Contains(t, names, toolDraftPriceChange)
	require.Contains(t, names, toolDraftCatalogDiff)
}

func TestAsk_DraftToolAbsentBehavesAsUnknownWhenFlagOff(t *testing.T) {
	f := newFixture()
	llm := &scriptLLM{turns: []*dashboard.ToolTurn{
		toolCall("t1", toolDraftPriceChange, `{"price_key":"premium-monthly","new_amount":15000000}`),
		{Text: "ok"},
	}}
	res := mustAsk(t, f.service(llm, false), "raise premium to $15")
	require.Empty(t, res.Drafts)
	tr := llm.convs[1][len(llm.convs[1])-1].ToolResults[0]
	require.True(t, tr.IsError)
	require.Contains(t, tr.Content, "unknown tool", "flag-off must look like the tool is ABSENT, not present-but-erroring")
}

func TestAsk_DraftPriceChange_IncreaseDefaultsToGrandfather(t *testing.T) {
	f := newFixture()
	llm := &scriptLLM{turns: []*dashboard.ToolTurn{
		toolCall("t1", toolDraftPriceChange, `{"price_key":"premium-monthly","new_amount":15000000}`),
		{Text: "drafted"},
	}}
	res := mustAsk(t, f.service(llm, true), "raise premium to $15")
	require.Len(t, res.Drafts, 1)
	d := res.Drafts[0]
	require.Equal(t, "price_change", d.Kind)
	require.NotNil(t, d.PriceChange)
	require.Equal(t, "increase", d.PriceChange.Direction)
	require.Equal(t, "grandfather", d.PriceChange.MigrationMode, "increase must default to grandfather")
	require.Equal(t, int64(15_000_000), d.PriceChange.NewAmount)
	require.Equal(t, int64(12_000_000), d.PriceChange.CurrentAmount)
	require.Equal(t, 5, d.PriceChange.AffectedCount)
	require.Equal(t, "copilot", d.PriceChange.DraftedBy)
	require.NotEmpty(t, d.PriceChange.DraftID)
	require.Nil(t, d.PriceChange.Reprice, "grandfather mode drafts no reprice call")
	require.Equal(t, "premium-monthly", d.PriceChange.CreatePrice.Key)
	require.Equal(t, f.premiumProduct.ID.String(), d.PriceChange.CreatePrice.ProductID)
	require.Contains(t, d.PriceChange.CreatePrice.Providers, "stripe", "the version bump must re-attach the current price's rails")
}

func TestAsk_DraftPriceChange_DecreaseDefaultsToMigrateNow(t *testing.T) {
	f := newFixture()
	llm := &scriptLLM{turns: []*dashboard.ToolTurn{
		toolCall("t1", toolDraftPriceChange, `{"price_key":"premium-monthly","new_amount":8000000}`),
		{Text: "drafted"},
	}}
	res := mustAsk(t, f.service(llm, true), "drop premium to $8")
	d := res.Drafts[0].PriceChange
	require.Equal(t, "decrease", d.Direction)
	require.Equal(t, "migrate", d.MigrationMode, "decrease must default to migrate-now")
	require.NotNil(t, d.Reprice)
	require.Equal(t, "premium-monthly", d.Reprice.PriceKey)
	require.False(t, d.Reprice.EffectiveAt.After(time.Date(2026, 7, 7, 0, 0, 0, 1, time.UTC)), "decrease effective date defaults to now, not 30 days out")
}

func TestAsk_DraftPriceChange_ExplicitMigrationModeAndDate(t *testing.T) {
	f := newFixture()
	llm := &scriptLLM{turns: []*dashboard.ToolTurn{
		toolCall("t1", toolDraftPriceChange, `{"price_key":"premium-monthly","new_amount":15000000,"migration_mode":"migrate","effective_date":"2026-09-01"}`),
		{Text: "drafted"},
	}}
	res := mustAsk(t, f.service(llm, true), "raise premium to $15, migrate everyone Sep 1")
	d := res.Drafts[0].PriceChange
	require.Equal(t, "migrate", d.MigrationMode)
	require.NotNil(t, d.Reprice)
	require.Equal(t, 2026, d.Reprice.EffectiveAt.Year())
	require.Equal(t, time.September, d.Reprice.EffectiveAt.Month())
	require.Equal(t, 1, d.Reprice.EffectiveAt.Day())
	require.Contains(t, d.ReviewText, "Sep 1, 2026")
}

func TestAsk_DraftPriceChange_CrossProductRefusalExplainsWorkaround(t *testing.T) {
	f := newFixture()
	llm := &scriptLLM{turns: []*dashboard.ToolTurn{
		toolCall("t1", toolDraftPriceChange, `{"price_key":"premium-monthly","new_amount":1,"migrate_to_price_key":"basic-monthly"}`),
		{Text: "refused, explained the workaround"},
	}}
	res := mustAsk(t, f.service(llm, true), "migrate everyone from premium onto basic")
	require.Len(t, res.Drafts, 1)
	d := res.Drafts[0]
	require.Equal(t, "refused", d.Kind)
	require.NotNil(t, d.Refusal)
	require.Equal(t, "cross_product", d.Refusal.Code)
	require.Contains(t, d.Refusal.Workaround, "archive")
	require.Contains(t, d.Refusal.Workaround, "grandfather")
	// The tool result content (what the model saw) must ALSO carry the
	// refusal + workaround + an explicit "no draft was created" statement —
	// this must be a well-formed, non-error tool result (a valid domain
	// answer), not a validation error to retry.
	tr := llm.convs[1][len(llm.convs[1])-1].ToolResults[0]
	require.False(t, tr.IsError)
	require.Contains(t, tr.Content, "cross_product")
	require.Contains(t, tr.Content, "no draft was created")
}

func TestAsk_DraftPriceChange_CrossCurrencyRefusal(t *testing.T) {
	f := newFixture()
	// Add a same-product, different-currency price to isolate the currency
	// check from the product check.
	eurTwin := &models.Price{ID: uuid.New(), ProductID: f.premiumProduct.ID, Key: "premium-monthly-eur",
		Amount: 11_000_000, Currency: "eur"}
	f.prices.current["premium-monthly-eur"] = eurTwin
	f.prices.byID[eurTwin.ID] = eurTwin

	llm := &scriptLLM{turns: []*dashboard.ToolTurn{
		toolCall("t1", toolDraftPriceChange, `{"price_key":"premium-monthly","new_amount":1,"migrate_to_price_key":"premium-monthly-eur"}`),
		{Text: "refused"},
	}}
	res := mustAsk(t, f.service(llm, true), "move premium USD subscribers onto the EUR price")
	d := res.Drafts[0]
	require.Equal(t, "refused", d.Kind)
	require.Equal(t, "cross_currency", d.Refusal.Code)
}

func TestAsk_DraftCatalogDiff_NewTierOnExistingProduct(t *testing.T) {
	f := newFixture()
	llm := &scriptLLM{turns: []*dashboard.ToolTurn{
		toolCall("t1", toolDraftCatalogDiff, `{"product_key":"premium","new_price_key":"premium-with-ads","unit_amount":6000000}`),
		{Text: "drafted a with-ads tier"},
	}}
	res := mustAsk(t, f.service(llm, true), "add a with-ads tier at $6 to premium")
	require.Len(t, res.Drafts, 1)
	d := res.Drafts[0]
	require.Equal(t, "catalog_diff", d.Kind)
	require.NotNil(t, d.CatalogDiff)
	require.Equal(t, "premium-with-ads", d.CatalogDiff.CreatePrice.Key)
	require.Equal(t, int64(6_000_000), d.CatalogDiff.CreatePrice.UnitAmount)
	require.Equal(t, "usd", d.CatalogDiff.CreatePrice.Currency, "currency inferred from the product's existing price")
	require.Equal(t, f.premiumProduct.ID.String(), d.CatalogDiff.CreatePrice.ProductID)
}

func TestAsk_DraftCatalogDiff_KeyCollisionRefused(t *testing.T) {
	f := newFixture()
	llm := &scriptLLM{turns: []*dashboard.ToolTurn{
		toolCall("bad", toolDraftCatalogDiff, `{"product_key":"premium","new_price_key":"premium-monthly","unit_amount":6000000}`),
		toolCall("good", toolDraftCatalogDiff, `{"product_key":"premium","new_price_key":"premium-with-ads","unit_amount":6000000}`),
		{Text: "fixed"},
	}}
	res := mustAsk(t, f.service(llm, true), "add a tier using the same key by mistake")
	require.Len(t, res.Drafts, 1, "the colliding attempt must not produce a draft")
	second := llm.convs[1]
	tr := second[len(second)-1].ToolResults[0]
	require.True(t, tr.IsError)
	require.Contains(t, tr.Content, "already in use")
}

func TestMemoryAskLimiter_WindowsAndIsolation(t *testing.T) {
	now := time.Now()
	l := NewAskLimiter(nil, func() time.Time { return now })
	ctx := context.Background()
	for i := 0; i < askRatePerMinute; i++ {
		ok, _, err := l.AllowAsk(ctx, "merchant-a")
		require.NoError(t, err)
		require.Truef(t, ok, "call %d within budget must pass", i)
	}
	ok, retry, err := l.AllowAsk(ctx, "merchant-a")
	require.NoError(t, err)
	require.False(t, ok)
	require.Greater(t, retry, time.Duration(0))
	ok, _, _ = l.AllowAsk(ctx, "merchant-b")
	require.True(t, ok, "a different merchant is unaffected")
}
