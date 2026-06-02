package vault

import (
	"context"
	"encoding/base64"
	"fmt"
	"strings"

	vaultapi "github.com/hashicorp/vault/api"
)

// TransitAdapter implements solana.TransitClient over a Vault Transit mount. The
// per-tenant Ed25519 key is created non-extractable (exportable=false) so it
// NEVER leaves Vault; OpenRails only ever requests signatures.
type TransitAdapter struct {
	client *vaultapi.Client
	mount  string
}

// NewTransitAdapter builds a Transit adapter for the given mount (e.g. "transit").
func NewTransitAdapter(client *vaultapi.Client, mount string) *TransitAdapter {
	return &TransitAdapter{client: client, mount: strings.Trim(strings.TrimSpace(mount), "/")}
}

// Sign returns the raw 64-byte Ed25519 signature for input over key `name`.
// Vault Ed25519 signs the raw message (prehashed=false; hash_algorithm ignored),
// which is what Solana verifies over.
func (t *TransitAdapter) Sign(ctx context.Context, name string, input []byte) ([]byte, error) {
	res, err := t.client.Logical().WriteWithContext(ctx, t.mount+"/sign/"+name, map[string]any{
		"input":     base64.StdEncoding.EncodeToString(input),
		"prehashed": false,
	})
	if err != nil {
		return nil, fmt.Errorf("vault transit sign: %w", err)
	}
	if res == nil || res.Data == nil {
		return nil, fmt.Errorf("vault transit sign: empty response")
	}
	raw, _ := res.Data["signature"].(string) // "vault:v1:<base64>"
	i := strings.LastIndex(raw, ":")
	if i < 0 {
		return nil, fmt.Errorf("vault transit sign: malformed signature %q", raw)
	}
	sig, err := base64.StdEncoding.DecodeString(raw[i+1:])
	if err != nil {
		return nil, fmt.Errorf("vault transit sign: decode signature: %w", err)
	}
	return sig, nil
}

// PublicKey returns the raw 32-byte Ed25519 public key for the latest version of
// key `name` (which is the tenant's Solana address as base58).
func (t *TransitAdapter) PublicKey(ctx context.Context, name string) ([]byte, error) {
	res, err := t.client.Logical().ReadWithContext(ctx, t.mount+"/keys/"+name)
	if err != nil {
		return nil, fmt.Errorf("vault transit read key: %w", err)
	}
	if res == nil || res.Data == nil {
		return nil, fmt.Errorf("vault transit key %q not found", name)
	}
	keys, ok := res.Data["keys"].(map[string]any)
	if !ok || len(keys) == 0 {
		return nil, fmt.Errorf("vault transit key %q has no versions", name)
	}
	latest := fmt.Sprint(res.Data["latest_version"])
	entry, ok := keys[latest].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("vault transit key %q missing latest version %s", name, latest)
	}
	b64, _ := entry["public_key"].(string)
	pub, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return nil, fmt.Errorf("vault transit decode public key: %w", err)
	}
	return pub, nil
}
