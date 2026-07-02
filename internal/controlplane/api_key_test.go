package controlplane

import (
	"testing"
)

// HasPermission semantics (exact match, glob bounds, apex-grant non-bypass,
// empty-denies-all) are pinned for BOTH credential shapes by the
// perm_glob_test.go matrix; this file keeps only the nil-receiver contract.

func TestControlPlane_TokenPrefix_NilSafe(t *testing.T) {
	var c *ControlPlane
	if got := c.TokenPrefix(); got != APIKeyPrefix {
		t.Errorf("nil control plane TokenPrefix() = %q, want %q", got, APIKeyPrefix)
	}
	if !c.LooksLikeAPIKey(APIKeyPrefix + "_st_key_secret") {
		t.Error("API key prefix should be fixed even for nil control plane")
	}
}
