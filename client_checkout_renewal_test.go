package openrails

import (
	"testing"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/billing"
	"github.com/stretchr/testify/require"
)

func TestClientCheckoutRetainsOrderRenewalChoice(t *testing.T) {
	for _, choice := range []struct {
		name  string
		value *bool
	}{
		{"default", nil},
		{"renew", new(true)},
		{"initial term only", new(false)},
	} {
		t.Run(choice.name, func(t *testing.T) {
			client, seen := recordingRemote(t, nil)
			customer := billing.CheckoutCustomerIdentity{ID: billing.CustomerID(uuid.New())}
			price := billing.PriceID(uuid.New())
			_, err := client.CreateCheckoutSession(t.Context(), billing.CreateCheckoutSessionParams{
				Customer: customer, PriceID: price, AutoRenew: choice.value,
			})
			require.NoError(t, err)
			for range 1 {
				request := <-seen
				if choice.value == nil {
					require.NotContains(t, request.body, "auto_renew", "omitted choice must retain the server default")
				} else {
					require.Equal(t, *choice.value, request.body["auto_renew"], "explicit false must survive serialization")
				}
			}
		})
	}
}
