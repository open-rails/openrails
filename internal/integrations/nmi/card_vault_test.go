package nmi

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/cardguard"
)

// NMI's documented test Visa.
const wireCardNumber = "4111111111111111"

func wireCard(t *testing.T) *cardguard.Card {
	t.Helper()
	card, err := cardguard.NewCard("4111 1111 1111 1111", 10, 2027, "999")
	require.NoError(t, err)
	return card
}

// The server card-entry request, byte for byte: the classic Customer Vault
// call with the caller's vault and billing ids, the card as ccnumber, MMYY
// ccexp and cvv, and no Origin or Referer.
func TestCardVaultWire(t *testing.T) {
	var header http.Header
	var body string
	answer := "response=1&responsetext=Customer+Added&authcode=&transactionid=&avsresponse=&cvvresponse=&orderid=&type=&response_code=100&customer_vault_id=700000000000000001"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		header, body = r.Header.Clone(), string(raw)
		_, _ = io.WriteString(w, answer)
	}))
	t.Cleanup(srv.Close)
	c := nmiClient(t, srv.URL)
	billing := CreateCustomerVaultData{FirstName: "Ada", LastName: "Lovelace", Address1: "1 Main St", Zip: "10001", Country: "US"}

	card := wireCard(t)
	got, err := c.CreateCustomerVaultFromCard(t.Context(), " 700000000000000001 ", "b-1", billing, card)
	require.NoError(t, err)
	require.Equal(t, CreateCustomerVaultResponse{CustomerVaultID: "700000000000000001"}, *got)
	require.Equal(t, "address1=1+Main+St&billing_id=b-1&country=US&customer_vault=add_customer&customer_vault_id=700000000000000001"+
		"&first_name=Ada&last_name=Lovelace&security_key=wire-key&zip=10001&ccnumber=4111111111111111&ccexp=1027&cvv=999", body)
	require.Equal(t, "application/x-www-form-urlencoded", header.Get("Content-Type"))
	for _, absent := range []string{"Origin", "Referer", "Authorization", "Cookie"} {
		require.Empty(t, header.Values(absent), absent)
	}
	require.False(t, cardguard.Unseal(card, func([]byte, []byte, int, int) {}), "the card is wiped once the gateway has it")

	card = wireCard(t)
	require.NoError(t, c.AddCustomerBillingFromCard(t.Context(), "700000000000000001", "b-2", CreateCustomerVaultData{Zip: "10001"}, card))
	require.Equal(t, "billing_id=b-2&customer_vault=add_billing&customer_vault_id=700000000000000001&security_key=wire-key&zip=10001"+
		"&ccnumber=4111111111111111&ccexp=1027&cvv=999", body)
	require.False(t, cardguard.Unseal(card, func([]byte, []byte, int, int) {}))
}

// However the call ends, the card is wiped and no error carries it.
func TestCardVaultOutcomesNeverKeepTheCard(t *testing.T) {
	refused := "response=3&responsetext=Invalid+Credit+Card+Number+" + wireCardNumber + "+REFID%3A1&authcode=&transactionid=&avsresponse=&cvvresponse=&orderid=&type=&response_code=300"
	for name, tc := range map[string]struct {
		respond   func(nmiCall) (int, string)
		billing   CreateCustomerVaultData
		ids       [2]string
		ambiguous bool
		calls     int
	}{
		"gateway refusal":   {respond: reply(refused), ids: [2]string{"v1", "b1"}, calls: 1},
		"gateway error":     {respond: func(nmiCall) (int, string) { return http.StatusBadGateway, "" }, ids: [2]string{"v1", "b1"}, ambiguous: true, calls: 1},
		"unreadable answer": {respond: reply("response=1&response=2"), ids: [2]string{"v1", "b1"}, ambiguous: true, calls: 1},
		"card with a token": {respond: reply(""), billing: CreateCustomerVaultData{PaymentToken: "tok"}, ids: [2]string{"v1", "b1"}},
		"no vault id":       {respond: reply(""), ids: [2]string{"", "b1"}},
		"no billing id":     {respond: reply(""), ids: [2]string{"v1", " "}},
	} {
		t.Run(name, func(t *testing.T) {
			f := newNMIFake(t, tc.respond)
			card := wireCard(t)
			_, err := f.client(t).CreateCustomerVaultFromCard(t.Context(), tc.ids[0], tc.ids[1], tc.billing, card)
			require.Error(t, err)
			require.Equal(t, tc.ambiguous, IsTransportAmbiguous(err))
			require.Len(t, f.Calls(), tc.calls)
			require.NotContains(t, err.Error(), wireCardNumber)
			require.NotContains(t, err.Error(), "4111")
			if !tc.ambiguous && tc.calls == 1 {
				var rejection *CustomerVaultError
				require.ErrorAs(t, err, &rejection)
				require.Equal(t, 300, rejection.ResponseCode)
				require.Empty(t, rejection.RawResponse)
				require.Equal(t, "refused", rejection.ResponseText, "a responsetext echoing the number is dropped")
			}
			require.False(t, cardguard.Unseal(card, func([]byte, []byte, int, int) {}))
		})
	}

	// A read-only client refuses before the gateway and still wipes the card.
	f := newNMIFake(t, reply(""))
	client, card := f.client(t), wireCard(t)
	client.ReadOnly = true
	_, err := client.CreateCustomerVaultFromCard(t.Context(), "v1", "b1", CreateCustomerVaultData{}, card)
	require.ErrorIs(t, err, ErrProviderReadOnly)
	require.Empty(t, f.Calls())
	require.False(t, cardguard.Unseal(card, func([]byte, []byte, int, int) {}))
}
