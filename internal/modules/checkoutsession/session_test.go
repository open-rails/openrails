package checkoutsession

import (
	"github.com/google/uuid"
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
	details := func(name, country, postal, line1 string) *billing.BillingDetails {
		value := func(v string) *string {
			if v == "" {
				return nil
			}
			return &v
		}
		return &billing.BillingDetails{Name: value(name), Address: &billing.BillingAddress{Line1: value(line1), PostalCode: value(postal), Country: value(country)}}
	}
	psp := billing.PSPID(uuid.New())
	card := Option{CheckoutSessionOption: CheckoutSessionOption{ID: "option_card", Rail: "nmi", Mode: "subscription", Driver: "collect_js", PSPID: psp}, Selector: "cards"}
	redirect := Option{CheckoutSessionOption: CheckoutSessionOption{ID: "option_redirect", Rail: "ccbill", Driver: "redirect", PSPID: billing.PSPID(uuid.New())}, Selector: "ccbill"}
	stripe := Option{CheckoutSessionOption: CheckoutSessionOption{ID: "option_stripe", Rail: "stripe", Mode: "subscription", Driver: "stripe_elements", PSPID: billing.PSPID(uuid.New())}, Selector: "stripe"}
	solana := Option{CheckoutSessionOption: CheckoutSessionOption{ID: "option_sol", Rail: "solana", Driver: "solana_pay", PublicConfig: map[string]string{"token_symbol": "usdc"}, PSPID: billing.PSPID(uuid.New())}, Selector: "solana"}
	method := billing.PaymentMethodID{1}
	owns := func(id string) bool { return id == method.String() }
	newCard := PayCheckoutSessionParams{PaymentToken: " tok ", BillingDetails: details("A Buyer", "us", "10001", "1 Main St")}

	got, _, err := Payment(card, newCard, "buyer@example.test", owns)
	require.NoError(t, err)
	require.Equal(t, billing.CheckoutPaymentOptions{PSP: "cards", PaymentToken: "tok", BillingDetails: &billing.BillingDetails{
		Name: new("A Buyer"), Email: new("buyer@example.test"), Address: &billing.BillingAddress{PostalCode: new("10001"), Country: new("US")},
	}}, got, "the option binds the PSP, the account the email; the compact form carries no street")

	got, _, err = Payment(card, PayCheckoutSessionParams{PaymentMethodID: method, BillingDetails: details("Someone Else", "US", "99999", "")}, "", owns)
	require.NoError(t, err)
	require.Equal(t, billing.CheckoutPaymentOptions{PSP: "cards", PaymentMethodID: method}, got, "a saved card keeps its own billing identity")

	got, _, err = Payment(stripe, PayCheckoutSessionParams{PaymentMethodID: method, BillingDetails: details("Ignored", "", "99999", "")}, "buyer@example.test", owns)
	require.NoError(t, err)
	require.Equal(t, billing.CheckoutPaymentOptions{PSP: "stripe", PaymentMethodID: method, BillingDetails: &billing.BillingDetails{Email: new("buyer@example.test")}}, got, "a card saved in Stripe's fields")

	got, _, err = Payment(solana, PayCheckoutSessionParams{TokenSymbol: "USDC"}, "", owns)
	require.NoError(t, err)
	require.Equal(t, "USDC", got.TokenSymbol)
	require.Equal(t, "transfer_request", got.Flow)

	_, _, err = Payment(redirect, PayCheckoutSessionParams{BillingDetails: details("A Buyer", "US", "10001", "")}, "buyer@example.test", owns)
	require.NoError(t, err)

	other := billing.PaymentMethodID{2}
	for name, tc := range map[string]struct {
		option Option
		in     PayCheckoutSessionParams
	}{
		"neither token nor method":  {card, PayCheckoutSessionParams{}},
		"both token and method":     {card, PayCheckoutSessionParams{PaymentToken: "tok", PaymentMethodID: method, BillingDetails: details("A", "US", "10001", "")}},
		"someone else's method":     {card, PayCheckoutSessionParams{PaymentMethodID: other}},
		"new card without a name":   {card, PayCheckoutSessionParams{PaymentToken: "tok", BillingDetails: details("", "US", "10001", "")}},
		"new card bad country":      {card, PayCheckoutSessionParams{PaymentToken: "tok", BillingDetails: details("A", "USA", "10001", "")}},
		"new card missing postal":   {card, PayCheckoutSessionParams{PaymentToken: "tok", BillingDetails: details("A", "DE", "", "")}},
		"new card bad US postal":    {card, PayCheckoutSessionParams{PaymentToken: "tok", BillingDetails: details("A", "US", "1000", "")}},
		"oversized token":           {card, PayCheckoutSessionParams{PaymentToken: strings.Repeat("t", 4097), BillingDetails: details("A", "US", "10001", "")}},
		"card token on a redirect":  {redirect, PayCheckoutSessionParams{PaymentToken: "tok"}},
		"another settlement token":  {solana, PayCheckoutSessionParams{TokenSymbol: "SOL"}},
		"saved method on solana":    {solana, PayCheckoutSessionParams{PaymentMethodID: method}},
		"a driver no page can run":  {Option{CheckoutSessionOption: CheckoutSessionOption{Driver: "elements"}}, PayCheckoutSessionParams{PaymentMethodID: method}},
		"a token on stripe":         {stripe, PayCheckoutSessionParams{PaymentToken: "tok"}},
		"a new card on stripe":      {stripe, PayCheckoutSessionParams{}},
		"another card on stripe":    {stripe, PayCheckoutSessionParams{PaymentMethodID: other}},
		"solana option, no binding": {Option{CheckoutSessionOption: CheckoutSessionOption{Driver: "solana_pay"}}, PayCheckoutSessionParams{}},
	} {
		_, _, err := Payment(tc.option, tc.in, "buyer@example.test", owns)
		require.ErrorIs(t, err, ErrInvalid, name)
	}

	_, _, err = Payment(card, PayCheckoutSessionParams{PaymentToken: "tok", BillingDetails: details("A", "QA", "", "")}, "", owns)
	require.NoError(t, err, "a postal code is not demanded where none exists")

	// A PSP whose card_entry is server takes the card itself (#1129).
	server := Option{CheckoutSessionOption: CheckoutSessionOption{ID: "option_server", Rail: "nmi", Driver: "card", PSPID: billing.PSPID(uuid.New())}, Selector: "cards"}
	entered, err := cardguard.NewCard("4111111111111111", 10, 2027, "0739")
	require.NoError(t, err)
	defer entered.Zero()
	got, sent, err := Payment(server, PayCheckoutSessionParams{Card: entered, BillingDetails: details("A Buyer", "US", "10001", "")}, "", owns)
	require.NoError(t, err)
	require.Same(t, entered, sent)
	require.Equal(t, "A Buyer", *got.BillingDetails.Name)
	_, _, err = Payment(server, PayCheckoutSessionParams{PaymentMethodID: method}, "", owns)
	require.NoError(t, err)
	for name, tc := range map[string]struct {
		option Option
		in     PayCheckoutSessionParams
	}{
		"a token for a card page":     {server, PayCheckoutSessionParams{PaymentToken: "tok", BillingDetails: details("A", "US", "10001", "")}},
		"card and a saved method":     {server, PayCheckoutSessionParams{Card: entered, PaymentMethodID: method}},
		"a card for a tokenizing PSP": {card, PayCheckoutSessionParams{Card: entered, BillingDetails: details("A", "US", "10001", "")}},
		"a card on a redirect":        {redirect, PayCheckoutSessionParams{Card: entered}},
		"a card on solana":            {solana, PayCheckoutSessionParams{Card: entered, TokenSymbol: "USDC"}},
		"a card without a name":       {server, PayCheckoutSessionParams{Card: entered, BillingDetails: details("", "US", "10001", "")}},
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
