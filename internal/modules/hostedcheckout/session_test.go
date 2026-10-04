package hostedcheckout

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/cardguard"
)

func TestSessionID(t *testing.T) {
	a, err := NewID()
	require.NoError(t, err)
	b, err := NewID()
	require.NoError(t, err)
	require.NotEqual(t, a, b)
	require.Len(t, a, len("ocs_")+64, "ocs_ and 256 random bits")
	require.True(t, ValidID(a))
	for _, bad := range []string{"", "ocs_", a[:len(a)-1], strings.ToUpper(a), "cs_" + a[4:], a + "0", strings.Replace(a, a[4:5], "g", 1)} {
		require.False(t, ValidID(bad), bad)
	}
	require.Len(t, IDHash(a), 32)
	require.NotContains(t, string(IDHash(a)), a)

	// Every submission of one attempt carries one key; the next attempt another.
	first, retry, next := Session{ID: a}, Session{ID: a}, Session{ID: a, Attempt: 1}
	require.Equal(t, first.AttemptKey(), retry.AttemptKey())
	require.NotEqual(t, first.AttemptKey(), next.AttemptKey())
	require.NotEqual(t, first.AttemptKey(), Session{ID: b}.AttemptKey())
	require.NotContains(t, first.AttemptKey(), a[4:], "the key never carries the id")
	require.False(t, cardguard.ContainsPAN(first.AttemptKey()))
}

func TestPayment(t *testing.T) {
	card := Option{HostedCheckoutRail: billing.HostedCheckoutRail{ID: "option_card", Rail: "nmi", Mode: "subscription", Driver: "collect_js"}, Selector: "cards", PSPID: "psp-1"}
	redirect := Option{HostedCheckoutRail: billing.HostedCheckoutRail{ID: "option_redirect", Rail: "ccbill", Driver: "redirect"}, Selector: "ccbill", PSPID: "psp-2"}
	solana := Option{HostedCheckoutRail: billing.HostedCheckoutRail{ID: "option_sol", Rail: "solana", Driver: "solana_pay", PublicConfig: map[string]string{"token_symbol": "usdc"}}, Selector: "solana", PSPID: "psp-3"}
	method := billing.PaymentMethodID{1}.String()
	owns := func(id string) bool { return id == method }
	newCard := billing.HostedCheckoutPayRequest{PaymentToken: " tok ", NameOnCard: "A Buyer", Country: "us", Zip: "10001", Address1: "1 Main St", Email: "attacker@example.test"}

	got, err := Payment(card, newCard, "buyer@example.test", owns)
	require.NoError(t, err)
	require.Equal(t, billing.CheckoutPaymentOptions{Rail: "cards", PSPID: "psp-1", PaymentToken: "tok", Email: "buyer@example.test", NameOnCard: "A Buyer", Country: "US", Zip: "10001"}, got,
		"the option binds the PSP, the account the email; the compact form carries no street")

	got, err = Payment(card, billing.HostedCheckoutPayRequest{PaymentMethodID: method, NameOnCard: "Someone Else", Zip: "99999", Country: "US"}, "", owns)
	require.NoError(t, err)
	require.Equal(t, billing.CheckoutPaymentOptions{Rail: "cards", PSPID: "psp-1", PaymentMethodID: method}, got, "a saved card keeps its own billing identity")

	got, err = Payment(solana, billing.HostedCheckoutPayRequest{TokenSymbol: "USDC"}, "", owns)
	require.NoError(t, err)
	require.Equal(t, "USDC", got.TokenSymbol)
	require.Equal(t, "transfer_request", got.Flow)

	_, err = Payment(redirect, billing.HostedCheckoutPayRequest{NameOnCard: "A Buyer", Country: "US", Zip: "10001"}, "buyer@example.test", owns)
	require.NoError(t, err)

	other := billing.PaymentMethodID{2}.String()
	for name, tc := range map[string]struct {
		option Option
		in     billing.HostedCheckoutPayRequest
	}{
		"neither token nor method":  {card, billing.HostedCheckoutPayRequest{}},
		"both token and method":     {card, billing.HostedCheckoutPayRequest{PaymentToken: "tok", PaymentMethodID: method, NameOnCard: "A", Country: "US", Zip: "10001"}},
		"someone else's method":     {card, billing.HostedCheckoutPayRequest{PaymentMethodID: other}},
		"malformed method id":       {card, billing.HostedCheckoutPayRequest{PaymentMethodID: "pm_nope"}},
		"new card without a name":   {card, billing.HostedCheckoutPayRequest{PaymentToken: "tok", Country: "US", Zip: "10001"}},
		"new card bad country":      {card, billing.HostedCheckoutPayRequest{PaymentToken: "tok", NameOnCard: "A", Country: "USA", Zip: "10001"}},
		"new card missing postal":   {card, billing.HostedCheckoutPayRequest{PaymentToken: "tok", NameOnCard: "A", Country: "DE"}},
		"new card bad US postal":    {card, billing.HostedCheckoutPayRequest{PaymentToken: "tok", NameOnCard: "A", Country: "US", Zip: "1000"}},
		"oversized token":           {card, billing.HostedCheckoutPayRequest{PaymentToken: strings.Repeat("t", 4097), NameOnCard: "A", Country: "US", Zip: "10001"}},
		"card token on a redirect":  {redirect, billing.HostedCheckoutPayRequest{PaymentToken: "tok"}},
		"another settlement token":  {solana, billing.HostedCheckoutPayRequest{TokenSymbol: "SOL"}},
		"saved method on solana":    {solana, billing.HostedCheckoutPayRequest{PaymentMethodID: method}},
		"a driver no page can run":  {Option{HostedCheckoutRail: billing.HostedCheckoutRail{Driver: "stripe_elements"}}, billing.HostedCheckoutPayRequest{PaymentMethodID: method}},
		"solana option, no binding": {Option{HostedCheckoutRail: billing.HostedCheckoutRail{Driver: "solana_pay"}}, billing.HostedCheckoutPayRequest{}},
	} {
		_, err := Payment(tc.option, tc.in, "buyer@example.test", owns)
		require.ErrorIs(t, err, ErrInvalid, name)
	}

	_, err = Payment(card, billing.HostedCheckoutPayRequest{PaymentToken: "tok", NameOnCard: "A", Country: "QA"}, "", owns)
	require.NoError(t, err, "a postal code is not demanded where none exists")

	// A PSP whose card_entry is server takes the card itself (#1129).
	server := Option{HostedCheckoutRail: billing.HostedCheckoutRail{ID: "option_server", Rail: "nmi", Driver: "card"}, Selector: "cards", PSPID: "psp-4"}
	entered, err := billing.NewCard("4111111111111111", 10, 2027, "0739")
	require.NoError(t, err)
	defer entered.Zero()
	got, err = Payment(server, billing.HostedCheckoutPayRequest{Card: entered, NameOnCard: "A Buyer", Country: "US", Zip: "10001", LastFour: "0000"}, "", owns)
	require.NoError(t, err)
	require.Same(t, entered, got.Card)
	require.Empty(t, got.LastFour, "the card names itself")
	_, err = Payment(server, billing.HostedCheckoutPayRequest{PaymentMethodID: method}, "", owns)
	require.NoError(t, err)
	for name, tc := range map[string]struct {
		option Option
		in     billing.HostedCheckoutPayRequest
	}{
		"a token for a card page":     {server, billing.HostedCheckoutPayRequest{PaymentToken: "tok", NameOnCard: "A", Country: "US", Zip: "10001"}},
		"card and a saved method":     {server, billing.HostedCheckoutPayRequest{Card: entered, PaymentMethodID: method}},
		"a card for a tokenizing PSP": {card, billing.HostedCheckoutPayRequest{Card: entered, NameOnCard: "A", Country: "US", Zip: "10001"}},
		"a card on a redirect":        {redirect, billing.HostedCheckoutPayRequest{Card: entered}},
		"a card on solana":            {solana, billing.HostedCheckoutPayRequest{Card: entered, TokenSymbol: "USDC"}},
		"a card without a name":       {server, billing.HostedCheckoutPayRequest{Card: entered, Country: "US", Zip: "10001"}},
	} {
		_, err := Payment(tc.option, tc.in, "", owns)
		require.ErrorIs(t, err, ErrInvalid, name)
	}
}

func TestDrivable(t *testing.T) {
	for driver, ok := range map[string]bool{"collect_js": true, "card": true, "redirect": true, "solana_pay": true, "stripe_elements": false, "": false} {
		require.Equal(t, ok, Drivable(driver), driver)
	}
}
