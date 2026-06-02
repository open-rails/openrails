package tenancy

import (
	"context"
	"errors"
	"fmt"
	"path"
	"strings"

	"github.com/open-rails/openrails/pkg/tenant"
)

// ErrVaultNotConfigured is returned by every vaultSecretStore operation until a
// live Vault client is wired in. The store is constructable (so managed wiring
// can reference it) but deliberately fails closed rather than silently returning
// empty secrets — a missing webhook signing secret must NEVER be treated as
// "verification disabled".
var ErrVaultNotConfigured = errors.New("tenancy: vault-backed secret store is not configured (no live Vault client)")

// VaultKV is the minimal KV-v2 surface the Vault-backed store needs. A real
// adapter satisfies it with hashicorp/vault/api (logical Read/Write/Delete/List)
// in a managed deployment. Keeping it an interface lets the store build and be
// unit-tested WITHOUT pulling in or running Vault.
type VaultKV interface {
	ReadSecret(ctx context.Context, path string) (map[string]string, error)
	WriteSecret(ctx context.Context, path string, data map[string]string) error
	DeleteSecret(ctx context.Context, path string) error
	ListSecrets(ctx context.Context, path string) ([]string, error)
}

// vaultSecretStore resolves the SAME (tenant, name) addressing as the DB-backed
// store to a tenant-scoped Vault KV path:
//
//	<mount>/openrails/tenants/<tenant-id>/<name>
//
// One tenant's secrets are therefore physically isolated under its own path
// prefix, and a Vault policy can grant a tenant operator read/write to ONLY its
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

// NewVaultSecretStore returns a Vault-backed TenantSecretStore. mount is the KV-v2
// mount path (e.g. "secret"). client may be nil — in that case the store is a
// documented stub that fails closed with ErrVaultNotConfigured, which is the
// state until a managed deployment injects a live VaultKV.
func NewVaultSecretStore(mount string, client VaultKV) TenantSecretStore {
	mount = strings.Trim(strings.TrimSpace(mount), "/")
	if mount == "" {
		mount = "secret"
	}
	return &vaultSecretStore{mount: mount, client: client}
}

// pathFor builds the tenant-scoped Vault path for a (tenant, name) pair.
func (v *vaultSecretStore) pathFor(tenantID tenant.ID, name string) string {
	return path.Join(v.mount, "openrails", "tenants", tenantID.String(), name)
}

func (v *vaultSecretStore) Get(ctx context.Context, tenantID tenant.ID, name string) (Secret, error) {
	if err := validateSecretRef(tenantID, name); err != nil {
		return Secret{}, err
	}
	if v.client == nil {
		return Secret{}, ErrVaultNotConfigured
	}
	data, err := v.client.ReadSecret(ctx, v.pathFor(tenantID, name))
	if err != nil {
		return Secret{}, fmt.Errorf("tenancy: vault read %q: %w", name, err)
	}
	val, ok := data["value"]
	if !ok {
		return Secret{}, ErrSecretNotFound
	}
	return Secret{Name: name, Value: val, Version: 1}, nil
}

func (v *vaultSecretStore) Put(ctx context.Context, tenantID tenant.ID, name, value string) (Secret, error) {
	if err := validateSecretRef(tenantID, name); err != nil {
		return Secret{}, err
	}
	if v.client == nil {
		return Secret{}, ErrVaultNotConfigured
	}
	if err := v.client.WriteSecret(ctx, v.pathFor(tenantID, name), map[string]string{"value": value}); err != nil {
		return Secret{}, fmt.Errorf("tenancy: vault write %q: %w", name, err)
	}
	return Secret{Name: name, Value: value, Version: 1}, nil
}

func (v *vaultSecretStore) Delete(ctx context.Context, tenantID tenant.ID, name string) error {
	if err := validateSecretRef(tenantID, name); err != nil {
		return err
	}
	if v.client == nil {
		return ErrVaultNotConfigured
	}
	if err := v.client.DeleteSecret(ctx, v.pathFor(tenantID, name)); err != nil {
		return fmt.Errorf("tenancy: vault delete %q: %w", name, err)
	}
	return nil
}

func (v *vaultSecretStore) List(ctx context.Context, tenantID tenant.ID) ([]string, error) {
	if tenantID.IsZero() {
		return nil, validateSecretRef(tenantID, "x")
	}
	if v.client == nil {
		return nil, ErrVaultNotConfigured
	}
	prefix := path.Join(v.mount, "openrails", "tenants", tenantID.String())
	names, err := v.client.ListSecrets(ctx, prefix)
	if err != nil {
		return nil, fmt.Errorf("tenancy: vault list: %w", err)
	}
	return names, nil
}
