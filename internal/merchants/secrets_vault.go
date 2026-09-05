package merchants

import (
	"context"
	"errors"
	"fmt"
	"path"
	"strings"

	"github.com/open-rails/openrails/pkg/merchant"
)

// ErrVaultNotConfigured is returned by every vaultSecretStore operation until a
// live Vault client is wired in. The store is constructable (so managed wiring
// can reference it) but deliberately fails closed rather than silently returning
// empty secrets — a missing webhook signing secret must NEVER be treated as
// "verification disabled".
//
// It wraps ErrSecretBackendUnavailable: an unconfigured Vault is an OPERATIONAL
// (retryable) state, NOT a terminal "secret absent" — callers must never read it
// as "verification disabled".
var ErrVaultNotConfigured = fmt.Errorf("%w: vault-backed secret store is not configured (no live Vault client)", ErrSecretBackendUnavailable)

// VaultKV is the minimal KV-v2 surface the Vault-backed store needs. A real
// adapter satisfies it with hashicorp/vault/api (logical Read/Write/Delete/List)
// in a managed deployment. Keeping it an interface lets the store build and be
// unit-tested WITHOUT pulling in or running Vault.
type VaultKV interface {
	// ReadSecret returns the data map and the KV-v2 current version (0 unknown).
	ReadSecret(ctx context.Context, path string) (map[string]string, int, error)
	// WriteSecret returns the new KV-v2 version (0 unknown).
	WriteSecret(ctx context.Context, path string, data map[string]string) (int, error)
	DeleteSecret(ctx context.Context, path string) error
	ListSecrets(ctx context.Context, path string) ([]string, error)
}

// vaultSecretStore resolves the SAME (merchant, name) addressing as the DB-backed
// store to a merchant-scoped Vault KV path:
//
//	<mount>/openrails/merchants/<merchant-uuid>/<name>
//
// One merchant's secrets are therefore physically isolated under its own path
// prefix, and a Vault policy can grant a merchant operator read/write to ONLY its
// own subtree. The value field stored at each path is keyed "value".
//
// This adapter is a STUB: when client is nil (the default until managed wiring
// injects a real VaultKV) every operation returns ErrVaultNotConfigured. The DB-
// backed store remains the dev / self-hosted default, so nothing here is required
// to build or run the rest of OpenRails.
type vaultSecretStore struct {
	mount  string
	client VaultKV
}

// NewVaultSecretStore returns a Vault-backed MerchantSecretStore. mount is the KV-v2
// mount path (e.g. "secret"). client may be nil — in that case the store is a
// documented stub that fails closed with ErrVaultNotConfigured, which is the
// state until a managed deployment injects a live VaultKV.
func NewVaultSecretStore(mount string, client VaultKV) MerchantSecretStore {
	mount = strings.Trim(strings.TrimSpace(mount), "/")
	if mount == "" {
		mount = "secret"
	}
	return &vaultSecretStore{mount: mount, client: client}
}

// pathFor builds the merchant-scoped Vault path for a (merchant, name) pair.
func (v *vaultSecretStore) pathFor(merchantID merchant.ID, name string) string {
	return path.Join(v.mount, "openrails", "merchants", merchantID.String(), cleanSecretName(name))
}

func (v *vaultSecretStore) Get(ctx context.Context, tenantID merchant.ID, name string) (Secret, error) {
	if err := validateSecretRef(tenantID, name); err != nil {
		return Secret{}, err
	}
	if v.client == nil {
		return Secret{}, ErrVaultNotConfigured
	}
	vpath := v.pathFor(tenantID, name)
	data, version, err := v.client.ReadSecret(ctx, vpath)
	if err != nil {
		// A read error is an OPERATIONAL failure (Vault unreachable / sealed /
		// permission). Absence is signalled by missing "value" below, NOT by an
		// error — so any error here is retryable, never "secret absent".
		return Secret{}, fmt.Errorf("merchants: vault read %q: %w", name, errors.Join(ErrSecretBackendUnavailable, err))
	}
	val, ok := data["value"]
	if !ok {
		return Secret{}, ErrSecretNotFound
	}
	return Secret{Name: name, Value: val, Version: kvVersionOrOne(version)}, nil
}

// kvVersionOrOne keeps the pre-#724 "at least 1" contract when the mount
// reports no version metadata.
func kvVersionOrOne(v int) int {
	if v < 1 {
		return 1
	}
	return v
}

func (v *vaultSecretStore) Put(ctx context.Context, tenantID merchant.ID, name, value string) (Secret, error) {
	if err := validateSecretRef(tenantID, name); err != nil {
		return Secret{}, err
	}
	if v.client == nil {
		return Secret{}, ErrVaultNotConfigured
	}
	vpath := v.pathFor(tenantID, name)
	version, err := v.client.WriteSecret(ctx, vpath, map[string]string{"value": value})
	if err != nil {
		return Secret{}, fmt.Errorf("merchants: vault write %q: %w", name, errors.Join(ErrSecretBackendUnavailable, err))
	}
	return Secret{Name: name, Value: value, Version: kvVersionOrOne(version)}, nil
}

func (v *vaultSecretStore) Delete(ctx context.Context, tenantID merchant.ID, name string) error {
	if err := validateSecretRef(tenantID, name); err != nil {
		return err
	}
	if v.client == nil {
		return ErrVaultNotConfigured
	}
	vpath := v.pathFor(tenantID, name)
	if err := v.client.DeleteSecret(ctx, vpath); err != nil {
		return fmt.Errorf("merchants: vault delete %q: %w", name, errors.Join(ErrSecretBackendUnavailable, err))
	}
	return nil
}

func (v *vaultSecretStore) List(ctx context.Context, tenantID merchant.ID) ([]string, error) {
	if tenantID.IsZero() {
		return nil, validateSecretRef(tenantID, "x")
	}
	if v.client == nil {
		return nil, ErrVaultNotConfigured
	}
	prefix := v.pathFor(tenantID, "")
	names, err := v.client.ListSecrets(ctx, prefix)
	if err != nil {
		return nil, fmt.Errorf("merchants: vault list: %w", errors.Join(ErrSecretBackendUnavailable, err))
	}
	return names, nil
}

func (v *vaultSecretStore) cleanupTarget(id merchant.ID) (string, string, error) {
	if id.IsZero() {
		return "", "", validateSecretRef(id, "x")
	}
	backend, ok := v.client.(interface{ BackendIdentity() string })
	if !ok || backend.BackendIdentity() == "" {
		return "", "", fmt.Errorf("Vault cleanup requires a stable backend identity")
	}
	return "vault:" + backend.BackendIdentity(), v.pathFor(id, ""), nil
}
