package ccbill

import (
	"crypto/sha256"
	"encoding/hex"
	"net/url"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/config"
)

func flexQuery(t *testing.T, resp *FlexFormResponse, err error, wantPrefix string) url.Values {
	t.Helper()
	require.NoError(t, err)
	u, err := url.Parse(resp.RedirectURL)
	require.NoError(t, err)
	require.Equal(t, wantPrefix, u.Scheme+"://"+u.Host+u.Path)
	return u.Query()
}

func TestFlexFormURLCarriesAccountPriceCurrencyAndSignature(t *testing.T) {
	cfg := &config.CCBillConfig{ClientAccNum: "945280", ClientSubAcc: "0001", Salt: "pepper"}
	sum := sha256.Sum256([]byte("alice" + "pepper"))
	signature := hex.EncodeToString(sum[:])

	resp, err := NewClient(cfg, true).GenerateFlexFormURL(&GenerateFlexFormURLParams{
		Username: "alice", Email: "a@example.com", FormName: "premium", FlexID: "flex-1", Currency: " eur ",
		CustomerFName: "Alice", CustomerLName: "Liddell", Address1: " 1 Main ", City: " ", ZipCode: " 12345 ", Country: " US ", ReservationID: " res-9 ",
	})
	q := flexQuery(t, resp, err, sandboxFlexFormBase+"/flex-1")
	require.Equal(t, url.Values{
		"clientAccnum": {"945280"}, "clientSubacc": {"0001"}, "formName": {"premium"}, "language": {"English"},
		"currencyCode": {"978"}, "email": {"a@example.com"}, "username": {"alice"}, "signature": {signature},
		"customer_fname": {"Alice"}, "customer_lname": {"Liddell"}, "address1": {"1 Main"},
		"zipcode": {"12345"}, "country": {"US"}, "reservationId": {"res-9"},
	}, q, "blank optional address fields are omitted, never sent empty")

	resp, err = NewClient(&config.CCBillConfig{ClientAccNum: "945280", ClientSubAcc: "0000"}, false).GenerateUpgradeFlexFormURL(&GenerateUpgradeFlexFormURLParams{
		Username: "alice", Email: "a@example.com", FormName: "gold", FlexID: "flex-2", Currency: "aud", OriginalSubscriptionID: "0125217202000000017",
	})
	q = flexQuery(t, resp, err, prodFlexFormBase+"/flex-2")
	require.Equal(t, "036", q.Get("currencyCode"), "the target price's currency, leading zero intact")
	require.Equal(t, "0125217202000000017", q.Get("originalSubscriptionId"))
	require.NotContains(t, q, "signature", "no salt, no signature")
	require.NotContains(t, q, "customer_fname")
}

func TestFlexFormRefusesIncompleteOrUnbillableRequests(t *testing.T) {
	client := NewClient(&config.CCBillConfig{ClientAccNum: "945280", ClientSubAcc: "0000"}, true)
	valid := func() GenerateFlexFormURLParams {
		return GenerateFlexFormURLParams{Username: "u", Email: "e", FormName: "f", FlexID: "x", Currency: "USD"}
	}
	for name, mut := range map[string]func(*GenerateFlexFormURLParams){
		"username": func(p *GenerateFlexFormURLParams) { p.Username = "" },
		"email":    func(p *GenerateFlexFormURLParams) { p.Email = "" },
		"form":     func(p *GenerateFlexFormURLParams) { p.FormName = "" },
		"flex id":  func(p *GenerateFlexFormURLParams) { p.FlexID = "" },
		"currency": func(p *GenerateFlexFormURLParams) { p.Currency = "" },
		"bitcoin":  func(p *GenerateFlexFormURLParams) { p.Currency = "BTC" },
	} {
		p := valid()
		mut(&p)
		resp, err := client.GenerateFlexFormURL(&p)
		require.Error(t, err, name)
		require.Nil(t, resp, name)
	}
	_, err := client.GenerateUpgradeFlexFormURL(&GenerateUpgradeFlexFormURLParams{Username: "u", Email: "e", FormName: "f", FlexID: "x", Currency: "USD"})
	require.ErrorContains(t, err, "original_subscription_id")
	_, err = client.GenerateUpgradeFlexFormURL(&GenerateUpgradeFlexFormURLParams{Username: "u", Email: "e", FormName: "f", FlexID: "x", Currency: "XYZ", OriginalSubscriptionID: "s"})
	var unsupported *UnsupportedCurrencyError
	require.ErrorAs(t, err, &unsupported)

	_, err = CurrencyCode("")
	require.ErrorContains(t, err, "never defaulted")
	require.Panics(t, func() { NewClient(nil, true) })
}

func TestCurrencyCodesRoundTripOneTable(t *testing.T) {
	require.Equal(t, []string{"AUD", "CAD", "EUR", "GBP", "JPY", "USD"}, SupportedCurrencies())
	want := map[string]string{"AUD": "036", "CAD": "124", "EUR": "978", "GBP": "826", "JPY": "392", "USD": "840"}
	for _, currency := range SupportedCurrencies() {
		code, err := CurrencyCode(currency)
		require.NoError(t, err)
		require.Equal(t, want[currency], code)
		back, ok := CurrencyFromCode(" " + code + " ")
		require.True(t, ok)
		require.Equal(t, currency, back, "webhook currencies land upper case")
	}
	for _, code := range []string{"36", "999", ""} {
		_, ok := CurrencyFromCode(code)
		require.False(t, ok, code)
	}
}

func TestWebhookAuthPinsTheArmedAccount(t *testing.T) {
	c := NewRESTClient(&config.CCBillConfig{ClientAccNum: "945280", ClientSubAcc: "0001"})
	require.NoError(t, c.ValidateWebhookAuth("945280", "0001"))
	require.ErrorContains(t, c.ValidateWebhookAuth("945281", "0001"), "clientAccnum")
	require.ErrorContains(t, c.ValidateWebhookAuth("945280", "0000"), "clientSubacc")
	require.Error(t, c.ValidateWebhookAuth("945280", ""))
}
