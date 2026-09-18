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
	require.Equal(t, MoneyCell(3), measureCell(t, measureByName["realized_revenue_per_customer"], g), "a money-unit ratio rounds to whole native units")
	require.Equal(t, MoneyCell(10), measureCell(t, measureByName["net_revenue"], g))
	require.Equal(t, int64(4), measureCell(t, measureByName["paying_customers"], g))
	require.Equal(t, 54.5, measureCell(t, measureByName["ended_membership_days"], g))
	sum, err := (leaf{n: math.MaxInt64 - 1}).add(leaf{n: 1})
	require.NoError(t, err)
	require.Equal(t, leaf{n: math.MaxInt64}, sum, "integer leaves add exactly")
}

func TestBalanceLeafSumBounds(t *testing.T) {
	for _, pair := range [][2]int64{{math.MaxInt64, 1}, {math.MinInt64, -1}} {
		_, err := (leaf{n: pair[0]}).add(leaf{n: pair[1]})
		require.ErrorContains(t, err, "does not fit int64")
	}
	for _, pair := range [][2]int64{{math.MaxInt64 - 1, 1}, {math.MinInt64 + 1, -1}, {math.MinInt64, math.MaxInt64}} {
		sum, err := (leaf{n: pair[0]}).add(leaf{n: pair[1]})
		require.NoError(t, err)
		require.Equal(t, pair[0]+pair[1], sum.n)
	}
}

func measureCell(t *testing.T, m *Measure, g *group) any {
	t.Helper()
	v, err := measureValue(m, g)
	require.NoError(t, err)
	return v
}

// A money-unit ratio is exact: the quotient is a rational, rounded half away
// from zero to whole native units, never a float64 (2^53+1 and MaxInt64
// survive a denominator of one; negatives round symmetrically; a quotient
// outside int64 is refused, never wrapped). Float ratios are the correctly
// rounded float of the exact quotient.
func TestMoneyRatiosAreExact(t *testing.T) {
	average := measureByName["realized_revenue_per_customer"]
	at := func(net, payers int64) any {
		return measureCell(t, average, &group{vals: map[string]leaf{"net_revenue": {n: net}, "paying_customers": {n: payers}}})
	}
	require.Equal(t, MoneyCell(9007199254740993), at(9007199254740993, 1), "2^53+1 over one customer")
	require.Equal(t, MoneyCell(math.MaxInt64), at(math.MaxInt64, 1))
	require.Equal(t, MoneyCell(math.MinInt64), at(math.MinInt64, 1))
	require.Equal(t, MoneyCell(4503599627370497), at(9007199254740993, 2), "…993/2 = …496.5 rounds up, not to the float …496")
	require.Equal(t, MoneyCell(-4503599627370497), at(-9007199254740993, 2), "negative halves round away from zero")
	require.Equal(t, MoneyCell(3), at(10, 4), "2.5 rounds to 3")
	require.Equal(t, MoneyCell(-3), at(-10, 4))
	require.Equal(t, MoneyCell(2), at(7, 3))
	require.Equal(t, MoneyCell(3074457345618258602), at(math.MaxInt64, 3), "…807/3 = …602.33 rounds down")
	require.Nil(t, at(10, 0), "no payers: a null cell, never a division")

	// A money ratio over a fractional float denominator can leave int64: the
	// cell fails closed instead of wrapping.
	perDay := &Measure{Name: "money_per_day", Class: ClassRatio, Unit: UnitMoney, Num: "net_revenue", Den: "ended_membership_days"}
	_, err := measureValue(perDay, &group{vals: map[string]leaf{"net_revenue": {n: math.MaxInt64}, "ended_membership_days": {f: 0.5, float: true}}})
	require.ErrorContains(t, err, "does not fit int64")
	v, err := measureValue(perDay, &group{vals: map[string]leaf{"net_revenue": {n: 9007199254740993}, "ended_membership_days": {f: 0.5, float: true}}})
	require.NoError(t, err)
	require.Equal(t, MoneyCell(18014398509481986), v)

	utilization := measureByName["credit_utilization"]
	ratio := measureCell(t, utilization, &group{vals: map[string]leaf{"usage_revenue": {n: 1}, "credits_sold": {n: 3}}})
	require.Equal(t, 1.0/3.0, ratio, "a float ratio is the correctly rounded quotient")
	huge := measureCell(t, utilization, &group{vals: map[string]leaf{"usage_revenue": {n: 9007199254740993}, "credits_sold": {n: 9007199254740993}}})
	require.Equal(t, 1.0, huge, "equal numerator and denominator above 2^53 is exactly one")
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
