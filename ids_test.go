package openrails

import (
	"encoding/json"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// Each typed id has one wire spelling (prefix + canonical UUID); the zero id is "".
func TestTypedIDsHaveOneWireSpelling(t *testing.T) {
	u := uuid.MustParse("0198f3a4-6f1e-7c2b-9d4e-1f2a3b4c5d6e")
	bare := u.String()
	kinds := []struct {
		prefix string
		parse  func(string) (wireID, error)
		of     func(uuid.UUID) wireID
	}{
		{CatalogIDPrefix, func(s string) (wireID, error) { return ParseCatalogID(s) }, func(u uuid.UUID) wireID { return CatalogID(u) }},
		{ProductIDPrefix, func(s string) (wireID, error) { return ParseProductID(s) }, func(u uuid.UUID) wireID { return ProductID(u) }},
		{PriceIDPrefix, func(s string) (wireID, error) { return ParsePriceID(s) }, func(u uuid.UUID) wireID { return PriceID(u) }},
		{SubscriptionIDPrefix, func(s string) (wireID, error) { return ParseSubscriptionID(s) }, func(u uuid.UUID) wireID { return SubscriptionID(u) }},
		{PaymentIDPrefix, func(s string) (wireID, error) { return ParsePaymentID(s) }, func(u uuid.UUID) wireID { return PaymentID(u) }},
		{PaymentMethodIDPrefix, func(s string) (wireID, error) { return ParsePaymentMethodID(s) }, func(u uuid.UUID) wireID { return PaymentMethodID(u) }},
		{CheckoutSessionIDPrefix, func(s string) (wireID, error) { return ParseCheckoutSessionID(s) }, func(u uuid.UUID) wireID { return CheckoutSessionID(u) }},
		{"", func(s string) (wireID, error) { return ParseCustomerID(s) }, func(u uuid.UUID) wireID { return CustomerID(u) }},
	}
	for _, kind := range kinds {
		id := kind.of(u)
		require.Equal(t, kind.prefix+bare, id.String())
		require.False(t, id.IsZero())
		parsed, err := kind.parse(" " + kind.prefix + bare + " ")
		require.NoError(t, err)
		require.Equal(t, id, parsed)
		zero, err := kind.parse("")
		require.NoError(t, err)
		require.True(t, zero.IsZero())
		require.Empty(t, zero.String())
		require.True(t, kind.of(uuid.Nil).IsZero())
		wrong := []string{"other_" + bare, kind.prefix + "not-a-uuid", kind.prefix + bare + "x", kind.prefix + uuid.Nil.String(),
			kind.prefix + "0198f3a46f1e7c2b9d4e1f2a3b4c5d6e", kind.prefix + "{" + bare + "}", kind.prefix + "urn:uuid:" + bare}
		for _, other := range kinds {
			if other.prefix != kind.prefix {
				wrong = append(wrong, other.prefix+bare)
			}
		}
		for _, s := range wrong {
			_, err := kind.parse(s)
			require.Error(t, err, "%q accepted for prefix %q", s, kind.prefix)
		}
	}
}

// JSON uses the typed spelling, refuses another kind, and omitzero drops the zero id.
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
	require.Equal(t, doc{Price: PriceID(u)}, back)
	for _, bad := range []string{`{"price_id":"prod_` + u.String() + `"}`, `{"price_id":"` + u.String() + `"}`, `{"price_id":7}`} {
		require.Error(t, json.Unmarshal([]byte(bad), &back), bad)
	}
	raw, err = json.Marshal(map[CustomerID][]string{CustomerID(u): {"pro"}})
	require.NoError(t, err)
	require.JSONEq(t, `{"`+u.String()+`":["pro"]}`, string(raw))
}

func TestSourceRefSpellsTheSourceKind(t *testing.T) {
	u := uuid.New()
	for sourceType, want := range map[string]string{
		"subscription": SubscriptionID(u).String(), "grace": SubscriptionID(u).String(),
		"one_off": PaymentID(u).String(), "purchase": PaymentID(u).String(), "admin": u.String(),
	} {
		require.Equal(t, want, SourceRef(sourceType, u.String()), sourceType)
	}
	require.Equal(t, "host-grant-7", SourceRef("subscription", "host-grant-7"))
}
