package openrails

import (
	"context"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
)

// ExportNotificationOrdering snapshots the merchant's account-inbox ordering
// for a billing-archive move; RestoreNotificationOrdering takes it on the
// destination. It never reads the inbox or replays the event log.
func (c *LocalClient) ExportNotificationOrdering(ctx context.Context) (billing.NotificationOrderingSnapshot, error) {
	e, merchant, err := c.local()
	if err != nil {
		return billing.NotificationOrderingSnapshot{}, err
	}
	if _, err := e.LiveMerchant(ctx, merchant); err != nil {
		return billing.NotificationOrderingSnapshot{}, err
	}
	return subscriptions.ExportNotificationOrdering(ctx, e.App.Runtime.DB, merchant)
}

// RestoreNotificationOrdering restores the separate notification ordering
// snapshot after moving/restoring a merchant's billing archive. It is trusted
// local administration, never a frontend operation.
func (c *LocalClient) RestoreNotificationOrdering(ctx context.Context, snapshot billing.NotificationOrderingSnapshot) error {
	e, merchant, err := c.local()
	if err != nil {
		return err
	}
	if _, err := e.LiveMerchant(ctx, merchant); err != nil {
		return err
	}
	return subscriptions.RestoreNotificationOrdering(ctx, e.App.Runtime.DB, merchant, snapshot)
}
