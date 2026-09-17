package operator

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// Fleet money travels as exact decimal strings in native units under the
// wire's `<thing>_amount` names (docs/money-wire.md); counts stay numbers.
func TestFleetSnapshotWire(t *testing.T) {
	snapshot := FleetSnapshot{
		WindowDays: 30,
		Merchants:  FleetMerchantFunnel{Total: 4, Armed: 3, FirstRevenue: 2, ActiveRevenue: 1},
		Revenue:    []FleetCurrencyRevenue{{Currency: "USD", Payments: 41, SettledAmount: 9007199254740993}},
		Rails:      []FleetRailHealth{{Rail: "nmi", Succeeded: 40, Failed: 1, Chargebacks: 0}},
		MRR:        []FleetMRR{{Currency: "JPY", Subscriptions: 3, MonthlyAmount: 297000}},
	}
	data, err := json.Marshal(snapshot)
	require.NoError(t, err)
	require.JSONEq(t, `{
		"window_days": 30,
		"merchants": {"total": 4, "armed": 3, "first_revenue": 2, "active_revenue": 1},
		"revenue": [{"currency": "USD", "payments": 41, "settled_amount": "9007199254740993"}],
		"rails": [{"rail": "nmi", "succeeded": 40, "failed": 1, "chargebacks": 0}],
		"mrr": [{"currency": "JPY", "subscriptions": 3, "monthly_amount": "297000"}]
	}`, string(data))

	var back FleetSnapshot
	require.NoError(t, json.Unmarshal(data, &back))
	require.Equal(t, snapshot, back)
}

func TestFleetSeriesWire(t *testing.T) {
	week := time.Date(2026, 9, 14, 0, 0, 0, 0, time.UTC)
	series := FleetSeries{
		Weeks:  12,
		Points: []FleetWeeklyPoint{{WeekStart: week, NewMerchants: 2, ActiveMerchants: 5, CancelledSubscriptions: 1}},
		Volume: []FleetWeeklyVolume{{WeekStart: week, Currency: "USD", Payments: 7, SettledAmount: 495000000}},
	}
	data, err := json.Marshal(series)
	require.NoError(t, err)
	require.JSONEq(t, `{
		"weeks": 12,
		"points": [{"week_start": "2026-09-14T00:00:00Z", "new_merchants": 2, "active_merchants": 5, "cancelled_subscriptions": 1}],
		"volume": [{"week_start": "2026-09-14T00:00:00Z", "currency": "USD", "payments": 7, "settled_amount": "495000000"}]
	}`, string(data))

	var back FleetSeries
	require.NoError(t, json.Unmarshal(data, &back))
	require.Equal(t, series, back)
}
