package copilot

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/modules/dashboard"
	"github.com/open-rails/openrails/pkg/merchant"
	"github.com/stretchr/testify/require"
)

// Catalog reads and proposals are covered by the mounted PostgreSQL workflow.
// These pre-reader consent/limit guards require no imitation catalog database.
type scriptLLM struct {
	turns []*dashboard.ToolTurn
	convs [][]dashboard.ToolMessage
}

func (f *scriptLLM) Complete(context.Context, string, []dashboard.LLMMessage) (string, error) {
	return "", errors.New("scriptLLM: text completion not scripted")
}

func (f *scriptLLM) CompleteTools(_ context.Context, _ string, _ []dashboard.ToolDef, msgs []dashboard.ToolMessage, _ int) (*dashboard.ToolTurn, error) {
	f.convs = append(f.convs, append([]dashboard.ToolMessage(nil), msgs...))
	i := len(f.convs) - 1
	if i >= len(f.turns) {
		i = len(f.turns) - 1
	}
	return f.turns[i], nil
}

func ctxWithMerchant() context.Context {
	return merchant.WithID(context.Background(), merchant.ID(uuid.New()))
}

type denyLimiter struct{ retry time.Duration }

func (d denyLimiter) AllowAsk(context.Context, string) (bool, time.Duration, error) {
	return false, d.retry, nil
}

func TestAsk_NotConfigured(t *testing.T) {
	_, err := NewService(Deps{}).Ask(ctxWithMerchant(), "anything")
	require.ErrorIs(t, err, ErrNotConfigured)

	svc := NewService(Deps{LLM: &scriptLLM{turns: []*dashboard.ToolTurn{{Text: "x"}}}})
	require.False(t, svc.Configured(), "LLM alone, without the consent flag, must not arm Q&A")
	_, err = svc.Ask(ctxWithMerchant(), "anything")
	require.ErrorIs(t, err, ErrNotConfigured)
}

func TestAsk_RateLimited(t *testing.T) {
	svc := NewService(Deps{
		LLM: &scriptLLM{turns: []*dashboard.ToolTurn{{Text: "never reached"}}}, Enabled: true,
		Limiter: denyLimiter{retry: 30 * time.Second},
	})
	_, err := svc.Ask(ctxWithMerchant(), "anything")
	var limited *RateLimitedError
	require.ErrorAs(t, err, &limited)
	require.Equal(t, 30*time.Second, limited.RetryAfter)
}

func TestSystemPrompt_DraftingDoctrineOnlyWhenArmed(t *testing.T) {
	off := NewService(Deps{LLM: &scriptLLM{}, Enabled: true}).systemPrompt(context.Background(), time.Now())
	require.NotContains(t, off, "Drafting:")
	on := NewService(Deps{LLM: &scriptLLM{}, Enabled: true, Drafting: true}).systemPrompt(context.Background(), time.Now())
	require.Contains(t, on, "Drafting:")
}

func TestAsk_ToolListExcludesDraftToolsWhenFlagOff(t *testing.T) {
	off := NewService(Deps{LLM: &scriptLLM{}, Enabled: true})
	names := toolNames(off.toolDefs())
	require.NotContains(t, names, toolDraftPriceChange)
	require.NotContains(t, names, toolDraftCatalogDiff)

	on := NewService(Deps{LLM: &scriptLLM{}, Enabled: true, Drafting: true})
	names = toolNames(on.toolDefs())
	require.Contains(t, names, toolDraftPriceChange)
	require.Contains(t, names, toolDraftCatalogDiff)
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

func TestAsk_ToolLoopRefusals(t *testing.T) {
	for _, name := range []string{"delete_everything", toolListCatalog} {
		t.Run(name, func(t *testing.T) {
			llm := &scriptLLM{turns: []*dashboard.ToolTurn{{ToolCalls: []dashboard.ToolCall{{ID: "x", Name: name, Input: json.RawMessage(`{}`)}}}}}
			if name == "delete_everything" {
				llm.turns = append(llm.turns, &dashboard.ToolTurn{Text: "ok"})
			}
			result, err := NewService(Deps{LLM: llm, Enabled: true}).Ask(ctxWithMerchant(), "inspect")
			if name == "delete_everything" {
				require.NoError(t, err)
				require.Empty(t, result.Evidence)
				refusal := llm.convs[1][2].ToolResults[0]
				require.True(t, refusal.IsError)
				require.Contains(t, refusal.Content, "unknown tool")
			} else {
				var noAnswer *NoAnswerError
				require.ErrorAs(t, err, &noAnswer)
				require.Equal(t, askMaxToolCalls, noAnswer.ToolCalls)
				require.Len(t, llm.convs, askMaxTurns)
			}
		})
	}
}
