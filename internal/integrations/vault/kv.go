// Package vault provides the live HashiCorp Vault adapters for OpenRails'
// per-tenant secret store (issue #251). It implements two interfaces declared
// elsewhere, so the rest of the codebase never imports hashicorp/vault/api:
//
//   - tenancy.VaultKV  — KV-v2 backend for the per-tenant secret store
//     (tenancy.NewVaultSecretStore wraps it; addressing is unchanged).
//   - solana.TransitClient — Vault Transit sign-as-a-service for the
//     non-extractable per-tenant Solana key.
//
// Tenant isolation is enforced by the (tenant, name) addressing in the tenancy
// layer; this package authenticates ONCE as the OpenRails process (AppRole / K8s)
// — see auth.go — and is the trusted broker.
package vault

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/open-rails/openrails/internal/observability"
	vaultapi "github.com/hashicorp/vault/api"
)

// KVv2Adapter implements tenancy.VaultKV over a KV-v2 mount.
//
// The tenancy store passes FULL logical paths that already include the mount
// (e.g. "secret/openrails/tenants/<id>/<name>"). KV-v2's HTTP API addresses data
// at "<mount>/data/<rest>" and metadata at "<mount>/metadata/<rest>", so this
// adapter strips the mount prefix and re-inserts the right segment per operation.
// Stored values live under the KV-v2 "data" envelope.
type KVv2Adapter struct {
	client *vaultapi.Client
	mount  string
}

// NewKVv2Adapter builds a KV-v2 adapter for the given mount (e.g. "secret").
func NewKVv2Adapter(client *vaultapi.Client, mount string) *KVv2Adapter {
	return &KVv2Adapter{client: client, mount: strings.Trim(strings.TrimSpace(mount), "/")}
}

// rest strips the leading "<mount>/" the tenancy store prepended.
func (a *KVv2Adapter) rest(full string) string {
	return strings.TrimPrefix(strings.TrimPrefix(full, a.mount), "/")
}

func (a *KVv2Adapter) dataPath(full string) string {
	return a.mount + "/data/" + a.rest(full)
}

func (a *KVv2Adapter) metadataPath(full string) string {
	return a.mount + "/metadata/" + a.rest(full)
}

// ReadSecret returns the stored key/value map at path, or (nil, nil) when the
// secret does not exist (the tenancy layer maps a missing "value" to
// ErrSecretNotFound). A transport/permission error propagates so callers can fail
// closed and distinguish "Vault unreachable" (retry) from "absent" (terminal).
func (a *KVv2Adapter) ReadSecret(ctx context.Context, path string) (map[string]string, error) {
	start := time.Now()
	defer func() {
		if m, ok := ctx.Value("otel.meter").(*observability.Meter); ok {
			m.Latency.Record(ctx, time.Since(start).Seconds())
		}
	}()

	sec, err := a.client.Logical().ReadWithContext(ctx, a.dataPath(path))
	if err != nil {
		return nil, fmt.Errorf("vault kv read: %w", err)
	}
	if sec == nil || sec.Data == nil {
		return nil, nil // not found
	}
	inner, ok := sec.Data["data"].(map[string]any)
	if !ok || inner == nil {
		return nil, nil
	}
	out := make(map[string]string, len(inner))
	for k, v := range inner {
		if s, ok := v.(string); ok {
			out[k] = s
		}
	}
	return out, nil
}

// WriteSecret purges ALL versions via the metadata endpoint (idempotent).
func (a *KVv2Adapter) WriteSecret(ctx context.Context, path string, data map[string]string) error {
	start := time.Now()
	defer func() {
		if m, ok := ctx.Value("otel.meter").(*observability.Meter); ok {
			m.Latency.Record(ctx, time.Since(start).Seconds())
		}
	}()

	payload := make(map[string]any, len(data))
	for k, v := range data {
		payload[k] = v
	}
	_, err := a.client.Logical().WriteWithContext(ctx, a.dataPath(path), map[string]any{"data": payload})
	if err != nil {
		return fmt.Errorf("vault kv write: %w", err)
	}
	return nil
}

// DeleteSecret purges ALL versions via the metadata endpoint (idempotent).
func (a *KVv2Adapter) DeleteSecret(ctx context.Context, path string) error {
	start := time.Now()
	defer func() {
		if m, ok := ctx.Value("otel.meter").(*observability.Meter); ok {
			m.Latency.Record(ctx, time.Since(start).Seconds())
		}
	}()

	if _, err := a.client.Logical().DeleteWithContext(ctx, a.metadataPath(path)); err != nil {
		return fmt.Errorf("vault kv delete: %w", err)
	}
	return nil
}

// ListSecrets enumerates child names under path (never values), via metadata.
func (a *KVv2Adapter) ListSecrets(ctx context.Context, path string) ([]string, error) {
	start := time.Now()
	defer func() {
		if m, ok := ctx.Value("otel.meter").(*observability.Meter); ok {
			m.Latency.Record(ctx, time.Since(start).Seconds())
		}
	}()

	sec, err := a.client.Logical().ListWithContext(ctx, a.metadataPath(path))
	if err != nil {
		return nil, fmt.Errorf("vault kv list: %w", err)
	}
	if sec == nil || sec.Data == nil {
		return nil, nil
	}
	raw, _ := sec.Data["keys"].([]any)
	names := make([]string, 0, len(raw))
	for _, k := range raw {
		if s, ok := k.(string); ok {
			names = append(names, s)
		}
	}
	return names, nil
}
