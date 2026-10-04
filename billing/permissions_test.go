package billing_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/billing"
)

// The step-up policy: merchant operations that move money, grant access or
// mint credentials; never reads or a buyer's own billing.
func TestRequiresRecentSignIn(t *testing.T) {
	for perm, want := range map[string]bool{
		billing.MerchantCustomerSettingsUpdate: true, billing.MerchantPaymentsRefund: true,
		billing.MerchantPSPsUpdate: true, billing.MerchantCatalogUpdate: true,
		billing.MerchantCreditsGrant: true, billing.MerchantBillingImport: true,
		billing.MerchantBillingExport: true, billing.MerchantCredentialsManage: true,
		billing.MerchantMembersManage: true, billing.MerchantAccessGrantPermanent: true,
		billing.MerchantPaymentsRead: false, billing.MerchantCatalogOwnRead: false,
		billing.MerchantDashboardUpdate: false, billing.MerchantHostEventsAcknowledge: false,
	} {
		require.Equal(t, want, billing.RequiresRecentSignIn(perm), perm)
	}
}
