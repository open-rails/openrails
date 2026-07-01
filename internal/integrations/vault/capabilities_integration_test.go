//go:build vaultint

// Live Vault capability-probe integration tests (#661). Run against a real Vault:
//
//	vault server -dev -dev-root-token-id=root &
//	export VAULT_ADDR=http://127.0.0.1:8200 VAULT_TOKEN=root
//	go test -tags vaultint -run SelfCapabilities ./internal/integrations/vault/...
//
// They mint real scoped child tokens (transit-only, KV read-write, KV read-only)
// and assert SelfCapabilities reports exactly what each policy grants — the
// security-critical determination that drives #661 feature-gating.
package vault

import (
	"context"
	"os"
	"strings"
	"testing"

	vaultapi "github.com/hashicorp/vault/api"
)

func capRootClient(t *testing.T) *vaultapi.Client {
	t.Helper()
	addr, token := os.Getenv("VAULT_ADDR"), os.Getenv("VAULT_TOKEN")
	if addr == "" || token == "" {
		t.Skip("set VAULT_ADDR + VAULT_TOKEN to run live Vault capability tests")
	}
	cfg := vaultapi.DefaultConfig()
	cfg.Address = addr
	cl, err := vaultapi.NewClient(cfg)
	if err != nil {
		t.Fatalf("vault client: %v", err)
	}
	cl.SetToken(token)
	return cl
}

func ensureTransit(t *testing.T, root *vaultapi.Client) {
	t.Helper()
	_, err := root.Logical().WriteWithContext(context.Background(), "sys/mounts/transit", map[string]any{"type": "transit"})
	if err != nil && !strings.Contains(err.Error(), "already in use") {
		t.Fatalf("enable transit: %v", err)
	}
}

// tokenWithPolicy writes an ACL policy and returns a client using a fresh child
// token bound only to that policy.
func tokenWithPolicy(t *testing.T, root *vaultapi.Client, name, hcl string) *vaultapi.Client {
	t.Helper()
	if err := root.Sys().PutPolicy(name, hcl); err != nil {
		t.Fatalf("put policy %s: %v", name, err)
	}
	secret, err := root.Auth().Token().Create(&vaultapi.TokenCreateRequest{
		Policies: []string{name},
		NoParent: true,
	})
	if err != nil {
		t.Fatalf("create token for %s: %v", name, err)
	}
	cfg := vaultapi.DefaultConfig()
	cfg.Address = root.Address()
	cl, err := vaultapi.NewClient(cfg)
	if err != nil {
		t.Fatalf("child client: %v", err)
	}
	cl.SetToken(secret.Auth.ClientToken)
	return cl
}

func TestSelfCapabilitiesRootSeesEverything(t *testing.T) {
	root := capRootClient(t)
	ensureTransit(t, root)
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
	root := capRootClient(t)
	ensureTransit(t, root)
	cl := tokenWithPolicy(t, root, "openrails-transit-only", `
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
	root := capRootClient(t)
	ensureTransit(t, root)

	rw := tokenWithPolicy(t, root, "openrails-kv-rw", `
path "secret/data/openrails/*" { capabilities = ["create","read","update","delete"] }
`)
	caps, err := SelfCapabilities(context.Background(), rw, "secret")
	if err != nil {
		t.Fatalf("probe rw: %v", err)
	}
	if !caps.KVRead || !caps.KVWrite {
		t.Fatalf("rw token missing KV caps: %+v", caps)
	}

	ro := tokenWithPolicy(t, root, "openrails-kv-ro", `
path "secret/data/openrails/*" { capabilities = ["read"] }
`)
	caps, err = SelfCapabilities(context.Background(), ro, "secret")
	if err != nil {
		t.Fatalf("probe ro: %v", err)
	}
	if !caps.KVRead || caps.KVWrite {
		t.Fatalf("read-only token caps = %+v, want KVRead && !KVWrite", caps)
	}
}
