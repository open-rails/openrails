package subscriptions

import (
	"testing"

	"github.com/open-rails/openrails/internal/db/models"
	"github.com/stretchr/testify/require"
)

func TestSubscriptionCardViewOnlyShowsUsableMethod(t *testing.T) {
	for _, tt := range []struct {
		status, parked string
		visible        bool
	}{
		{"active", "", true},
		{"removed", "removed:receipt", false},
		{"closed", "", false},
		{"replaced", "", false},
		{"active", "delete:receipt", false},
	} {
		t.Run(tt.status+tt.parked, func(t *testing.T) {
			method := &models.PaymentMethod{Status: tt.status, ParkReason: tt.parked, Card: models.Card{Brand: "visa", Last4: "4242"}}
			card := subscriptionCardView(method)
			if tt.visible {
				require.NotNil(t, card)
			} else {
				require.Nil(t, card)
			}
			require.Equal(t, "4242", method.Card.Last4, "presentation never erases historical metadata")
		})
	}
	require.Nil(t, subscriptionCardView(nil))
}
