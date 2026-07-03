//go:build integration

// Live Vault capability-probe integration tests (#661). They run unconditionally
// under the integration tag against the vaulttest dev-mode container (#724);
// VAULT_ADDR + VAULT_TOKEN reuse an external dev server instead.
//
// They mint real scoped child tokens (transit-only, KV read-write, KV read-only)
// and assert SelfCapabilities reports exactly what each policy grants — the
// security-critical determination that drives #661 feature-gating.
package vault

import (
	"context"
	"testing"

	"github.com/open-rails/openrails/internal/integrations/vault/vaulttest"
)

func TestSelfCapabilitiesRootSeesEverything(t *testing.T) {
	root := vaulttest.RootClient(t)
	vaulttest.EnsureTransit(t)
	caps, err := SelfCapabilities(context.Background(), root, "secret")
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if !caps.KVRead || !caps.KVWrite {
		t.Fatalf("root caps = %+v, want all true", caps)
	}
}

// A transit-only policy (key names are operator-chosen — any name works) grants
// zero KV capability: the #661 least-privilege deployment.
func TestSelfCapabilitiesTransitOnly(t *testing.T) {
	vaulttest.EnsureTransit(t)
	cl := vaulttest.ClientWithPolicy(t, "openrails-transit-only", `
path "transit/sign/my-mainnet-signer" { capabilities = ["update"] }
path "transit/keys/my-mainnet-signer" { capabilities = ["read"] }
`)
	caps, err := SelfCapabilities(context.Background(), cl, "secret")
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if caps.KVRead || caps.KVWrite {
		t.Fatalf("transit-only token has KV caps: %+v", caps)
	}
}

func TestSelfCapabilitiesKVReadWriteThenReadOnly(t *testing.T) {
	rw := vaulttest.ClientWithPolicy(t, "openrails-kv-rw", vaulttest.PolicyKVReadWrite)
	caps, err := SelfCapabilities(context.Background(), rw, "secret")
	if err != nil {
		t.Fatalf("probe rw: %v", err)
	}
	if !caps.KVRead || !caps.KVWrite {
		t.Fatalf("rw token missing KV caps: %+v", caps)
	}

	ro := vaulttest.ClientWithPolicy(t, "openrails-kv-ro", vaulttest.PolicyKVReadOnly)
	caps, err = SelfCapabilities(context.Background(), ro, "secret")
	if err != nil {
		t.Fatalf("probe ro: %v", err)
	}
	if !caps.KVRead || caps.KVWrite {
		t.Fatalf("read-only token caps = %+v, want KVRead && !KVWrite", caps)
	}
}
