package controlplane

// BND-C1 regression: service-JWT permissions must be intersected against
// stored authority; a token cannot self-assert permissions that were never
// granted (e.g. root:*).

import (
	"testing"
)

func TestIntersectPermissions_EmptyStoredDeniesAll(t *testing.T) {
	// Stored authority is empty → no permissions survive, regardless of what
	// the token claims.
	claimed := []string{"root:*", PermMerchantCustomerSettingsRead, PermMerchantCustomerSettingsUpdate}
	got := intersectPermissions(claimed, nil)
	if len(got) != 0 {
		t.Errorf("intersectPermissions with empty stored = %v, want empty", got)
	}
}

func TestIntersectPermissions_EmptyClaimedYieldsEmpty(t *testing.T) {
	stored := []string{PermMerchantCustomerSettingsRead, PermMerchantCustomerSettingsUpdate}
	got := intersectPermissions(nil, stored)
	if len(got) != 0 {
		t.Errorf("intersectPermissions with empty claimed = %v, want empty", got)
	}
}

func TestIntersectPermissions_OnlyGrantedPermissionsSurvive(t *testing.T) {
	stored := []string{PermMerchantCustomerSettingsRead}
	claimed := []string{PermMerchantCustomerSettingsRead, PermMerchantCustomerSettingsUpdate, "root:*"}
	got := intersectPermissions(claimed, stored)
	if len(got) != 1 || got[0] != PermMerchantCustomerSettingsRead {
		t.Errorf("intersectPermissions = %v, want [%q]", got, PermMerchantCustomerSettingsRead)
	}
}

func TestIntersectPermissions_AdminNotGrantedUnlessStored(t *testing.T) {
	// The critical BND-C1 case: a token self-asserts root:* but the
	// remote application's stored authority only grants merchant customer reads.
	// The owner-grant claim must be stripped.
	stored := []string{PermMerchantCustomerSettingsRead}
	claimed := []string{"root:*"}
	got := intersectPermissions(claimed, stored)
	if len(got) != 0 {
		t.Errorf("self-asserted admin survived intersection: got %v, want empty", got)
	}
}

func TestIntersectPermissions_AdminGrantedWhenStored(t *testing.T) {
	// If an operator EXPLICITLY grants root:* to a remote_application
	// (unusual but valid), the token may claim it and it should pass through.
	stored := []string{"root:*", PermMerchantCustomerSettingsRead}
	claimed := []string{"root:*"}
	got := intersectPermissions(claimed, stored)
	if len(got) != 1 || got[0] != "root:*" {
		t.Errorf("explicitly stored admin should survive: got %v", got)
	}
}

func TestIntersectPermissions_OrderFollowsClaimed(t *testing.T) {
	stored := []string{"b", "a", "c"}
	claimed := []string{"c", "a"}
	got := intersectPermissions(claimed, stored)
	if len(got) != 2 || got[0] != "c" || got[1] != "a" {
		t.Errorf("intersectPermissions order = %v, want [c a]", got)
	}
}

func TestIntersectPermissions_NoDuplicates(t *testing.T) {
	// cleanPermissionList handles dedup before intersection; this verifies
	// intersectPermissions doesn't re-introduce dupes from the stored side.
	stored := []string{"a", "a"}
	claimed := []string{"a"}
	got := intersectPermissions(claimed, stored)
	if len(got) != 1 {
		t.Errorf("intersectPermissions produced duplicates: %v", got)
	}
}
