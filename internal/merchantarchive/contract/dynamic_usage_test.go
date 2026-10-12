package contract

import "testing"

func TestDynamicUsageArchiveValues(t *testing.T) {
	checkRows(t, []rowCase{
		{"dynamic receipt", "usage_events", map[string]string{
			"stated_amount": "9007199254740993",
			"charges":       `[{"meter_key":"gpu","model":"dynamic","quantity":3,"amount":"9007199254740993"}]`,
		}, true},
		{"dynamic estimate", "admission_operations", map[string]string{
			"state": "captured", "estimate_usage": `{"event_type":"gpu","dimensions":{"units":3},"amount":"9007199254740993"}`,
		}, true},
		{"numeric money refused", "admission_operations", map[string]string{
			"state": "captured", "estimate_usage": `{"event_type":"gpu","amount":9007199254740993}`,
		}, false},
	})
}
