package dashboard

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/modules/metrics"
)

// Every default-template widget must pass the same compiler validation PUT
// enforces — the seed can never be an invalid dashboard.
func TestDefaultWidgets_AllValidate(t *testing.T) {
	for _, hasUsage := range []bool{false, true} {
		widgets := DefaultWidgets(hasUsage)
		require.Nil(t, ValidateWidgets(widgets), "default template (usage=%v) must validate", hasUsage)
		if hasUsage {
			require.Greater(t, len(widgets), len(DefaultWidgets(false)), "usage activity must add widgets")
		}
	}
}

func TestValidateWidgets_WidgetIndexedErrors(t *testing.T) {
	widgets := []Widget{
		{ID: "ok", Title: "OK", Viz: "stat",
			Query: metrics.Query{Measures: []string{"mrr"}, Range: &metrics.QueryRange{Last: "30d"}},
			Grid:  Grid{W: 3, H: 2}},
		{ID: "bad", Title: "Bad", Viz: "sparkles",
			Query: metrics.Query{Measures: []string{"revnue"}, Range: &metrics.QueryRange{Last: "7d"}},
			Grid:  Grid{W: 3, H: 2}},
	}
	verr := ValidateWidgets(widgets)
	require.NotNil(t, verr)
	var params []string
	for _, fe := range verr.Errors {
		params = append(params, fe.Param)
	}
	require.Contains(t, params, "widgets[1].viz")
	require.Contains(t, params, "widgets[1].query.measures[0]")
	for _, p := range params {
		require.True(t, strings.HasPrefix(p, "widgets[1]."), "only widget 1 should error, got %q", p)
	}
	// The compiler's corrective context (did-you-mean / valid list) survives.
	for _, fe := range verr.Errors {
		if fe.Param == "widgets[1].query.measures[0]" {
			require.NotEmpty(t, fe.Valid, "corrective valid list must ride through")
		}
	}
}

func TestDecodePut_UnknownKeysFailLoudly(t *testing.T) {
	_, verr := DecodePut(strings.NewReader(`{"widgets":[{"id":"a","title":"A","viz":"stat","qeury":{},"grid":{"x":0,"y":0,"w":3,"h":2}}]}`))
	require.NotNil(t, verr)
	require.Equal(t, "unknown_body_key", verr.Errors[0].Code)
}

// fakeLLM scripts deterministic responses and records the conversation.
type fakeLLM struct {
	responses []string
	calls     [][]LLMMessage
}

func (f *fakeLLM) Complete(_ context.Context, _ string, msgs []LLMMessage) (string, error) {
	f.calls = append(f.calls, append([]LLMMessage(nil), msgs...))
	if len(f.calls) > len(f.responses) {
		return f.responses[len(f.responses)-1], nil
	}
	return f.responses[len(f.calls)-1], nil
}

const goldenResponse = `{"query":{"measures":["cancellations"],"by":["time"],"grain":"day","range":{"last":"7d"}},"title":"Cancellations per day","viz":"line"}`

func TestGenerate_HappyPathGolden(t *testing.T) {
	llm := &fakeLLM{responses: []string{goldenResponse}}
	svc := NewService(nil, llm, nil)
	res, err := svc.Generate(context.Background(), "count of users who cancelled per day, for the past 7 days")
	require.NoError(t, err)
	require.Equal(t, []string{"cancellations"}, res.Query.Measures)
	require.Equal(t, []string{"time"}, res.Query.By)
	require.Equal(t, "day", res.Query.Grain)
	require.Equal(t, "7d", res.Query.Range.Last)
	require.Equal(t, "line", res.Viz)
	require.NotEmpty(t, res.Title)
	require.Len(t, llm.calls, 1)
}

func TestGenerate_RetryLoopFeedsErrorsBack(t *testing.T) {
	invalid := `{"query":{"measures":["cancelations"],"by":["time"],"grain":"day","range":{"last":"7d"}},"title":"Cancellations","viz":"line"}`
	llm := &fakeLLM{responses: []string{invalid, goldenResponse}}
	svc := NewService(nil, llm, nil)
	res, err := svc.Generate(context.Background(), "cancelled per day past week")
	require.NoError(t, err)
	require.Equal(t, []string{"cancellations"}, res.Query.Measures)
	require.Len(t, llm.calls, 2)

	// The second call's conversation carries the first response verbatim and
	// the compiler's corrective errors (did-you-mean) fed back verbatim.
	second := llm.calls[1]
	require.Len(t, second, 3) // prompt, assistant candidate, error feedback
	require.Equal(t, "assistant", second[1].Role)
	require.Equal(t, invalid, second[1].Content)
	require.Contains(t, second[2].Content, "unknown_measure")
	require.Contains(t, second[2].Content, "cancellations", "did-you-mean must ride into the feedback")
	require.Contains(t, second[2].Content, "query.measures[0]")
}

func TestGenerate_ExhaustedRetriesReturnsErrors(t *testing.T) {
	invalid := `{"query":{"measures":["nope"],"range":{"last":"7d"}},"title":"x","viz":"stat"}`
	llm := &fakeLLM{responses: []string{invalid, invalid, invalid}}
	svc := NewService(nil, llm, nil)
	_, err := svc.Generate(context.Background(), "whatever")
	var gerr *GenerateInvalidError
	require.ErrorAs(t, err, &gerr)
	require.NotEmpty(t, gerr.Errors)
	require.Len(t, llm.calls, 3, "initial + 2 retries, never more")
}

func TestGenerate_Unconfigured(t *testing.T) {
	svc := NewService(nil, nil, nil)
	_, err := svc.Generate(context.Background(), "anything")
	require.ErrorIs(t, err, ErrLLMNotConfigured)
	require.False(t, svc.NLConfigured())
}

func TestGenerate_StripsCodeFences(t *testing.T) {
	llm := &fakeLLM{responses: []string{"```json\n" + goldenResponse + "\n```"}}
	svc := NewService(nil, llm, nil)
	res, err := svc.Generate(context.Background(), "cancellations per day")
	require.NoError(t, err)
	require.Equal(t, []string{"cancellations"}, res.Query.Measures)
}

// The system prompt is the LLM's whole world: it must carry the schema
// document and the output contract.
func TestGenerateSystemPrompt_CarriesSchema(t *testing.T) {
	svc := NewService(nil, &fakeLLM{responses: []string{goldenResponse}}, nil)
	p := generateSystemPrompt(svc.clock.Now())
	require.Contains(t, p, `"measures"`)
	require.Contains(t, p, "cancellations")   // registry rode in
	require.Contains(t, p, "previous_period") // examples rode in
	require.Contains(t, p, `"viz"`)
	var doc metrics.SchemaDoc
	start := strings.Index(p, "<schema>\n")
	end := strings.Index(p, "\n</schema>")
	require.Greater(t, end, start)
	require.NoError(t, json.Unmarshal([]byte(p[start+len("<schema>\n"):end]), &doc), "embedded schema must be valid JSON")
	require.NotEmpty(t, doc.Examples)
}
