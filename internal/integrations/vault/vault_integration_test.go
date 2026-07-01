//go:build vaultint

// Live Vault integration tests (no mocks). They run against a real Vault when
// VAULT_ADDR + VAULT_TOKEN are set — e.g. a dev server:
//
//	vault server -dev -dev-root-token-id=root &
//	export VAULT_ADDR=http://127.0.0.1:8200 VAULT_TOKEN=root
//	vault secrets enable -version=2 -path=secret kv   # (dev mode pre-enables this)
//	vault secrets enable transit
//	go test -tags vaultint -run Vault ./internal/integrations/vault/...
//
// They exercise the REAL KV-v2 put/get/list/delete + merchant path isolation and
// real Transit Ed25519 create/sign/verify against the boundary OpenRails uses.
package vault

import (
	"context"
	"crypto/ed25519"
	"os"
	"testing"
	"time"

	vaultapi "github.com/hashicorp/vault/api"

	solanaint "github.com/open-rails/openrails/internal/integrations/solana"
	"github.com/open-rails/openrails/pkg/merchant"
)

func liveClient(t *testing.T) *vaultapi.Client {
	t.Helper()
	addr := os.Getenv("VAULT_ADDR")
	token := os.Getenv("VAULT_TOKEN")
	if addr == "" || token == "" {
		t.Skip("set VAULT_ADDR + VAULT_TOKEN to run live Vault integration tests")
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

func TestVaultKVRoundTripAndIsolation(t *testing.T) {
	ctx := context.Background()
	kv := NewKVv2Adapter(liveClient(t), "secret")

	pathA := "secret/openrails/merchants/tenant-a/provider_accounts/solana/live/AKnL4NNf3DGWZJS6cPknBuEGnVsV4A4m5tgebLHaRSZ9/private_key"
	pathB := "secret/openrails/merchants/tenant-b/provider_accounts/solana/live/9hSR6S7WPtxmTojgo6GG3k4yDPecgJY292j7xrsUGWBu/private_key"

	if err := kv.WriteSecret(ctx, pathA, map[string]string{"value": "keyA"}); err != nil {
		t.Fatalf("write A: %v", err)
	}
	if err := kv.WriteSecret(ctx, pathB, map[string]string{"value": "keyB"}); err != nil {
		t.Fatalf("write B: %v", err)
	}
	defer kv.DeleteSecret(ctx, pathA)
	defer kv.DeleteSecret(ctx, pathB)

	got, err := kv.ReadSecret(ctx, pathA)
	if err != nil {
		t.Fatalf("read A: %v", err)
	}
	if got["value"] != "keyA" {
		t.Fatalf("merchant A value = %q, want keyA (isolation/round-trip broken)", got["value"])
	}

	missing, err := kv.ReadSecret(ctx, "secret/openrails/merchants/tenant-a/does/not/exist")
	if err != nil {
		t.Fatalf("read missing: %v", err)
	}
	if len(missing) != 0 {
		t.Fatalf("missing secret returned %v, want empty (maps to ErrSecretNotFound)", missing)
	}
}

func TestVaultTransitEd25519SignVerify(t *testing.T) {
	ctx := context.Background()
	client := liveClient(t)
	transit := NewTransitAdapter(client, "transit")

	name := "openrails-it-" + time.Now().Format("150405")
	// Create a non-extractable Ed25519 key.
	if _, err := client.Logical().WriteWithContext(ctx, "transit/keys/"+name, map[string]any{
		"type":       "ed25519",
		"exportable": false,
	}); err != nil {
		t.Skipf("transit engine unavailable (enable it): %v", err)
	}
	defer client.Logical().DeleteWithContext(ctx, "transit/keys/"+name+"/config") //nolint

	pub, err := transit.PublicKey(ctx, name)
	if err != nil {
		t.Fatalf("transit public key: %v", err)
	}
	if len(pub) != 32 {
		t.Fatalf("public key len = %d, want 32", len(pub))
	}

	msg := []byte("transfer_subscription message")
	sig, err := transit.Sign(ctx, name, msg)
	if err != nil {
		t.Fatalf("transit sign: %v", err)
	}
	if len(sig) != 64 {
		t.Fatalf("signature len = %d, want 64", len(sig))
	}
	if !ed25519.Verify(ed25519.PublicKey(pub), msg, sig) {
		t.Fatal("Vault Transit Ed25519 signature did not verify — encoding mismatch")
	}

	signer := solanaint.NewTransitSigner(transit, func(merchant.ID) string { return name }, 0)
	signerPub, err := signer.PublicKey(ctx, merchant.ID{})
	if err == nil {
		t.Fatalf("zero merchant PublicKey err = nil, pub=%s", signerPub)
	}
	tid := merchant.ID([16]byte{1})
	signerPub, err = signer.PublicKey(ctx, tid)
	if err != nil {
		t.Fatalf("solana transit signer public key: %v", err)
	}
	if string(signerPub.Bytes()) != string(pub) {
		t.Fatalf("solana transit signer public key mismatch")
	}
	signerSig, err := signer.SignMessage(ctx, tid, msg)
	if err != nil {
		t.Fatalf("solana transit signer sign: %v", err)
	}
	if !ed25519.Verify(ed25519.PublicKey(pub), msg, signerSig[:]) {
		t.Fatal("Solana signer over Vault Transit produced an unverifiable signature")
	}
}
