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

func TestUpdatePriceSellViaDecoding(t *testing.T) {
	var params UpdatePriceParams
	require.NoError(t, json.Unmarshal([]byte(`{"sell_via":["solana"]}`), &params))
	require.Equal(t, []string{SellViaSolana}, params.SellVia.Value)
	var every UpdatePriceParams
	require.NoError(t, json.Unmarshal([]byte(`{"sell_via":null}`), &every))
	require.True(t, every.SellVia.Null, "null sells through every method")
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
