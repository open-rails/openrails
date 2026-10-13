package billing

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCreateProductEntitlementListDecoding(t *testing.T) {
	for _, raw := range []string{`{"key":"p"}`, `{"key":"p","entitlements":[]}`, `{"key":"p","entitlements":["post:101","premium"]}`} {
		var params CreateProductParams
		require.NoError(t, json.Unmarshal([]byte(raw), &params), raw)
		require.Equal(t, "p", params.Key)
	}
	for _, raw := range []string{
		`{"entitlements":null}`,
		`{"entitlements":{"premium":null}}`,
		`{"entitlements_spec":{"premium":null}}`,
		`{"entitlements":[1]}`,
	} {
		var params CreateProductParams
		require.Error(t, json.Unmarshal([]byte(raw), &params), raw)
	}
}

func TestUpdatePricePSPsDecoding(t *testing.T) {
	var params UpdatePriceParams
	require.NoError(t, json.Unmarshal([]byte(`{"psps":{"mobius":{"plan_id":"premium"},"ccbill":null}}`), &params))
	require.Equal(t, map[string]string{"plan_id": "premium"}, params.PSPs["mobius"].Value)
	require.True(t, params.PSPs["ccbill"].Null, "null unlinks a PSP")
}

func TestNotificationURLPicksByPurpose(t *testing.T) {
	profile := MerchantProfile{SupportURL: "https://s/help", SubscribeURL: "https://s/subscribe", BillingURL: "https://s/account"}
	require.Equal(t, "https://s/subscribe", profile.NotificationURL("premium_ended"))
	for _, kind := range []string{"payment_method_failed", "payment_method_update_required", "premium_renewed", "one_off_purchase_completed"} {
		require.Equal(t, "https://s/account", profile.NotificationURL(kind), kind)
	}
	support := MerchantProfile{SupportURL: "https://s/help"}
	require.Equal(t, "https://s/help", support.NotificationURL("premium_ended"))
	require.Equal(t, "https://s/help", support.NotificationURL("payment_method_failed"))
}
