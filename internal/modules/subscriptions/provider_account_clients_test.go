package subscriptions

import (
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/db/models"
)

func TestPaymentMethodMatchesSubscriptionProvider(t *testing.T) {
	accountA := uuid.MustParse("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")
	accountB := uuid.MustParse("bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb")

	require.True(t, PaymentMethodMatchesSubscriptionProvider(
		&models.PaymentMethod{ProviderAccountID: &accountA},
		&models.Subscription{ProviderAccountID: &accountA},
	))
	require.False(t, PaymentMethodMatchesSubscriptionProvider(
		&models.PaymentMethod{ProviderAccountID: &accountB},
		&models.Subscription{ProviderAccountID: &accountA},
	))
	require.True(t, PaymentMethodMatchesSubscriptionProvider(
		&models.PaymentMethod{},
		&models.Subscription{ProviderAccountID: &accountA},
	))
}
