package dashboard

import (
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
