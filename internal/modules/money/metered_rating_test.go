package money

import (
	"testing"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/pkg/pricing"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestResolvePayerRateCardOverrides(t *testing.T) {
	meterKey := "requests"
	defaultCard := catalogRateCardRow{
		ID:          uuid.New(),
		MeterKey:    meterKey,
		EventType:   "request.completed",
		ValueKey:    "$.units",
		Aggregation: pricing.AggregationSum,
		GroupBy:     map[string]string{"region": "$.region"},
		Filter:      map[string][]string{"region": {"eu"}},
		Allowance:   &pricing.Allowance{Included: 5},
		Price: pricing.RatePrice{
			Model:    pricing.ModelPerUnit,
			Currency: "USD",
			PerUnit:  &pricing.PerUnitPrice{UnitAmount: 10_000},
		},
	}
	override := catalogRateCardRow{
		ID:          uuid.New(),
		MeterKey:    meterKey,
		PayerScoped: true,
		EventType:   "poisoned.event",
		GroupBy:     map[string]string{"poisoned": "$.poisoned"},
		Filter:      map[string][]string{"poisoned": {"value"}},
		Allowance:   &pricing.Allowance{Included: 20},
		Price: pricing.RatePrice{
			Model:    pricing.ModelPerUnit,
			Currency: "USD",
			PerUnit:  &pricing.PerUnitPrice{UnitAmount: 4_000},
		},
	}

	resolved, err := resolvePayerRateCardOverrides([]catalogRateCardRow{override, defaultCard})
	require.NoError(t, err)
	require.Len(t, resolved, 1)
	card := resolved[0]
	assert.Equal(t, override.ID, card.ID)
	assert.True(t, card.PayerScoped)
	assert.Equal(t, override.Price, card.Price)
	assert.Equal(t, override.Allowance, card.Allowance)
	assert.Equal(t, defaultCard.EventType, card.EventType)
	assert.Equal(t, defaultCard.GroupBy, card.GroupBy)
	assert.Equal(t, defaultCard.Filter, card.Filter)
}

func TestResolvePayerRateCardOverridesRequiresDefault(t *testing.T) {
	_, err := resolvePayerRateCardOverrides([]catalogRateCardRow{{
		ID:          uuid.New(),
		MeterKey:    "requests",
		PayerScoped: true,
	}})
	require.ErrorIs(t, err, ErrDefaultRateCardRequired)
}

func TestRateCardFilterRulesResolveEveryDeclaredDimension(t *testing.T) {
	card := catalogRateCardRow{
		ID:      uuid.New(),
		GroupBy: map[string]string{"region": "metadata.region", "plan": "$.plan"},
		Filter:  map[string][]string{"region": {"eu"}, "plan": {"pro", "team"}},
	}

	rules, err := rateCardFilterRules(card)
	require.NoError(t, err)
	assert.ElementsMatch(t, []usageFilterRule{
		{PropertyKey: "region", AllowedValues: []string{"eu"}},
		{PropertyKey: "plan", AllowedValues: []string{"pro", "team"}},
	}, rules)
}

func TestRateCardFilterRulesRejectMissingMeterProperty(t *testing.T) {
	card := catalogRateCardRow{
		ID:      uuid.New(),
		GroupBy: map[string]string{},
		Filter:  map[string][]string{"region": {"eu"}},
	}

	_, err := rateCardFilterRules(card)
	require.ErrorContains(t, err, `filter dimension "region" has no meter property`)
}
