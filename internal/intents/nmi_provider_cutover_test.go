package intents

import (
	"encoding/json"
	"testing"

	"github.com/open-rails/openrails/internal/integrations/nmi"
	"github.com/stretchr/testify/require"
)

func TestCutoverPlanExactProviderAmount(t *testing.T) {
	for _, tc := range []struct {
		name     string
		micros   int64
		provider string
		matches  bool
	}{
		{"one_cent", 10_000, "0.01", true},
		{"usual_price", 19_990_000, "19.99", true},
		{"above_float_integer_range", 9_007_199_254_750_000, "9007199254.75", true},
		{"hundredfold_drift", 19_990_000, "1999.00", false},
		{"subcent_not_rounded", 19_990_001, "19.99", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := nmiCutoverPayload{Currency: "usd", Amount: tc.micros, CycleHours: 720, PlanID: "exact-plan"}
			plan := nmi.V5Plan{ID: p.PlanID, PlanAmount: tc.provider, PlanPayments: "0", DayFrequency: "30"}
			require.Equal(t, tc.matches, cutoverPlanMatches(plan, p))
		})
	}
}

func TestCutoverPauseScalarShapes(t *testing.T) {
	for _, tc := range []struct {
		raw           string
		paused, known bool
	}{
		{`true`, true, true}, {`false`, false, true}, {`1`, true, true}, {`0`, false, true},
		{`"1"`, true, true}, {`"0"`, false, true}, {`null`, false, false},
		{`2`, false, false}, {`0.5`, false, false}, {`{}`, false, false}, {`"true"`, false, false},
	} {
		t.Run(tc.raw, func(t *testing.T) {
			var value any
			require.NoError(t, json.Unmarshal([]byte(tc.raw), &value))
			paused, known := cutoverPaused(value)
			require.Equal(t, tc.paused, paused)
			require.Equal(t, tc.known, known)
		})
	}
}
