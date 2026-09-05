// Package vault provides the live HashiCorp Vault adapters for OpenRails'
// per-merchant secret store (issue #251). It implements two interfaces declared
// elsewhere, so the rest of the codebase never imports hashicorp/vault/api:
//
//   - merchants.VaultKV  — KV-v2 backend for the per-merchant secret store
//     (merchants.NewVaultSecretStore wraps it; addressing is unchanged).
//   - solana.TransitClient — Vault Transit sign-as-a-service for the
//     non-extractable per-merchant Solana key.
//
// Merchant isolation is enforced by the (merchant, name) addressing in the
// merchants layer; this package authenticates ONCE as the OpenRails process
// (AppRole / K8s) — see auth.go — and is the trusted broker.
package vault

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	vaultapi "github.com/hashicorp/vault/api"
)

// KVv2Adapter implements merchants.VaultKV over a KV-v2 mount.
//
// The merchant store passes FULL logical paths that already include the mount
// (e.g. "secret/openrails/merchants/<id>/<name>"). KV-v2's HTTP API addresses data
// at "<mount>/data/<rest>" and metadata at "<mount>/metadata/<rest>", so this
// adapter strips the mount prefix and re-inserts the right segment per operation.
// Stored values live under the KV-v2 "data" envelope.
type KVv2Adapter struct {
	client *vaultapi.Client
	mount  string

	// onPermissionDenied is called with every error a KV operation observes,
	// letting a wired Supervisor decide (via NotifyPermissionDenied) whether
	// it signals real token death (#751 task 5). Nil (the zero value / a bare
	// NewKVv2Adapter) is a valid no-op — tests and one-off root/dedicated-
	// container clients never need it.
	onPermissionDenied func(error)
}

// NewKVv2Adapter builds a KV-v2 adapter for the given mount (e.g. "secret").
func NewKVv2Adapter(client *vaultapi.Client, mount string) *KVv2Adapter {
	return &KVv2Adapter{client: client, mount: strings.Trim(strings.TrimSpace(mount), "/")}
}

// WithReauthTrigger wires the adapter so every failed KV operation reports
// its error to sup.NotifyPermissionDenied (#751 task 5): a permission-denied
// response whose self-lookup confirms the token is dead triggers an
// immediate re-auth instead of waiting out the current lease or the
// merchant-secret cache TTL. Returns the adapter for chaining at
// construction (e.g. merchantsecrets/store.go:
// NewKVv2Adapter(client, mount).WithReauthTrigger(sup)). A nil sup is a no-op.
func (a *KVv2Adapter) WithReauthTrigger(sup *Supervisor) *KVv2Adapter {
	if sup != nil {
		a.onPermissionDenied = sup.NotifyPermissionDenied
	}
	return a
}

func (a *KVv2Adapter) notifyErr(err error) {
	if err != nil && a.onPermissionDenied != nil {
		a.onPermissionDenied(err)
	}
}

// rest strips the leading "<mount>/" the merchant store prepended.
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
func (a *KVv2Adapter) ReadSecret(ctx context.Context, path string) (map[string]string, int, error) {
	sec, err := a.client.Logical().ReadWithContext(ctx, a.dataPath(path))
	if err != nil {
		a.notifyErr(err)
		return nil, 0, fmt.Errorf("vault kv read: %w", err)
	}
	if sec == nil || sec.Data == nil {
		return nil, 0, nil // not found
	}
	inner, ok := sec.Data["data"].(map[string]any)
	if !ok || inner == nil {
		return nil, 0, nil
	}
	out := make(map[string]string, len(inner))
	for k, v := range inner {
		if s, ok := v.(string); ok {
			out[k] = s
		}
	}
	return out, kvResponseVersion(sec.Data["metadata"]), nil
}

// kvResponseVersion extracts KV-v2's current version from a data-read metadata
// block or a write response. 0 when absent (older mounts / malformed).
func kvResponseVersion(raw any) int {
	meta, ok := raw.(map[string]any)
	if ok {
		raw = meta["version"]
	}
	if n, ok := raw.(json.Number); ok {
		if v, err := n.Int64(); err == nil {
			return int(v)
		}
	}
	return 0
}

func (a *KVv2Adapter) WriteSecret(ctx context.Context, path string, data map[string]string) (int, error) {
	payload := make(map[string]any, len(data))
	for k, v := range data {
		payload[k] = v
	}
	sec, err := a.client.Logical().WriteWithContext(ctx, a.dataPath(path), map[string]any{"data": payload})
	if err != nil {
		a.notifyErr(err)
		return 0, fmt.Errorf("vault kv write: %w", err)
	}
	if sec != nil {
		return kvResponseVersion(sec.Data), nil
	}
	return 0, nil
}

// DeleteSecret purges ALL versions via the metadata endpoint (idempotent).
func (a *KVv2Adapter) DeleteSecret(ctx context.Context, path string) error {
	if _, err := a.client.Logical().DeleteWithContext(ctx, a.metadataPath(path)); err != nil {
		a.notifyErr(err)
		return fmt.Errorf("vault kv delete: %w", err)
	}
	return nil
}

// ListSecrets enumerates LEAF secret names under path (never values), relative
// to path, recursively descending KV-v2 directory entries ("name/"). KV-v2 LIST
// returns one level only, but the merchant store's List contract (matching the
// DB store, #724 backend parity) is full relative names such as
// "psps/<rail>/<env>/<acct>/<key>" — without recursion the
// status surfaces would see only "psps/" and report every
// credential unconfigured.
func (a *KVv2Adapter) ListSecrets(ctx context.Context, path string) ([]string, error) {
	return a.listSecrets(ctx, strings.TrimSuffix(path, "/"), "", 0)
}

// maxListDepth bounds the recursive descent (canonical secret names are ≤5
// segments; this is a loop guard, not a contract).
const maxListDepth = 16

func (a *KVv2Adapter) listSecrets(ctx context.Context, root, rel string, depth int) ([]string, error) {
	if depth > maxListDepth {
		return nil, fmt.Errorf("vault kv list: exceeded max depth %d under %q", maxListDepth, root)
	}
	full := root
	if rel != "" {
		full = root + "/" + rel
	}
	sec, err := a.client.Logical().ListWithContext(ctx, a.metadataPath(full))
	if err != nil {
		a.notifyErr(err)
		return nil, fmt.Errorf("vault kv list: %w", err)
	}
	if sec == nil || sec.Data == nil {
		return nil, nil
	}
	raw, _ := sec.Data["keys"].([]any)
	var names []string
	for _, k := range raw {
		s, ok := k.(string)
		if !ok || s == "" {
			continue
		}
		child := strings.TrimSuffix(s, "/")
		if rel != "" {
			child = rel + "/" + child
		}
		// A KV-v2 path can be both a leaf and a directory; LIST returns both
		// "name" and "name/" entries, so each case is handled independently.
		if strings.HasSuffix(s, "/") {
			sub, err := a.listSecrets(ctx, root, child, depth+1)
			if err != nil {
				return nil, err
			}
			names = append(names, sub...)
			continue
		}
		names = append(names, child)
	}
	return names, nil
}

// BackendIdentity identifies the configured Vault address and namespace without
// credentials. Cleanup refuses a later configuration pointed at another backend.
func (a *KVv2Adapter) BackendIdentity() string {
	if a.client == nil {
		return ""
	}
	return strings.TrimRight(a.client.Address(), "/") + "|" + a.client.Namespace()
}
