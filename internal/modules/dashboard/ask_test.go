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

// toolScriptLLM plays ToolTurns in order (last repeats) and records every
// conversation + system prompt.
type toolScriptLLM struct {
	turns   []*ToolTurn
	convs   [][]ToolMessage
	systems []string
}

func (f *toolScriptLLM) Complete(context.Context, string, []LLMMessage) (string, error) {
	return "", errors.New("toolScriptLLM: text completion not scripted")
}

func (f *toolScriptLLM) CompleteTools(_ context.Context, system string, _ []ToolDef, msgs []ToolMessage, _ int) (*ToolTurn, error) {
	f.systems = append(f.systems, system)
	f.convs = append(f.convs, append([]ToolMessage(nil), msgs...))
	i := len(f.convs) - 1
	if i >= len(f.turns) {
		i = len(f.turns) - 1
	}
	return f.turns[i], nil
}

// fakeExecutor returns canned results in order (last repeats).
type fakeExecutor struct {
	results []*metrics.Result
	plans   []*metrics.Plan
}

func (f *fakeExecutor) Execute(_ context.Context, plan *metrics.Plan) (*metrics.Result, error) {
	f.plans = append(f.plans, plan)
	i := len(f.plans) - 1
	if i >= len(f.results) {
		i = len(f.results) - 1
	}
	return f.results[i], nil
}

func askService(llm LLM, exec MetricsExecutor) *Service {
	return NewService(Deps{Metrics: exec, LLM: llm, AskEnabled: true})
}

func toolCall(id, query string) *ToolTurn {
	return &ToolTurn{ToolCalls: []ToolCall{{ID: id, Name: askToolName, Input: json.RawMessage(query)}}}
}

func cannedResult(measure string, value int64) *metrics.Result {
	return &metrics.Result{
		Columns: []metrics.Column{{Name: measure, Kind: "measure", Unit: "micros"}},
		Rows:    [][]any{{value}},
	}
}

const (
	askQ1 = `{"measures":["net_revenue"],"range":{"last":"7d"}}`
	askQ2 = `{"measures":["cancellations"],"by":["time"],"grain":"day","range":{"last":"7d"}}`
)

func TestAsk_MultiToolLoopCollectsEvidenceInOrder(t *testing.T) {
	llm := &toolScriptLLM{turns: []*ToolTurn{
		toolCall("t1", askQ1),
		toolCall("t2", askQ2),
		{Text: "Net revenue was $1.20; cancellations held steady."},
	}}
	exec := &fakeExecutor{results: []*metrics.Result{
		cannedResult("net_revenue", 1_200_000),
		cannedResult("cancellations", 3),
	}}
	res, err := askService(llm, exec).Ask(context.Background(), "why did revenue dip last week?")
	require.NoError(t, err)
	require.Equal(t, "Net revenue was $1.20; cancellations held steady.", res.Answer)

	// Evidence: every executed call, in order, verbatim.
	require.Len(t, res.Evidence, 2)
	require.Equal(t, []string{"net_revenue"}, res.Evidence[0].Query.Measures)
	require.Equal(t, [][]any{{int64(1_200_000)}}, res.Evidence[0].Rows)
	require.Equal(t, []string{"cancellations"}, res.Evidence[1].Query.Measures)
	require.Equal(t, "day", res.Evidence[1].Query.Grain)

	// The loop replayed the assistant tool calls and fed results back.
	require.Len(t, llm.convs, 3)
	second := llm.convs[1]
	require.Len(t, second, 3) // question, assistant tool_use, user tool_result
	require.Equal(t, "assistant", second[1].Role)
	require.Len(t, second[1].ToolCalls, 1)
	require.Len(t, second[2].ToolResults, 1)
	require.Equal(t, "t1", second[2].ToolResults[0].ToolUseID)
	require.False(t, second[2].ToolResults[0].IsError)
	require.Contains(t, second[2].ToolResults[0].Content, "1200000")

	// Evidence marshals to the wire contract: query + flattened result fields.
	wire, err := json.Marshal(res.Evidence[0])
	require.NoError(t, err)
	require.Contains(t, string(wire), `"query"`)
	require.Contains(t, string(wire), `"columns"`)
	require.Contains(t, string(wire), `"rows"`)
}

func TestAsk_ToolCallCapForcesAnswer(t *testing.T) {
	// The model asks 6 times, then answers: the 6th call must be refused with
	// a budget-exhausted error; only 5 execute.
	turns := make([]*ToolTurn, 0, 7)
	for i := 0; i < 6; i++ {
		turns = append(turns, toolCall(fmt.Sprintf("t%d", i), askQ1))
	}
	turns = append(turns, &ToolTurn{Text: "Answering with what I have."})
	llm := &toolScriptLLM{turns: turns}
	exec := &fakeExecutor{results: []*metrics.Result{cannedResult("net_revenue", 1)}}

	res, err := askService(llm, exec).Ask(context.Background(), "dig deep")
	require.NoError(t, err)
	require.Equal(t, "Answering with what I have.", res.Answer)
	require.Len(t, res.Evidence, askMaxToolCalls)
	require.Len(t, exec.plans, askMaxToolCalls)

	last := llm.convs[len(llm.convs)-1]
	tr := last[len(last)-1].ToolResults[0]
	require.True(t, tr.IsError)
	require.Contains(t, tr.Content, "budget exhausted")
}

func TestAsk_ModelNeverAnswersIsAnError(t *testing.T) {
	llm := &toolScriptLLM{turns: []*ToolTurn{toolCall("t", askQ1)}} // repeats forever
	exec := &fakeExecutor{results: []*metrics.Result{cannedResult("net_revenue", 1)}}
	_, err := askService(llm, exec).Ask(context.Background(), "loop forever")
	var noAnswer *AskNoAnswerError
	require.ErrorAs(t, err, &noAnswer)
	require.Equal(t, askMaxToolCalls, noAnswer.ToolCalls)
	require.Len(t, llm.convs, askMaxTurns, "loop must stop at the turn ceiling")
}

func TestAsk_InvalidToolArgsGetCorrectiveErrorsThenCorrectedCall(t *testing.T) {
	invalid := `{"measures":["cancelations"],"by":["time"],"grain":"day","range":{"last":"7d"}}`
	llm := &toolScriptLLM{turns: []*ToolTurn{
		toolCall("bad", invalid),
		toolCall("good", askQ2),
		{Text: "3 cancellations."},
	}}
	exec := &fakeExecutor{results: []*metrics.Result{cannedResult("cancellations", 3)}}
	res, err := askService(llm, exec).Ask(context.Background(), "cancellations per day")
	require.NoError(t, err)

	// The invalid call executed nothing and produced no evidence.
	require.Len(t, res.Evidence, 1)
	require.Equal(t, []string{"cancellations"}, res.Evidence[0].Query.Measures)
	require.Len(t, exec.plans, 1)

	// The compiler's corrective errors (did-you-mean) went back to the model.
	second := llm.convs[1]
	tr := second[len(second)-1].ToolResults[0]
	require.True(t, tr.IsError)
	require.Equal(t, "bad", tr.ToolUseID)
	require.Contains(t, tr.Content, "unknown_measure")
	require.Contains(t, tr.Content, "cancellations", "did-you-mean must ride back")
}

func TestAsk_UnknownToolNameIsRejectedWithoutExecuting(t *testing.T) {
	llm := &toolScriptLLM{turns: []*ToolTurn{
		{ToolCalls: []ToolCall{{ID: "x", Name: "delete_everything", Input: json.RawMessage(`{}`)}}},
		{Text: "ok"},
	}}
	exec := &fakeExecutor{results: []*metrics.Result{cannedResult("net_revenue", 1)}}
	res, err := askService(llm, exec).Ask(context.Background(), "hi")
	require.NoError(t, err)
	require.Empty(t, res.Evidence)
	require.Empty(t, exec.plans)
	tr := llm.convs[1][len(llm.convs[1])-1].ToolResults[0]
	require.True(t, tr.IsError)
	require.Contains(t, tr.Content, "unknown tool")
}

func TestAsk_CannotAnswerPathReturnsProseWithNoEvidence(t *testing.T) {
	llm := &toolScriptLLM{turns: []*ToolTurn{
		{Text: "The metrics schema has no page-view data, so I cannot answer that."},
	}}
	res, err := askService(llm, &fakeExecutor{results: []*metrics.Result{cannedResult("x", 0)}}).
		Ask(context.Background(), "how many page views did I get?")
	require.NoError(t, err)
	require.Empty(t, res.Evidence)
	require.Contains(t, res.Answer, "no page-view data")
}

func TestAsk_NotConfigured(t *testing.T) {
	// No LLM at all.
	_, err := NewService(Deps{}).Ask(context.Background(), "anything")
	require.ErrorIs(t, err, ErrAskNotConfigured)
	// LLM present but no ask consent.
	svc := NewService(Deps{LLM: &toolScriptLLM{turns: []*ToolTurn{{Text: "x"}}}})
	require.True(t, svc.NLConfigured())
	require.False(t, svc.AskConfigured())
	_, err = svc.Ask(context.Background(), "anything")
	require.ErrorIs(t, err, ErrAskNotConfigured)
}

type denyLimiter struct{ retry time.Duration }

func (d denyLimiter) AllowAsk(context.Context, string) (bool, time.Duration, error) {
	return false, d.retry, nil
}

func TestAsk_RateLimited(t *testing.T) {
	svc := NewService(Deps{
		LLM:        &toolScriptLLM{turns: []*ToolTurn{{Text: "never reached"}}},
		AskEnabled: true,
		AskLimiter: denyLimiter{retry: 30 * time.Second},
	})
	ctx := merchant.WithID(context.Background(), merchant.ID(uuid.New()))
	_, err := svc.Ask(ctx, "anything")
	var limited *AskRateLimitedError
	require.ErrorAs(t, err, &limited)
	require.Equal(t, 30*time.Second, limited.RetryAfter)
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
	require.False(t, ok, "minute window must trip")
	require.Greater(t, retry, time.Duration(0))
	// Another merchant is unaffected.
	ok, _, _ = l.AllowAsk(ctx, "merchant-b")
	require.True(t, ok)
	// The window rolls over.
	now = now.Add(2 * time.Minute)
	ok, _, _ = l.AllowAsk(ctx, "merchant-a")
	require.True(t, ok)
}

func TestAskToolPayload_TruncatesOversizedResults(t *testing.T) {
	rows := make([][]any, 0, 20000)
	for i := 0; i < 20000; i++ {
		rows = append(rows, []any{fmt.Sprintf("2026-06-%02dT00:00:00Z", i%28+1), i})
	}
	res := &metrics.Result{
		Columns: []metrics.Column{{Name: "time", Kind: "time"}, {Name: "payment_count", Kind: "measure"}},
		Rows:    rows,
	}
	payload := askToolPayload(res)
	require.LessOrEqual(t, len(payload), askToolResultMaxBytes+1024)
	require.Contains(t, payload, `"truncated":true`)
	require.Contains(t, payload, `"rows_total":20000`)
	require.Len(t, res.Rows, 20000, "the verbatim result (evidence) must stay whole")
}

func TestAskSystemPromptAndToolDef(t *testing.T) {
	p := askSystemPrompt(time.Date(2026, 7, 4, 0, 0, 0, 0, time.UTC))
	require.Contains(t, p, "2026-07-04")
	require.Contains(t, p, "<schema>")
	require.Contains(t, p, "NEVER improvise", "honesty rule must ride in")
	require.Contains(t, p, "say plainly what is missing")
	require.Contains(t, p, "Answer concisely")
	var doc metrics.SchemaDoc
	start := strings.Index(p, "<schema>\n")
	end := strings.Index(p, "\n</schema>")
	require.Greater(t, end, start)
	require.NoError(t, json.Unmarshal([]byte(p[start+len("<schema>\n"):end]), &doc))
	require.NotEmpty(t, doc.Examples)

	def := askToolDef()
	require.Equal(t, askToolName, def.Name)
	require.True(t, json.Valid(def.InputSchema), "tool input schema must be valid JSON")
}
