package metrics

import (
	"encoding/json"
	"math"
	"testing"

	"github.com/stretchr/testify/require"
)

func cell(t *testing.T, m *Measure, vals map[string]leaf) any {
	t.Helper()
	v, err := measureValue(m, &group{vals: vals})
	require.NoError(t, err)
	return v
}

// Money cells are exact decimal strings, counts JSON numbers, ratios floats; full int64 range survives.
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

	var c MoneyCell
	require.NoError(t, json.Unmarshal([]byte(`"9223372036854775807"`), &c))
	require.Equal(t, MoneyCell(math.MaxInt64), c)
	require.Error(t, json.Unmarshal([]byte(`9223372036854775807`), &c), "a JSON number is not a money cell")

	vals := map[string]leaf{"net_revenue": {n: 10}, "paying_customers": {n: 4}, "ended_membership_days": {f: 54.5, float: true}}
	require.Equal(t, MoneyCell(10), cell(t, measureByName["net_revenue"], vals))
	require.Equal(t, int64(4), cell(t, measureByName["paying_customers"], vals))
	require.Equal(t, 54.5, cell(t, measureByName["ended_membership_days"], vals))
}

// Money ratios are exact rationals rounded half away from zero, never float64; overflow fails closed.
func TestMoneyRatiosAreExact(t *testing.T) {
	avg := measureByName["realized_revenue_per_customer"]
	for _, tc := range []struct {
		net, payers int64
		want        any
	}{
		{9007199254740993, 1, MoneyCell(9007199254740993)}, // 2^53+1
		{math.MaxInt64, 1, MoneyCell(math.MaxInt64)},
		{math.MinInt64, 1, MoneyCell(math.MinInt64)},
		{9007199254740993, 2, MoneyCell(4503599627370497)}, // .5 rounds up, not to the float
		{-9007199254740993, 2, MoneyCell(-4503599627370497)},
		{10, 4, MoneyCell(3)},
		{-10, 4, MoneyCell(-3)},
		{7, 3, MoneyCell(2)},
		{math.MaxInt64, 3, MoneyCell(3074457345618258602)},
		{10, 0, nil}, // no payers: null, never a division
	} {
		require.Equal(t, tc.want, cell(t, avg, map[string]leaf{"net_revenue": {n: tc.net}, "paying_customers": {n: tc.payers}}), "%d/%d", tc.net, tc.payers)
	}

	perDay := &Measure{Name: "money_per_day", Class: ClassRatio, Unit: UnitMoney, Num: "net_revenue", Den: "ended_membership_days"}
	_, err := measureValue(perDay, &group{vals: map[string]leaf{"net_revenue": {n: math.MaxInt64}, "ended_membership_days": {f: 0.5, float: true}}})
	require.ErrorContains(t, err, "does not fit int64")
	require.Equal(t, MoneyCell(18014398509481986), cell(t, perDay, map[string]leaf{"net_revenue": {n: 9007199254740993}, "ended_membership_days": {f: 0.5, float: true}}))

	util := measureByName["credit_utilization"]
	require.Equal(t, 1.0/3.0, cell(t, util, map[string]leaf{"usage_revenue": {n: 1}, "credits_sold": {n: 3}}))
	require.Equal(t, 1.0, cell(t, util, map[string]leaf{"usage_revenue": {n: 9007199254740993}, "credits_sold": {n: 9007199254740993}}))
}

func TestLeafAddIsExactAndBounded(t *testing.T) {
	for _, p := range [][2]int64{{math.MaxInt64, 1}, {math.MinInt64, -1}} {
		_, err := leaf{n: p[0]}.add(leaf{n: p[1]})
		require.ErrorContains(t, err, "does not fit int64")
	}
	for _, p := range [][2]int64{{math.MaxInt64 - 1, 1}, {math.MinInt64 + 1, -1}, {math.MinInt64, math.MaxInt64}} {
		sum, err := leaf{n: p[0]}.add(leaf{n: p[1]})
		require.NoError(t, err)
		require.Equal(t, leaf{n: p[0] + p[1]}, sum)
	}
}
