package metrics

import (
	"encoding/json"
	"math"
	"testing"

	"github.com/stretchr/testify/require"
)

// Money cells are exact decimal strings on the wire, counts JSON numbers and
// ratios floats; the full int64 range survives, and a money-unit ratio is a
// whole-unit MoneyCell.
func TestResultWireEncoding(t *testing.T) {
	res := Result{
		Columns: []Column{{Name: "currency", Kind: "dimension"}, {Name: "gross_revenue", Kind: "measure", Unit: UnitMoney}, {Name: "payment_count", Kind: "measure", Unit: "count"}, {Name: "approval_rate", Kind: "measure", Unit: "ratio"}},
		Rows:    [][]any{{"USD", MoneyCell(math.MaxInt64), int64(3), 0.5}, {"JPY", MoneyCell(math.MinInt64), int64(0), nil}},
	}
	raw, err := json.Marshal(res)
	require.NoError(t, err)
	require.Contains(t, string(raw), `["USD","9223372036854775807",3,0.5]`)
	require.Contains(t, string(raw), `["JPY","-9223372036854775808",0,null]`)
	var back Result
	require.NoError(t, json.Unmarshal(raw, &back))
	var cell MoneyCell
	require.NoError(t, json.Unmarshal([]byte(`"9223372036854775807"`), &cell))
	require.Equal(t, MoneyCell(math.MaxInt64), cell)
	require.Error(t, json.Unmarshal([]byte(`9223372036854775807`), &cell), "a JSON number is not a money cell")

	g := &group{vals: map[string]leaf{"net_revenue": {n: 10}, "paying_customers": {n: 4}, "ended_membership_days": {f: 54.5, float: true}}}
	require.Equal(t, MoneyCell(3), measureValue(measureByName["realized_revenue_per_customer"], g), "a money-unit ratio rounds to whole native units")
	require.Equal(t, MoneyCell(10), measureValue(measureByName["net_revenue"], g))
	require.Equal(t, int64(4), measureValue(measureByName["paying_customers"], g))
	require.Equal(t, 54.5, measureValue(measureByName["ended_membership_days"], g))
	require.Equal(t, leaf{n: math.MaxInt64}, leaf{n: math.MaxInt64 - 1}.add(leaf{n: 1}), "integer leaves add exactly")
}

// Every money measure carries the money unit, so a client keys money
// formatting off one label.
func TestMoneyMeasuresCarryTheMoneyUnit(t *testing.T) {
	for _, m := range Measures {
		if m.Money && m.Class != ClassRatio {
			require.Equal(t, UnitMoney, m.Unit, m.Name)
		}
		if m.Unit == UnitMoney {
			require.True(t, m.Money, m.Name)
		}
	}
}
