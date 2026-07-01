package vault

import (
	"context"
	"fmt"

	vaultapi "github.com/hashicorp/vault/api"
)

// Capabilities is what OpenRails' OWN Vault token is allowed to do on the paths it
// uses (#661). Derived from sys/capabilities-self at startup. ADVISORY ONLY — it
// drives feature-gating and boot diagnostics, never authorization (Vault's runtime
// 403 stays the real boundary; a gated-on feature may still 403 if policy changes).
type Capabilities struct {
	KVRead  bool // can read merchant secrets from the KV mount
	KVWrite bool // can create/update merchant secrets (config-push surface)
	Transit bool // can sign + read the pubkey for Solana Vault Transit
}

// SelfCapabilities probes what the client's token may do on the KV and Transit
// paths OpenRails uses. Transit is probed against the documented `openrails-*` key
// convention; a policy scoped to that prefix (or wider) matches. Never derive
// authorization from this — only hide/degrade features.
func SelfCapabilities(ctx context.Context, client *vaultapi.Client, kvMount, transitMount string) (Capabilities, error) {
	kvPath := kvMount + "/data/openrails/probe"
	transitSign := transitMount + "/sign/openrails-probe"
	transitKeys := transitMount + "/keys/openrails-probe"

	caps, err := selfCapabilitiesOnPaths(ctx, client, []string{kvPath, transitSign, transitKeys})
	if err != nil {
		return Capabilities{}, err
	}
	return Capabilities{
		KVRead:  hasCap(caps[kvPath], "read"),
		KVWrite: hasCap(caps[kvPath], "create") || hasCap(caps[kvPath], "update"),
		Transit: hasCap(caps[transitSign], "update") && hasCap(caps[transitKeys], "read"),
	}, nil
}

func selfCapabilitiesOnPaths(ctx context.Context, client *vaultapi.Client, paths []string) (map[string][]string, error) {
	secret, err := client.Logical().WriteWithContext(ctx, "sys/capabilities-self", map[string]any{"paths": paths})
	if err != nil {
		return nil, fmt.Errorf("vault: sys/capabilities-self: %w", err)
	}
	if secret == nil || secret.Data == nil {
		return nil, fmt.Errorf("vault: sys/capabilities-self returned no data")
	}
	out := make(map[string][]string, len(paths))
	for _, p := range paths {
		out[p] = capsFor(secret.Data[p])
	}
	return out, nil
}

func capsFor(v any) []string {
	list, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(list))
	for _, item := range list {
		if s, ok := item.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// hasCap reports whether the capability list grants `want`. A root token reports
// ["root"] on every path, which grants everything.
func hasCap(caps []string, want string) bool {
	for _, c := range caps {
		if c == want || c == "root" {
			return true
		}
	}
	return false
}
