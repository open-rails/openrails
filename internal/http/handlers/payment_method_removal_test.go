package handlers

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/stretchr/testify/require"
)

func TestPaymentMethodRemovalFenceHealth(t *testing.T) {
	for _, test := range []struct {
		reason  string
		pending bool
	}{
		{"", false},
		{"account_updater", false},
		{"removed:00000000-0000-4000-8000-000000000001", false},
		{"delete:00000000-0000-4000-8000-000000000001", true},
	} {
		t.Run(test.reason, func(t *testing.T) {
			out := PaymentMethodToAPI(&models.PaymentMethod{
				Status: "active", ParkReason: test.reason,
				Card: models.Card{ExpMonth: 12, ExpYear: 2099},
			}, nil, nil, time.Now())
			require.Equal(t, test.reason == "", out.Health.Active)
			raw, err := json.Marshal(out)
			require.NoError(t, err)
			var wire map[string]any
			require.NoError(t, json.Unmarshal(raw, &wire))
			require.NotContains(t, string(raw), "delete:")
		})
	}
}

func TestPaymentMethodListsOnlyCurrentUsage(t *testing.T) {
	active, canceled := uuid.New(), uuid.New()
	out := PaymentMethodToAPI(&models.PaymentMethod{Subscriptions: []*models.Subscription{
		{ID: active, Status: models.StatusActive},
		{ID: canceled, Status: models.StatusCanceled},
	}}, nil, nil, time.Now())
	require.Len(t, out.Subscriptions, 1)
	require.Equal(t, active, out.Subscriptions[0].ID.UUID())
}
