package guide

import (
	"context"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
)

// PageOps sends medium and higher findings to a Slack channel.
func PageOps(ctx context.Context, shop *openrails.Client, url string) error {
	// guide:alert-webhook
	_, err := shop.CreateAlertWebhook(ctx, billing.CreateAlertWebhookParams{
		Name:   "ops",
		URL:    url, // write-only: reads show only its host
		Format: billing.AlertWebhookSlack,
	})
	return err
	// guide:end
}
