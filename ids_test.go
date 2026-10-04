package openrails

import (
	"encoding/json"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/billing"
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
		{billing.CatalogIDPrefix, func(s string) (wireID, error) { return billing.ParseCatalogID(s) }, func(u uuid.UUID) wireID { return billing.CatalogID(u) }},
		{billing.ProductIDPrefix, func(s string) (wireID, error) { return billing.ParseProductID(s) }, func(u uuid.UUID) wireID { return billing.ProductID(u) }},
		{billing.PriceIDPrefix, func(s string) (wireID, error) { return billing.ParsePriceID(s) }, func(u uuid.UUID) wireID { return billing.PriceID(u) }},
		{billing.SubscriptionIDPrefix, func(s string) (wireID, error) { return billing.ParseSubscriptionID(s) }, func(u uuid.UUID) wireID { return billing.SubscriptionID(u) }},
		{billing.PaymentIDPrefix, func(s string) (wireID, error) { return billing.ParsePaymentID(s) }, func(u uuid.UUID) wireID { return billing.PaymentID(u) }},
		{billing.PaymentMethodIDPrefix, func(s string) (wireID, error) { return billing.ParsePaymentMethodID(s) }, func(u uuid.UUID) wireID { return billing.PaymentMethodID(u) }},
		{billing.CheckoutAttemptIDPrefix, func(s string) (wireID, error) { return billing.ParseCheckoutAttemptID(s) }, func(u uuid.UUID) wireID { return billing.CheckoutAttemptID(u) }},
		{billing.PaymentAttemptIDPrefix, func(s string) (wireID, error) { return billing.ParsePaymentAttemptID(s) }, func(u uuid.UUID) wireID { return billing.PaymentAttemptID(u) }},
		{billing.RebillCycleIDPrefix, func(s string) (wireID, error) { return billing.ParseRebillCycleID(s) }, func(u uuid.UUID) wireID { return billing.RebillCycleID(u) }},
		{"", func(s string) (wireID, error) { return billing.ParseCustomerID(s) }, func(u uuid.UUID) wireID { return billing.CustomerID(u) }},
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
		Price    billing.PriceID         `json:"price_id"`
		Optional billing.SubscriptionID  `json:"subscription_id,omitzero"`
		Nullable *billing.PaymentID      `json:"payment_id"`
		Method   billing.PaymentMethodID `json:"payment_method_id"`
	}
	raw, err := json.Marshal(doc{Price: billing.PriceID(u)})
	require.NoError(t, err)
	require.JSONEq(t, `{"price_id":"price_`+u.String()+`","payment_id":null,"payment_method_id":""}`, string(raw))
	var back doc
	require.NoError(t, json.Unmarshal(raw, &back))
	require.Equal(t, doc{Price: billing.PriceID(u)}, back)
	for _, bad := range []string{`{"price_id":"prod_` + u.String() + `"}`, `{"price_id":"` + u.String() + `"}`, `{"price_id":7}`} {
		require.Error(t, json.Unmarshal([]byte(bad), &back), bad)
	}
	raw, err = json.Marshal(map[billing.CustomerID][]string{billing.CustomerID(u): {"pro"}})
	require.NoError(t, err)
	require.JSONEq(t, `{"`+u.String()+`":["pro"]}`, string(raw))
}

func TestSourceRefSpellsTheSourceKind(t *testing.T) {
	u := uuid.New()
	for sourceType, want := range map[string]string{
		"subscription": billing.SubscriptionID(u).String(), "grace": billing.SubscriptionID(u).String(),
		"one_off": billing.PaymentID(u).String(), "purchase": billing.PaymentID(u).String(), "admin": u.String(),
	} {
		require.Equal(t, want, billing.SourceRef(sourceType, u.String()), sourceType)
	}
	require.Equal(t, "host-grant-7", billing.SourceRef("subscription", "host-grant-7"))
}
