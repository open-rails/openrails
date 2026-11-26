package catalog

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/catalogrules/syntax"
)

func TestBalanceLimitIsReserved(t *testing.T) {
	require.Error(t, syntax.ValidateLimitKey(BalanceLimit))
	require.NoError(t, syntax.ValidateLimitKey("seats"))
}

func TestLimitCounts(t *testing.T) {
	app, err := ParseApplicationYAML([]byte(`schema_version: 1
meters:
  tokens: {event_type: chat, value_property: tokens, aggregation: sum, group_by: {model: model}}
limits:
  weekly:
    unit: usage_units
    reset: {kind: lifetime}
    counts:
    - {meter: tokens}
    - {meter: tokens, weight: 5, per: 1000, filter: {model: [large]}}
`))
	require.NoError(t, err)
	require.Equal(t, []LimitCount{{Meter: "tokens"}, {Meter: "tokens", Weight: 5, Per: 1000, Filter: map[string][]string{"model": {"large"}}}}, app.Limits["weekly"].Counts.Value)

	_, err = ParseApplicationYAML([]byte("schema_version: 1\nlimits:\n  weekly:\n    unit: usage_units\n    reset: {kind: lifetime}\n    rate_cards: [{meter: tokens, price: {model: per_unit, per_unit: {unit_amount: \"1\"}}}]\n"))
	require.ErrorContains(t, err, `unknown field "rate_cards"`)

	_, err = ParseApplicationYAML([]byte("schema_version: 1\nlimits:\n  spend: {unit: USD, reset: {kind: lifetime}, counts: [{meter: tokens}]}\n"))
	require.ErrorContains(t, err, "a currency limit counts the rated amount and has no counts")

	_, err = ParseApplicationYAML([]byte("schema_version: 1\nlimits:\n  weekly: {unit: usage_units, reset: {kind: lifetime}, counts: [{meter: tokens, weight: -1}]}\n"))
	require.ErrorContains(t, err, "weight and per must be positive")
}
