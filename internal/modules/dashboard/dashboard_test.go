package dashboard

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/modules/metrics"
)

// The seeded template passes the same validation PUT enforces.
func TestDefaultWidgetsValidate(t *testing.T) {
	require.Nil(t, ValidateWidgets(DefaultWidgets(false)))
	require.Nil(t, ValidateWidgets(DefaultWidgets(true)))
	require.Greater(t, len(DefaultWidgets(true)), len(DefaultWidgets(false)), "usage activity adds widgets")
}

// All errors come back at once, widget-indexed, with the compiler's corrective context intact.
func TestValidateWidgetsIndexesEveryError(t *testing.T) {
	ok := Widget{ID: "a", Title: "OK", Viz: "stat", Query: metrics.Query{Measures: []string{"mrr"}, Range: &metrics.QueryRange{Last: "30d"}}, Grid: Grid{W: 3, H: 2}}
	bad := Widget{ID: "a", Viz: "sparkles", Query: metrics.Query{Measures: []string{"revnue"}, Range: &metrics.QueryRange{Last: "7d"}}, Grid: Grid{X: -1, W: 3, H: 2}}
	verr := ValidateWidgets([]Widget{ok, bad})
	require.NotNil(t, verr)
	byParam := map[string]metrics.FieldError{}
	for _, fe := range verr.Errors {
		require.True(t, strings.HasPrefix(fe.Param, "widgets[1]."), "only widget 1 errs, got %q", fe.Param)
		byParam[fe.Param] = fe
	}
	require.Equal(t, "duplicate_widget_id", byParam["widgets[1].id"].Code)
	require.Equal(t, "missing_widget_title", byParam["widgets[1].title"].Code)
	require.Equal(t, "invalid_viz", byParam["widgets[1].viz"].Code)
	require.Equal(t, "invalid_grid", byParam["widgets[1].grid"].Code)
	require.NotEmpty(t, byParam["widgets[1].query.measures[0]"].Valid)

	require.Equal(t, "too_many_widgets", ValidateWidgets(make([]Widget, MaxWidgets+1)).Errors[0].Code)
}

func TestDecodePutIsStrict(t *testing.T) {
	_, verr := DecodePut(strings.NewReader(`{"widgets":[{"id":"a","title":"A","viz":"stat","qeury":{},"grid":{"x":0,"y":0,"w":3,"h":2}}]}`))
	require.Equal(t, "unknown_body_key", verr.Errors[0].Code)
	_, verr = DecodePut(strings.NewReader(`{"widgets":[]} {"widgets":[]}`))
	require.Equal(t, "invalid_body", verr.Errors[0].Code, "exactly one JSON object")
}

const goldenWidget = `{"query":{"measures":["cancellations"],"by":["time"],"grain":"day","range":{"last":"7d"}},"title":"Cancellations per day","viz":"line"}`

func generate(t *testing.T, base *metrics.Query, responses ...string) (*GenerateResult, *scriptLLM, error) {
	t.Helper()
	llm := &scriptLLM{responses: responses}
	res, err := NewService(Deps{LLM: llm}).Generate(context.Background(), "cancelled per day", base)
	return res, llm, err
}

// Generate only returns compiler-validated widgets.
func TestGenerate(t *testing.T) {
	_, err := NewService(Deps{}).Generate(context.Background(), "x", nil)
	require.ErrorIs(t, err, ErrLLMNotConfigured)

	for _, raw := range []string{goldenWidget, "```json\n" + goldenWidget + "\n```"} {
		res, llm, err := generate(t, nil, raw)
		require.NoError(t, err)
		require.Equal(t, metrics.Query{Measures: []string{"cancellations"}, By: []string{"time"}, Grain: "day", Range: &metrics.QueryRange{Last: "7d"}}, res.Query)
		require.Equal(t, "line", res.Viz)
		require.Len(t, llm.calls, 1)
	}

	invalid := `{"query":{"measures":["cancelations"],"range":{"last":"7d"}},"title":"C","viz":"line"}`
	res, llm, err := generate(t, nil, invalid, goldenWidget)
	require.NoError(t, err)
	require.Equal(t, []string{"cancellations"}, res.Query.Measures)
	second := llm.calls[1]
	require.Len(t, second, 3, "prompt, candidate verbatim, corrective feedback")
	require.Equal(t, invalid, second[1].Content)
	require.Contains(t, second[2].Content, "query.measures[0]")
	require.Contains(t, second[2].Content, `"did_you_mean": "cancellations"`)

	_, llm, err = generate(t, nil, invalid, `not json`, `{"query":{},"title":"x","viz":"stat","extra":1}`)
	var gerr *GenerateInvalidError
	require.ErrorAs(t, err, &gerr)
	require.NotEmpty(t, gerr.Errors)
	require.Len(t, llm.calls, 3, "initial + 2 retries, never more")

	// Refine: the base query rides into the user turn so the instruction edits it.
	base := &metrics.Query{Measures: []string{"cancellations"}, By: []string{"time"}, Grain: "week", Range: &metrics.QueryRange{Last: "12w"}}
	_, llm, err = generate(t, base, goldenWidget)
	require.NoError(t, err)
	turn := llm.calls[0][0].Content
	require.Contains(t, turn, "Current query to modify:")
	require.Contains(t, turn, `"grain":"week"`)
	require.Contains(t, turn, "Instruction: cancelled per day")
}
