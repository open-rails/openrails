package openrails

import (
	"github.com/open-rails/openrails/billing"
	"github.com/stretchr/testify/require"
	"strings"
	"testing"
)

func TestNotificationConsumerName(t *testing.T) {
	client := &Client{}
	for _, name := range []string{"contentkit.site", "saas-inbox", "a_b", "a", strings.Repeat("a", 64)} {
		handle, err := client.NotificationEvents(name)
		require.NoError(t, err, name)
		require.NotNil(t, handle)
	}
	for _, name := range []string{"", ".", "..", "a/b", "a%2Fb", "a b", strings.Repeat("a", 65)} {
		_, err := client.NotificationEvents(name)
		require.Error(t, err, name)
	}
}

func TestNotificationFactClassification(t *testing.T) {
	for _, kind := range []string{"premium_renewed", "one_off_purchase_completed", "usage_charged"} {
		require.True(t, billing.CustomerNotificationOptional(kind, billing.NotificationData{}), kind)
	}
	require.False(t, billing.CustomerNotificationOptional("premium_renewed", billing.NotificationData{DowngradeApplied: true}))
	for _, kind := range []string{"premium_started", "subscription_changed", "subscription_reprice_scheduled", "payment_method_failed", "future_kind"} {
		require.False(t, billing.CustomerNotificationOptional(kind, billing.NotificationData{}), kind)
	}
	require.True(t, billing.CustomerNotificationActionRequested("order_overdue"))
	require.False(t, billing.CustomerNotificationActionRequested("future_kind"))
}
