package controlplane

import (
	"testing"

	authcore "github.com/open-rails/authkit/core"

	"github.com/open-rails/openrails/internal/dbtest"
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
	for _, p := range CatalogNames() {
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

func TestValidateAPIKeyResourcesRejectsLegacyPayableKinds(t *testing.T) {
	legacyKinds := []string{
		"customer_id",
		"payer_account_id",
		"account_id",
		"delegated_user_id",
		"subject_type",
		"openrails.payer_account",
		"openrails.account",
		"openrails.delegated_user",
	}
	for _, kind := range legacyKinds {
		t.Run(kind, func(t *testing.T) {
			err := validateAPIKeyResources(dbtest.TestMerchantID, []authcore.APIKeyResource{
				MerchantResource(dbtest.TestMerchantID),
				{Kind: kind, ID: "legacy"},
			})
			if err != ErrServiceCredentialScopeDenied {
				t.Fatalf("validateAPIKeyResources(%q) error = %v, want %v", kind, err, ErrServiceCredentialScopeDenied)
			}
		})
	}
}

// Service-JWT authority is no longer an intersection of a requested permission
// set against a server-side grant. Registering an issuer to a merchant grants that
// merchant full authority over its own resources; the self-signed token's claims
// are authoritative, bounded only by validateAPIKeyResources (own-merchant
// scope). The former intersectPermissions/intersectResources tests were removed
// with that logic.
