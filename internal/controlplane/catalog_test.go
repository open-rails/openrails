package controlplane

import (
	"testing"
	"time"

	"github.com/open-rails/authkit"
)

func TestGroups_CustomerExposesRemoteApplications(t *testing.T) {
	for _, group := range Groups() {
		if group.Name == CustomerType {
			if !group.Capabilities.RemoteApplications {
				t.Fatal("customer groups must expose remote_application registration")
			}
			return
		}
	}
	t.Fatal("customer group persona missing")
}

func TestAnyLiveAPIKey(t *testing.T) {
	now := time.Now()
	revoked := &now
	if anyLiveAPIKey(nil) {
		t.Error("nil API keys should not be live")
	}
	if anyLiveAPIKey([]authkit.APIKey{{RevokedAt: revoked}}) {
		t.Error("only-revoked API keys should not count as live")
	}
	if !anyLiveAPIKey([]authkit.APIKey{{RevokedAt: nil}}) {
		t.Error("a non-revoked API key should count as live")
	}
	if !anyLiveAPIKey([]authkit.APIKey{{RevokedAt: revoked}, {RevokedAt: nil}}) {
		t.Error("a mix with one live API key should count as live")
	}
}

// TestAdmissionCreatePermission_GateSemantics proves the admission hot-path gate:
// an API key WITHOUT merchant:admissions:create fails the admission gate while
// still passing customer writes. (The merchant owner's default grant is proven
// end-to-end in the bootstrap integration test.)
func TestAdmissionCreatePermission_GateSemantics(t *testing.T) {
	// A customer-write key without admission authority fails the spend gate.
	writeOnly := &ResolvedServiceCredential{Permissions: []string{PermMerchantCustomerSettingsUpdate}}
	if writeOnly.HasPermission(PermMerchantAdmissionsCreate) {
		t.Fatalf("merchant:customer-settings:update must NOT imply merchant:admissions:create")
	}
	if !writeOnly.HasPermission(PermMerchantCustomerSettingsUpdate) {
		t.Fatalf("write-only API key must still pass the write gate")
	}

	// An API key holding merchant:admissions:create passes the spend gate.
	spender := &ResolvedServiceCredential{Permissions: []string{PermMerchantCustomerSettingsUpdate, PermMerchantAdmissionsCreate}}
	if !spender.HasPermission(PermMerchantAdmissionsCreate) {
		t.Fatalf("API key holding %q must pass the spend gate", PermMerchantAdmissionsCreate)
	}

	admin := &ResolvedServiceCredential{Permissions: []string{"root:*"}}
	if admin.HasPermission(PermMerchantAdmissionsCreate) {
		t.Fatalf("foreign-persona apex grant %q must not bypass merchant spend gate", "root:*")
	}
}
