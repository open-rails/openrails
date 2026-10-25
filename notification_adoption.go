package openrails

import (
	"context"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
)

// ExportNotificationAdoption produces the exact legacy-origin mapping and
// source ordering snapshot for an explicit account-inbox cutover. Stop source
// writers and the old importer until destination adoption is committed. It
// never reads the destination inbox or replays the general event log.
func (c *LocalClient) ExportNotificationAdoption(ctx context.Context) (billing.NotificationAdoption, error) {
	if _, err := c.engine.LiveMerchant(ctx, c.merchantID); err != nil {
		return billing.NotificationAdoption{}, err
	}
	return subscriptions.ExportNotificationAdoption(ctx, c.engine.App.Runtime.DB, c.merchantID, c.engine.App.Runtime.Clock.Now().UTC())
}

// RestoreNotificationOrdering restores the separate notification ordering
// snapshot after moving/restoring a merchant's billing archive. It is trusted
// local administration, never a frontend operation.
func (c *LocalClient) RestoreNotificationOrdering(ctx context.Context, snapshot billing.NotificationAdoption) error {
	if _, err := c.engine.LiveMerchant(ctx, c.merchantID); err != nil {
		return err
	}
	return subscriptions.RestoreNotificationOrdering(ctx, c.engine.App.Runtime.DB, c.merchantID, snapshot)
}
