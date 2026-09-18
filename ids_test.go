package openrails

import (
	"encoding/json"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// Every typed id has one wire spelling: its prefix plus the canonical UUID.
// A bare UUID, another kind's prefix, or a malformed body is refused; the
// zero id is "" both ways.
func TestTypedIDsHaveOneWireSpelling(t *testing.T) {
	u := uuid.MustParse("0198f3a4-6f1e-7c2b-9d4e-1f2a3b4c5d6e")
	bare := u.String()
	kinds := []struct {
		name, prefix string
		parse        func(string) (wireID, error)
		of           func(uuid.UUID) wireID
	}{
		{"product", "prod_", func(s string) (wireID, error) { return ParseProductID(s) }, func(u uuid.UUID) wireID { return ProductID(u) }},
		{"price", "price_", func(s string) (wireID, error) { return ParsePriceID(s) }, func(u uuid.UUID) wireID { return PriceID(u) }},
		{"subscription", "sub_", func(s string) (wireID, error) { return ParseSubscriptionID(s) }, func(u uuid.UUID) wireID { return SubscriptionID(u) }},
		{"payment", "pay_", func(s string) (wireID, error) { return ParsePaymentID(s) }, func(u uuid.UUID) wireID { return PaymentID(u) }},
		{"payment method", "pm_", func(s string) (wireID, error) { return ParsePaymentMethodID(s) }, func(u uuid.UUID) wireID { return PaymentMethodID(u) }},
		{"checkout session", "cs_", func(s string) (wireID, error) { return ParseCheckoutSessionID(s) }, func(u uuid.UUID) wireID { return CheckoutSessionID(u) }},
		{"customer", "", func(s string) (wireID, error) { return ParseCustomerID(s) }, func(u uuid.UUID) wireID { return CustomerID(u) }},
	}
	for _, kind := range kinds {
		t.Run(kind.name, func(t *testing.T) {
			id := kind.of(u)
			require.Equal(t, kind.prefix+bare, id.String())
			require.False(t, id.IsZero())
			parsed, err := kind.parse(" " + kind.prefix + bare + " ")
			require.NoError(t, err)
			require.Equal(t, id, parsed)
			zero, err := kind.parse("")
			require.NoError(t, err)
			require.True(t, zero.IsZero())
			require.Equal(t, "", zero.String())
			for _, wrong := range []string{"other_" + bare, kind.prefix + "not-a-uuid", kind.prefix + bare + "x", kind.prefix + uuid.Nil.String(), kind.prefix + "0198F3A46F1E7C2B9D4E1F2A3B4C5D6E"} {
				_, err := kind.parse(wrong)
				require.Error(t, err, wrong)
			}
			if kind.prefix != "" {
				_, err := kind.parse(bare)
				require.Error(t, err, "a bare uuid is not a %s id", kind.name)
			} else {
				_, err := kind.parse("cus_" + bare)
				require.Error(t, err)
			}
		})
	}
}

// JSON round-trips the typed spelling, refuses a wrong kind, and omitzero
// drops the zero id from requests.
func TestTypedIDsJSON(t *testing.T) {
	u := uuid.MustParse("0198f3a4-6f1e-7c2b-9d4e-1f2a3b4c5d6e")
	type doc struct {
		Price    PriceID         `json:"price_id"`
		Optional SubscriptionID  `json:"subscription_id,omitzero"`
		Nullable *PaymentID      `json:"payment_id"`
		Method   PaymentMethodID `json:"payment_method_id"`
	}
	raw, err := json.Marshal(doc{Price: PriceID(u)})
	require.NoError(t, err)
	require.JSONEq(t, `{"price_id":"price_`+u.String()+`","payment_id":null,"payment_method_id":""}`, string(raw))
	var back doc
	require.NoError(t, json.Unmarshal(raw, &back))
	require.Equal(t, PriceID(u), back.Price)
	require.True(t, back.Method.IsZero())
	require.Error(t, json.Unmarshal([]byte(`{"price_id":"prod_`+u.String()+`"}`), &back), "a product id is not a price id")
	require.Error(t, json.Unmarshal([]byte(`{"price_id":"`+u.String()+`"}`), &back), "a bare uuid is not a price id")
	keyed := map[CustomerID][]string{CustomerID(u): {"pro"}}
	raw, err = json.Marshal(keyed)
	require.NoError(t, err)
	require.JSONEq(t, `{"`+u.String()+`":["pro"]}`, string(raw))
}
