package controlplane

import (
	"testing"
)

func TestResolvedServiceCredential_HasPermission(t *testing.T) {
	r := &ResolvedServiceCredential{Permissions: []string{PermMerchantCustomerSettingsRead, PermMerchantCustomerSettingsUpdate}}

	if !r.HasPermission(PermMerchantCustomerSettingsUpdate) {
		t.Errorf("expected granted permission %q to be held", PermMerchantCustomerSettingsUpdate)
	}
	if r.HasPermission(PermMerchantCatalogUpdate) {
		t.Errorf("did not expect ungranted permission %q to be held", PermMerchantCatalogUpdate)
	}
}

func TestResolvedServiceCredential_ApexGrantDoesNotBypassPermissionChecks(t *testing.T) {
	r := &ResolvedServiceCredential{Permissions: []string{"root:*"}}
	for _, p := range []string{
		PermMerchantSettingsRead, PermMerchantAdmissionsCreate,
		PermMerchantPaymentsRefund, PermCustomerBalanceRead,
		PermCustomerSpendDelegationsUpdate,
	} {
		if r.HasPermission(p) {
			t.Errorf("apex grant %q must not bypass exact check for %q", "root:*", p)
		}
	}
}

func TestResolvedServiceCredential_EmptyDeniesAll(t *testing.T) {
	r := &ResolvedServiceCredential{}
	if r.HasPermission(PermMerchantCustomerSettingsRead) {
		t.Error("empty API key should grant nothing")
	}
}

func TestControlPlane_TokenPrefix_NilSafe(t *testing.T) {
	var c *ControlPlane
	if got := c.TokenPrefix(); got != APIKeyPrefix {
		t.Errorf("nil control plane TokenPrefix() = %q, want %q", got, APIKeyPrefix)
	}
	if !c.LooksLikeAPIKey(APIKeyPrefix + "_st_key_secret") {
		t.Error("API key prefix should be fixed even for nil control plane")
	}
}

// #569 (hard cut): validateAPIKeyResources and the API-key resource-scope concept
// were removed. A key's merchant identity is the permission group it was minted
// under, so there is nothing to validate against a resource scope and the legacy
// "reject payable-kind resources" test was deleted with that logic.

// Service-JWT authority is no longer an intersection of a requested permission
// set against a server-side grant. Registering an issuer to a merchant grants that
// merchant full authority over its own resources; the self-signed token's claims
// are authoritative. The former intersectPermissions/intersectResources tests were
// removed with that logic.
