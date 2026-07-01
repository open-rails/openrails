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
//
// Only KV is probed: its paths are OpenRails-owned addressing (<mount>/data/openrails/*),
// so the probe is exact. Transit key names are operator-chosen (no naming convention),
// so transit access is verified against the REAL key at provision time
// (solanaTransitPublicKey) and by the runtime 403 — never by path-probing here.
type Capabilities struct {
	KVRead  bool // can read merchant secrets from the KV mount
	KVWrite bool // can create/update merchant secrets (config-push surface)
}

// SelfCapabilities probes what the client's token may do on the KV paths OpenRails
// uses. Never derive authorization from this — only hide/degrade features.
func SelfCapabilities(ctx context.Context, client *vaultapi.Client, kvMount string) (Capabilities, error) {
	kvPath := kvMount + "/data/openrails/probe"
	caps, err := selfCapabilitiesOnPaths(ctx, client, []string{kvPath})
	if err != nil {
		return Capabilities{}, err
	}
	return Capabilities{
		KVRead:  hasCap(caps[kvPath], "read"),
		KVWrite: hasCap(caps[kvPath], "create") || hasCap(caps[kvPath], "update"),
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
