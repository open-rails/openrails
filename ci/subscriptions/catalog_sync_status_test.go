//go:build e2e && integration

package subscriptions_test

import (
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/nmimock"
)

// Verifying a provider price reads its provider again: an NMI plan's terms
// still matching are in_sync, terms the provider changed are drift (the
// provider's terms win on the next apply), and CCBill, which has no read API,
// is sync_disabled, never in_sync.
func TestProviderPriceVerifyReadsItsProvider(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	c := w.client[embedded]
	product, err := c.CreateProduct(t.Context(), billing.CreateProductParams{Key: "provider-sync-" + uuid.NewString()[:8], DisplayName: "Provider sync"})
	require.NoError(t, err)
	hours := monthHours
	flexform, err := c.CreateProviderPrice(t.Context(), billing.CreateProviderPriceParams{ProductID: product.ID, Key: "ccbill", PSP: "ccbill", FlexID: ccbillFlexID, RecurringBillingOptionID: ccbillRBO, UnitAmount: new(int64(9_990_000)), Currency: "USD", BillingIntervalHours: &hours})
	require.NoError(t, err)
	verified, err := c.GetPrice(t.Context(), flexform.ID, billing.GetPriceParams{Verify: true})
	require.NoError(t, err)
	require.Equal(t, billing.SyncStatusSyncDisabled, verified.Provider.SyncStatus)

	plan := "sync_plan_" + uuid.NewString()[:8]
	w.nmi.AddPlan(nmimock.Plan{ID: plan, Name: "Sync", Amount: "9.99", Days: 30})
	legacy, err := c.CreateProviderPrice(t.Context(), billing.CreateProviderPriceParams{ProductID: product.ID, Key: "nmi", PSP: "nmi", PlanID: plan})
	require.NoError(t, err)
	verified, err = c.GetPrice(t.Context(), legacy.ID, billing.GetPriceParams{Verify: true})
	require.NoError(t, err)
	require.Equal(t, billing.SyncStatusInSync, verified.Provider.SyncStatus)
	w.nmi.AddPlan(nmimock.Plan{ID: plan, Name: "Sync", Amount: "12.99", Days: 30})
	verified, err = c.GetPrice(t.Context(), legacy.ID, billing.GetPriceParams{Verify: true})
	require.NoError(t, err)
	require.Equal(t, billing.SyncStatusDrifted, verified.Provider.SyncStatus)
	require.Equal(t, "unit_amount", verified.Provider.Drift[0].Field)
}
