package copilot

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/modules/dashboard"
	"github.com/open-rails/openrails/pkg/merchant"
)

// Catalog reads and proposals run against PostgreSQL elsewhere; these are the
// consent, budget and tool-gating guards that sit before any reader.
type scriptLLM struct {
	turns []*dashboard.ToolTurn
	convs [][]dashboard.ToolMessage
}

func (f *scriptLLM) Complete(context.Context, string, []dashboard.LLMMessage) (string, error) {
	return "", errors.New("not scripted")
}

func (f *scriptLLM) CompleteTools(_ context.Context, _ string, _ []dashboard.ToolDef, msgs []dashboard.ToolMessage, _ int) (*dashboard.ToolTurn, error) {
	f.convs = append(f.convs, append([]dashboard.ToolMessage(nil), msgs...))
	return f.turns[min(len(f.convs), len(f.turns))-1], nil
}

type denyLimiter struct{ retry time.Duration }

func (d denyLimiter) AllowAsk(context.Context, string) (bool, time.Duration, error) {
	return false, d.retry, nil
}

func merchantCtx() context.Context {
	return merchant.WithID(context.Background(), merchant.ID(uuid.New()))
}

func TestAskRequiresConsentAndBudget(t *testing.T) {
	llm := &scriptLLM{turns: []*dashboard.ToolTurn{{Text: "never reached"}}}
	for name, d := range map[string]Deps{"no LLM": {Enabled: true}, "LLM without consent flag": {LLM: llm}} {
		_, err := NewService(d).Ask(merchantCtx(), "q")
		require.ErrorIs(t, err, ErrNotConfigured, name)
	}
	_, err := NewService(Deps{LLM: llm, Enabled: true, Limiter: denyLimiter{retry: 30 * time.Second}}).Ask(merchantCtx(), "q")
	var limited *RateLimitedError
	require.ErrorAs(t, err, &limited)
	require.Equal(t, 30*time.Second, limited.RetryAfter)
	require.Empty(t, llm.convs, "refused before any LLM spend")
}

// Drafting tools are absent, not present-but-erroring, unless both consent flags are set.
func TestDraftToolsOnlyWhenArmed(t *testing.T) {
	for _, tc := range []struct {
		enabled, drafting, want bool
	}{{true, false, false}, {false, true, false}, {true, true, true}} {
		svc := NewService(Deps{LLM: &scriptLLM{}, Enabled: tc.enabled, Drafting: tc.drafting})
		names := toolNames(svc.toolDefs())
		for _, draft := range []string{toolDraftPriceChange, toolDraftCatalogDiff} {
			if tc.want {
				require.Contains(t, names, draft)
			} else {
				require.NotContains(t, names, draft)
			}
		}
	}
}

func TestAskToolLoopRefusals(t *testing.T) {
	for _, name := range []string{"delete_everything", toolDraftPriceChange} {
		llm := &scriptLLM{turns: []*dashboard.ToolTurn{
			{ToolCalls: []dashboard.ToolCall{{ID: "x", Name: name, Input: json.RawMessage(`{}`)}}},
			{Text: "ok"},
		}}
		res, err := NewService(Deps{LLM: llm, Enabled: true}).Ask(merchantCtx(), "inspect")
		require.NoError(t, err)
		require.Empty(t, res.Evidence)
		require.Empty(t, res.Drafts)
		refusal := llm.convs[1][2].ToolResults[0]
		require.True(t, refusal.IsError)
		require.Contains(t, refusal.Content, "unknown tool", name)
	}

	// A model that never stops calling tools is cut off at the budget and turn ceiling.
	llm := &scriptLLM{turns: []*dashboard.ToolTurn{{ToolCalls: []dashboard.ToolCall{{ID: "x", Name: toolListCatalog, Input: json.RawMessage(`{}`)}}}}}
	_, err := NewService(Deps{LLM: llm, Enabled: true}).Ask(merchantCtx(), "inspect")
	var noAnswer *NoAnswerError
	require.ErrorAs(t, err, &noAnswer)
	require.Equal(t, askMaxToolCalls, noAnswer.ToolCalls)
	require.Len(t, llm.convs, askMaxTurns)
}

func TestMemoryAskLimiterWindows(t *testing.T) {
	now := time.Date(2026, 7, 4, 0, 0, 0, 0, time.UTC)
	l := NewAskLimiter(nil, func() time.Time { return now })
	ctx := context.Background()
	for minute := 0; minute < askRatePerDay/askRatePerMinute; minute++ {
		for i := 0; i < askRatePerMinute; i++ {
			ok, _, err := l.AllowAsk(ctx, "a")
			require.NoError(t, err)
			require.True(t, ok, "minute %d call %d", minute, i)
		}
		ok, retry, _ := l.AllowAsk(ctx, "a")
		require.False(t, ok, "minute window trips")
		require.Greater(t, retry, time.Duration(0))
		now = now.Add(time.Minute)
	}
	ok, retry, _ := l.AllowAsk(ctx, "a")
	require.False(t, ok, "daily window trips")
	require.Greater(t, retry, time.Hour)
	ok, _, _ = l.AllowAsk(ctx, "b")
	require.True(t, ok, "merchants are isolated")
}
