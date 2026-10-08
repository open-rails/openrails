package checkoutsession

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestStoredOfferPreservesRenewalAndAccess(t *testing.T) {
	for _, tc := range []struct {
		raw             string
		billing, access *int
		renew           bool
	}{
		{`{"plan":{"period_hours":720,"automatically_renews":true}}`, new(720), new(720), true},
		{`{"plan":{"period_hours":24,"automatically_renews":false}}`, nil, new(24), false},
		{`{"plan":{"period_hours":null,"automatically_renews":false}}`, nil, nil, false},
		{`{"plan":{"billing_interval_hours":720,"access_duration_hours":null,"auto_renew":true}}`, new(720), nil, true},
		{`{"auto_renew":false,"plan":{"billing_interval_hours":720,"access_duration_hours":24,"auto_renew":false}}`, new(720), new(24), false},
	} {
		var offer Offer
		require.NoError(t, json.Unmarshal([]byte(tc.raw), &offer))
		require.Equal(t, tc.billing, offer.Plan.BillingIntervalHours)
		require.Equal(t, tc.access, offer.Plan.AccessDurationHours)
		require.Equal(t, tc.renew, offer.Plan.AutoRenew)
		raw, err := json.Marshal(offer)
		require.NoError(t, err)
		require.NotContains(t, string(raw), "automatically_renews")
		require.NotContains(t, string(raw), "period_hours")
		var restored Offer
		require.NoError(t, json.Unmarshal(raw, &restored))
		require.Equal(t, offer, restored)
	}
}
