//go:build e2e && integration

package subscriptions_test

import (
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/billing"
)

// A provider OpenRails cannot read reports its link sync_disabled when the
// price is verified, never in_sync.
func TestUnreadableProviderLinkIsSyncDisabled(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	c := w.client[embedded]
	product, err := c.CreateProduct(t.Context(), billing.CreateProductParams{Key: "ccbill-sync-" + uuid.NewString()[:8], DisplayName: "CCBill sync"})
	require.NoError(t, err)
	hours := monthHours
	price, err := c.CreatePrice(t.Context(), billing.CreatePriceParams{ProductID: product.ID, Key: "monthly", UnitAmount: 9_990_000, Currency: "USD", BillingIntervalHours: &hours, AccessDurationHours: &hours,
		PSPLinks: map[string]map[string]string{"ccbill": {"form_name": ccbillFormName, "flex_id": ccbillFlexID, "recurring_billing_option_id": ccbillRBO}}})
	require.NoError(t, err)

	verified, err := c.GetPrice(t.Context(), price.ID, billing.GetPriceParams{Verify: true})
	require.NoError(t, err)
	require.Contains(t, verified.PSPs, "ccbill")
	require.Equal(t, billing.SyncStatusSyncDisabled, verified.PSPs["ccbill"].SyncStatus)
}
