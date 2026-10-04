package checkoutsession

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
	card := Option{CheckoutSessionOption: CheckoutSessionOption{ID: "option_card", Rail: "nmi", Mode: "subscription", Driver: "collect_js", PSPID: "psp-1"}, Selector: "cards"}
	redirect := Option{CheckoutSessionOption: CheckoutSessionOption{ID: "option_redirect", Rail: "ccbill", Driver: "redirect", PSPID: "psp-2"}, Selector: "ccbill"}
	stripe := Option{CheckoutSessionOption: CheckoutSessionOption{ID: "option_stripe", Rail: "stripe", Mode: "subscription", Driver: "stripe_elements", PSPID: "psp-5"}, Selector: "stripe"}
	solana := Option{CheckoutSessionOption: CheckoutSessionOption{ID: "option_sol", Rail: "solana", Driver: "solana_pay", PublicConfig: map[string]string{"token_symbol": "usdc"}, PSPID: "psp-3"}, Selector: "solana"}
	method := billing.PaymentMethodID{1}.String()
	owns := func(id string) bool { return id == method }
	newCard := CheckoutSessionPayRequest{PaymentToken: " tok ", NameOnCard: "A Buyer", Country: "us", Zip: "10001", Address1: "1 Main St", Email: "attacker@example.test"}

	got, _, err := Payment(card, newCard, "buyer@example.test", owns)
	require.NoError(t, err)
	require.Equal(t, billing.CheckoutPaymentOptions{Rail: "cards", PSPID: "psp-1", PaymentToken: "tok", Email: "buyer@example.test", NameOnCard: "A Buyer", Country: "US", Zip: "10001"}, got,
		"the option binds the PSP, the account the email; the compact form carries no street")

	got, _, err = Payment(card, CheckoutSessionPayRequest{PaymentMethodID: method, NameOnCard: "Someone Else", Zip: "99999", Country: "US"}, "", owns)
	require.NoError(t, err)
	require.Equal(t, billing.CheckoutPaymentOptions{Rail: "cards", PSPID: "psp-1", PaymentMethodID: method}, got, "a saved card keeps its own billing identity")

	got, _, err = Payment(stripe, CheckoutSessionPayRequest{PaymentMethodID: method, NameOnCard: "Ignored", Zip: "99999"}, "buyer@example.test", owns)
	require.NoError(t, err)
	require.Equal(t, billing.CheckoutPaymentOptions{Rail: "stripe", PSPID: "psp-5", PaymentMethodID: method, Email: "buyer@example.test"}, got, "a card saved in Stripe's fields")

	got, _, err = Payment(solana, CheckoutSessionPayRequest{TokenSymbol: "USDC"}, "", owns)
	require.NoError(t, err)
	require.Equal(t, "USDC", got.TokenSymbol)
	require.Equal(t, "transfer_request", got.Flow)

	_, _, err = Payment(redirect, CheckoutSessionPayRequest{NameOnCard: "A Buyer", Country: "US", Zip: "10001"}, "buyer@example.test", owns)
	require.NoError(t, err)

	other := billing.PaymentMethodID{2}.String()
	for name, tc := range map[string]struct {
		option Option
		in     CheckoutSessionPayRequest
	}{
		"neither token nor method":  {card, CheckoutSessionPayRequest{}},
		"both token and method":     {card, CheckoutSessionPayRequest{PaymentToken: "tok", PaymentMethodID: method, NameOnCard: "A", Country: "US", Zip: "10001"}},
		"someone else's method":     {card, CheckoutSessionPayRequest{PaymentMethodID: other}},
		"malformed method id":       {card, CheckoutSessionPayRequest{PaymentMethodID: "pm_nope"}},
		"new card without a name":   {card, CheckoutSessionPayRequest{PaymentToken: "tok", Country: "US", Zip: "10001"}},
		"new card bad country":      {card, CheckoutSessionPayRequest{PaymentToken: "tok", NameOnCard: "A", Country: "USA", Zip: "10001"}},
		"new card missing postal":   {card, CheckoutSessionPayRequest{PaymentToken: "tok", NameOnCard: "A", Country: "DE"}},
		"new card bad US postal":    {card, CheckoutSessionPayRequest{PaymentToken: "tok", NameOnCard: "A", Country: "US", Zip: "1000"}},
		"oversized token":           {card, CheckoutSessionPayRequest{PaymentToken: strings.Repeat("t", 4097), NameOnCard: "A", Country: "US", Zip: "10001"}},
		"card token on a redirect":  {redirect, CheckoutSessionPayRequest{PaymentToken: "tok"}},
		"another settlement token":  {solana, CheckoutSessionPayRequest{TokenSymbol: "SOL"}},
		"saved method on solana":    {solana, CheckoutSessionPayRequest{PaymentMethodID: method}},
		"a driver no page can run":  {Option{CheckoutSessionOption: CheckoutSessionOption{Driver: "elements"}}, CheckoutSessionPayRequest{PaymentMethodID: method}},
		"a token on stripe":         {stripe, CheckoutSessionPayRequest{PaymentToken: "tok"}},
		"a new card on stripe":      {stripe, CheckoutSessionPayRequest{}},
		"another card on stripe":    {stripe, CheckoutSessionPayRequest{PaymentMethodID: other}},
		"solana option, no binding": {Option{CheckoutSessionOption: CheckoutSessionOption{Driver: "solana_pay"}}, CheckoutSessionPayRequest{}},
	} {
		_, _, err := Payment(tc.option, tc.in, "buyer@example.test", owns)
		require.ErrorIs(t, err, ErrInvalid, name)
	}

	_, _, err = Payment(card, CheckoutSessionPayRequest{PaymentToken: "tok", NameOnCard: "A", Country: "QA"}, "", owns)
	require.NoError(t, err, "a postal code is not demanded where none exists")

	// A PSP whose card_entry is server takes the card itself (#1129).
	server := Option{CheckoutSessionOption: CheckoutSessionOption{ID: "option_server", Rail: "nmi", Driver: "card", PSPID: "psp-4"}, Selector: "cards"}
	entered, err := cardguard.NewCard("4111111111111111", 10, 2027, "0739")
	require.NoError(t, err)
	defer entered.Zero()
	got, sent, err := Payment(server, CheckoutSessionPayRequest{Card: entered, NameOnCard: "A Buyer", Country: "US", Zip: "10001", LastFour: "0000"}, "", owns)
	require.NoError(t, err)
	require.Same(t, entered, sent)
	require.Empty(t, got.LastFour, "the card names itself")
	_, _, err = Payment(server, CheckoutSessionPayRequest{PaymentMethodID: method}, "", owns)
	require.NoError(t, err)
	for name, tc := range map[string]struct {
		option Option
		in     CheckoutSessionPayRequest
	}{
		"a token for a card page":     {server, CheckoutSessionPayRequest{PaymentToken: "tok", NameOnCard: "A", Country: "US", Zip: "10001"}},
		"card and a saved method":     {server, CheckoutSessionPayRequest{Card: entered, PaymentMethodID: method}},
		"a card for a tokenizing PSP": {card, CheckoutSessionPayRequest{Card: entered, NameOnCard: "A", Country: "US", Zip: "10001"}},
		"a card on a redirect":        {redirect, CheckoutSessionPayRequest{Card: entered}},
		"a card on solana":            {solana, CheckoutSessionPayRequest{Card: entered, TokenSymbol: "USDC"}},
		"a card without a name":       {server, CheckoutSessionPayRequest{Card: entered, Country: "US", Zip: "10001"}},
	} {
		_, _, err := Payment(tc.option, tc.in, "", owns)
		require.ErrorIs(t, err, ErrInvalid, name)
	}
}

func TestDrivable(t *testing.T) {
	for driver, ok := range map[string]bool{"collect_js": true, "card": true, "stripe_elements": true, "redirect": true, "solana_pay": true, "elements": false, "": false} {
		require.Equal(t, ok, Drivable(driver), driver)
	}
}
