package ccbill

import (
	"net/url"
	"testing"

	"github.com/open-rails/openrails/config"
	"github.com/stretchr/testify/require"
)

// #819 wire-pinning: the price's currency — not a hardcoded USD — decides the
// FlexForm `currencyCode` on the wire. Known ISO alpha in => exact CCBill
// numeric code out.
func TestFlexFormCurrencyCodeIsPinnedToThePriceCurrency(t *testing.T) {
	client := NewClient(&config.CCBillConfig{ClientAccNum: "945280", ClientSubAcc: "0000"}, true)

	for _, tc := range []struct {
		currency string
		wireCode string
	}{
		{"usd", "840"},
		{"USD", "840"},
		{"eur", "978"},
		{" EUR ", "978"},
		{"gbp", "826"},
		{"cad", "124"},
		{"aud", "036"}, // leading zero must survive: string table, never an int
		{"jpy", "392"},
	} {
		resp, err := client.GenerateFlexFormURL(&GenerateFlexFormURLParams{
			Username: "alice",
			Email:    "alice@example.com",
			FormName: "premium",
			FlexID:   "flex-123",
			Currency: tc.currency,
		})
		require.NoError(t, err, "currency %q", tc.currency)
		parsed, err := url.Parse(resp.RedirectURL)
		require.NoError(t, err)
		require.Equal(t, tc.wireCode, parsed.Query().Get("currencyCode"), "currency %q", tc.currency)
	}
}

// A currency CCBill cannot bill must be refused BEFORE any URL exists — a
// customer can only be charged by loading the form, so refusing here is
// refusing before the charge. Silently substituting USD is the unacceptable
// option (#819).
func TestFlexFormRefusesUnbillableCurrencyBeforeGeneratingAURL(t *testing.T) {
	client := NewClient(&config.CCBillConfig{ClientAccNum: "945280", ClientSubAcc: "0000"}, true)

	for _, currency := range []string{"sek", "chf", "", "  ", "eu", "euro"} {
		resp, err := client.GenerateFlexFormURL(&GenerateFlexFormURLParams{
			Username: "alice",
			Email:    "alice@example.com",
			FormName: "premium",
			FlexID:   "flex-123",
			Currency: currency,
		})
		require.Error(t, err, "currency %q must be refused", currency)
		require.Nil(t, resp)

		upgradeResp, err := client.GenerateUpgradeFlexFormURL(&GenerateUpgradeFlexFormURLParams{
			Username:               "alice",
			Email:                  "alice@example.com",
			FormName:               "premium",
			FlexID:                 "flex-123",
			Currency:               currency,
			OriginalSubscriptionID: "sub-1",
		})
		require.Error(t, err, "upgrade currency %q must be refused", currency)
		require.Nil(t, upgradeResp)
	}
}

func TestUpgradeFlexFormCarriesTheTargetPriceCurrency(t *testing.T) {
	client := NewClient(&config.CCBillConfig{ClientAccNum: "945280", ClientSubAcc: "0000"}, true)

	resp, err := client.GenerateUpgradeFlexFormURL(&GenerateUpgradeFlexFormURLParams{
		Username:               "alice",
		Email:                  "alice@example.com",
		FormName:               "premium",
		FlexID:                 "flex-123",
		Currency:               "eur",
		OriginalSubscriptionID: "sub-1",
	})
	require.NoError(t, err)
	parsed, err := url.Parse(resp.RedirectURL)
	require.NoError(t, err)
	require.Equal(t, "978", parsed.Query().Get("currencyCode"))
}

// The outbound alpha->numeric map and the inbound numeric->alpha map are the
// same table read in both directions: a currency we can bill is a currency the
// webhook can match, so no currency can be "charged then rejected".
func TestCurrencyCodeRoundTrips(t *testing.T) {
	for _, currency := range SupportedCurrencies() {
		code, err := CurrencyCode(currency)
		require.NoError(t, err)
		back, ok := CurrencyFromCode(code)
		require.True(t, ok, "code %q must map back", code)
		require.Equal(t, currency, back)
	}
	_, ok := CurrencyFromCode("999")
	require.False(t, ok)
}
