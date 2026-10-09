//go:build e2e && integration

package entitlements_test

import (
	"context"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
)

// revokeAccess takes a product-access window back.
func revokeAccess(client *openrails.Client, ctx context.Context, id billing.ProductAccessID) error {
	return client.RevokeProductAccess(ctx, id, billing.RevokeProductAccessParams{Reason: "test"})
}
