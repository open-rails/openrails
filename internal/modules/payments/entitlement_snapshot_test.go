package payments

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/open-rails/openrails/internal/modules/grants"
	"github.com/stretchr/testify/require"
)

func TestAcceptedEntitlementListUsesOnlyPriceDuration(t *testing.T) {
	start := time.Date(2026, 10, 8, 0, 0, 0, 0, time.UTC)
	for _, duration := range []*int{nil, new(72)} {
		payload := NMISalePayload{Entitlements: []string{"post:101", "premium", " opaque key "}, AccessDurationHours: duration}
		raw, err := json.Marshal(payload)
		require.NoError(t, err)
		require.Contains(t, string(raw), `"entitlements":["post:101","premium"," opaque key "]`)
		require.NotContains(t, string(raw), "legacy_entitlements")
		var restored NMISalePayload
		require.NoError(t, json.Unmarshal(raw, &restored))
		require.Equal(t, payload.Entitlements, restored.Entitlements)
		require.Nil(t, restored.LegacyEntitlements)
		windows, _ := grants.PurchaseWindows(restored.Entitlements, duration, start, start, nil)
		for _, name := range payload.Entitlements {
			if duration == nil {
				require.Nil(t, windows[name].End)
			} else {
				require.Equal(t, start.Add(72*time.Hour), *windows[name].End)
			}
		}
	}
}

func TestLegacyAcceptedPurchaseRetainsFeatureDurationsOnlyForItsHistory(t *testing.T) {
	var legacy NMISalePayload
	require.NoError(t, json.Unmarshal([]byte(`{"entitlements":{"post:101":12,"premium":null}}`), &legacy))
	require.Equal(t, []string{"post:101", "premium"}, legacy.Entitlements)
	historical := grants.HistoricalEntitlementHours(legacy.LegacyEntitlements)
	require.Equal(t, map[string]int{"post:101": 12}, historical)
	start := time.Date(2026, 10, 8, 0, 0, 0, 0, time.UTC)
	windows, _ := grants.PurchaseWindows(legacy.Entitlements, nil, start, start, historical)
	require.Equal(t, start.Add(12*time.Hour), *windows["post:101"].End)
	require.Nil(t, windows["premium"].End)
	// An accepted price duration was authoritative even for historical maps.
	windows, _ = grants.PurchaseWindows(legacy.Entitlements, new(72), start, start, historical)
	require.Equal(t, start.Add(72*time.Hour), *windows["post:101"].End)
	require.Equal(t, start.Add(72*time.Hour), *windows["premium"].End)
	raw, err := json.Marshal(legacy)
	require.NoError(t, err)
	var resumed NMISalePayload
	require.NoError(t, json.Unmarshal(raw, &resumed))
	require.Equal(t, legacy, resumed, "resuming a frozen legacy purchase keeps its exact original benefit promise")
}

func TestEmptyHistoricalSnapshotRetainsItsWireIdentity(t *testing.T) {
	var original NMISalePayload
	require.NoError(t, json.Unmarshal([]byte(`{"entitlements":{}}`), &original))
	require.NotNil(t, original.LegacyEntitlements)
	raw, err := json.Marshal(original)
	require.NoError(t, err)
	require.Contains(t, string(raw), `"legacy_entitlements":{}`)
	var restored NMISalePayload
	require.NoError(t, json.Unmarshal(raw, &restored))
	require.Equal(t, original, restored, "an empty admitted object is not silently changed to a new list contract")
}
