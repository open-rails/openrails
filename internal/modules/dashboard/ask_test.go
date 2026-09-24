package dashboard

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/modules/metrics"
	"github.com/open-rails/openrails/pkg/merchant"
)

// scriptLLM plays turns (tool loop) or responses (Generate) in order, the last
// repeating, and records every conversation it was sent.
type scriptLLM struct {
	turns     []*ToolTurn
	responses []string
	convs     [][]ToolMessage
	calls     [][]LLMMessage
}

func (f *scriptLLM) Complete(_ context.Context, _ string, msgs []LLMMessage) (string, error) {
	f.calls = append(f.calls, append([]LLMMessage(nil), msgs...))
	if len(f.responses) == 0 {
		return "", errors.New("not scripted")
	}
	return f.responses[min(len(f.calls), len(f.responses))-1], nil
}

func (f *scriptLLM) CompleteTools(_ context.Context, _ string, _ []ToolDef, msgs []ToolMessage, _ int) (*ToolTurn, error) {
	f.convs = append(f.convs, append([]ToolMessage(nil), msgs...))
	return f.turns[min(len(f.convs), len(f.turns))-1], nil
}

// fakeExecutor stands in for the PostgreSQL-backed metrics service.
type fakeExecutor struct {
	value int64
	plans []*metrics.Plan
}

func (f *fakeExecutor) Execute(_ context.Context, plan *metrics.Plan) (*metrics.Result, error) {
	f.plans = append(f.plans, plan)
	return &metrics.Result{
		Columns: []metrics.Column{{Name: plan.Measures[0].Name, Kind: "measure", Unit: "count"}},
		Rows:    [][]any{{f.value}},
	}, nil
}

func toolCall(id, query string) *ToolTurn {
	return &ToolTurn{ToolCalls: []ToolCall{{ID: id, Name: askToolName, Input: json.RawMessage(query)}}}
}

const (
	askQ1 = `{"measures":["net_revenue"],"range":{"last":"7d"}}`
	askQ2 = `{"measures":["cancellations"],"by":["time"],"grain":"day","range":{"last":"7d"}}`
)

func ask(t *testing.T, llm LLM, exec *fakeExecutor) (*AskResult, error) {
	t.Helper()
	return NewService(Deps{Metrics: exec, LLM: llm, AskEnabled: true}).Ask(context.Background(), "q")
}

func lastResult(conv []ToolMessage) ToolResult { return conv[len(conv)-1].ToolResults[0] }

// On-screen numbers come from evidence (every executed query, verbatim, in order), never from prose.
func TestAskCollectsEvidenceAndFeedsResultsBack(t *testing.T) {
	llm := &scriptLLM{turns: []*ToolTurn{toolCall("t1", askQ1), toolCall("t2", askQ2), {Text: " Revenue held. "}}}
	exec := &fakeExecutor{value: 1_200_000}
	res, err := ask(t, llm, exec)
	require.NoError(t, err)
	require.Equal(t, "Revenue held.", res.Answer)
	require.Len(t, res.Evidence, 2)
	require.Equal(t, []string{"net_revenue"}, res.Evidence[0].Query.Measures)
	require.Equal(t, [][]any{{int64(1_200_000)}}, res.Evidence[0].Rows)
	require.Equal(t, "day", res.Evidence[1].Query.Grain)

	second := llm.convs[1]
	require.Len(t, second, 3, "question, assistant tool call, tool result")
	require.Equal(t, "assistant", second[1].Role)
	tr := lastResult(second)
	require.Equal(t, "t1", tr.ToolUseID)
	require.False(t, tr.IsError)
	require.Contains(t, tr.Content, "1200000")

	wire, err := json.Marshal(res.Evidence[0])
	require.NoError(t, err)
	for _, key := range []string{`"query"`, `"columns"`, `"rows"`} {
		require.Contains(t, string(wire), key, "evidence flattens the result beside its query")
	}
}

func TestAskToolRefusals(t *testing.T) {
	t.Run("invalid args return corrective errors and execute nothing", func(t *testing.T) {
		llm := &scriptLLM{turns: []*ToolTurn{toolCall("bad", `{"measures":["cancelations"],"range":{"last":"7d"}}`), toolCall("good", askQ2), {Text: "3."}}}
		exec := &fakeExecutor{value: 3}
		res, err := ask(t, llm, exec)
		require.NoError(t, err)
		require.Len(t, res.Evidence, 1)
		require.Len(t, exec.plans, 1)
		tr := lastResult(llm.convs[1])
		require.True(t, tr.IsError)
		require.Equal(t, "bad", tr.ToolUseID)
		require.Contains(t, tr.Content, "unknown_measure")
		require.Contains(t, tr.Content, `"did_you_mean":"cancellations"`)
	})
	t.Run("unknown tool is refused without executing", func(t *testing.T) {
		llm := &scriptLLM{turns: []*ToolTurn{{ToolCalls: []ToolCall{{ID: "x", Name: "delete_everything", Input: json.RawMessage(`{}`)}}}, {Text: "ok"}}}
		exec := &fakeExecutor{}
		res, err := ask(t, llm, exec)
		require.NoError(t, err)
		require.Empty(t, res.Evidence)
		require.Empty(t, exec.plans)
		require.Contains(t, lastResult(llm.convs[1]).Content, "unknown tool")
	})
	t.Run("call budget forces an answer", func(t *testing.T) {
		var turns []*ToolTurn
		for i := 0; i <= askMaxToolCalls; i++ {
			turns = append(turns, toolCall(fmt.Sprintf("t%d", i), askQ1))
		}
		llm := &scriptLLM{turns: append(turns, &ToolTurn{Text: "Answering with what I have."})}
		exec := &fakeExecutor{value: 1}
		res, err := ask(t, llm, exec)
		require.NoError(t, err)
		require.Len(t, res.Evidence, askMaxToolCalls)
		require.Len(t, exec.plans, askMaxToolCalls)
		tr := lastResult(llm.convs[len(llm.convs)-1])
		require.True(t, tr.IsError)
		require.Contains(t, tr.Content, "budget exhausted")
	})
	t.Run("a model that never answers stops at the turn ceiling", func(t *testing.T) {
		llm := &scriptLLM{turns: []*ToolTurn{toolCall("t", askQ1)}}
		_, err := ask(t, llm, &fakeExecutor{value: 1})
		var noAnswer *AskNoAnswerError
		require.ErrorAs(t, err, &noAnswer)
		require.Equal(t, askMaxToolCalls, noAnswer.ToolCalls)
		require.Len(t, llm.convs, askMaxTurns)
	})
}

// Ask shows the model merchant data, so it needs its own consent beyond an LLM key, and a spend budget.
func TestAskRequiresConsentAndBudget(t *testing.T) {
	_, err := NewService(Deps{}).Ask(context.Background(), "q")
	require.ErrorIs(t, err, ErrAskNotConfigured)

	llm := &scriptLLM{turns: []*ToolTurn{{Text: "never reached"}}}
	svc := NewService(Deps{LLM: llm})
	require.True(t, svc.NLConfigured())
	require.False(t, svc.AskConfigured())
	_, err = svc.Ask(context.Background(), "q")
	require.ErrorIs(t, err, ErrAskNotConfigured)

	ctx := merchant.WithID(context.Background(), merchant.ID(uuid.New()))
	_, err = NewService(Deps{LLM: llm, AskEnabled: true, AskLimiter: denyLimiter{retry: 30 * time.Second}}).Ask(ctx, "q")
	var limited *AskRateLimitedError
	require.ErrorAs(t, err, &limited)
	require.Equal(t, 30*time.Second, limited.RetryAfter)
	require.Empty(t, llm.convs, "refused before any LLM spend")
}

type denyLimiter struct{ retry time.Duration }

func (d denyLimiter) AllowAsk(context.Context, string) (bool, time.Duration, error) {
	return false, d.retry, nil
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

// The model is told it sees a truncated result; evidence keeps it whole.
func TestAskToolPayloadTruncatesOversizedResults(t *testing.T) {
	rows := make([][]any, 20000)
	for i := range rows {
		rows[i] = []any{fmt.Sprintf("2026-06-%02dT00:00:00Z", i%28+1), i}
	}
	res := &metrics.Result{Columns: []metrics.Column{{Name: "time", Kind: "time"}, {Name: "payment_count", Kind: "measure"}}, Rows: rows}
	payload := askToolPayload(res)
	require.LessOrEqual(t, len(payload), askToolResultMaxBytes+1024)
	require.Contains(t, payload, `"truncated":true`)
	require.Contains(t, payload, `"rows_total":20000`)
	require.Len(t, res.Rows, 20000)
}

// Both prompts embed the metrics schema as parseable JSON; the tool schema is valid JSON.
func TestPromptsEmbedValidSchema(t *testing.T) {
	now := time.Date(2026, 7, 4, 0, 0, 0, 0, time.UTC)
	for _, p := range []string{askSystemPrompt(now), generateSystemPrompt(now)} {
		start, end := strings.Index(p, "<schema>\n"), strings.Index(p, "\n</schema>")
		require.Greater(t, end, start)
		require.Greater(t, start, -1)
		var doc metrics.SchemaDoc
		require.NoError(t, json.Unmarshal([]byte(p[start+len("<schema>\n"):end]), &doc))
		require.NotEmpty(t, doc.Examples)
		require.Contains(t, p, "2026-07-04")
	}
	require.True(t, json.Valid(askToolDef().InputSchema))
}
