package tenancy

import (
	"context"
	"sort"
	"sync"

	"github.com/open-rails/openrails/pkg/merchant"
)

// memSecretStore is an in-memory TenantSecretStore for tests and pure-dev runs
// with no database. It is fully tenant-namespaced and safe for concurrent use.
type memSecretStore struct {
	mu   sync.RWMutex
	data map[string]map[string]Secret // tenantID -> name -> Secret
}

// NewMemorySecretStore returns an in-memory TenantSecretStore. It requires no
// database or Vault, so the secret abstraction (and everything built on it)
// compiles and tests without external dependencies.
func NewMemorySecretStore() TenantSecretStore {
	return &memSecretStore{data: make(map[string]map[string]Secret)}
}

func (m *memSecretStore) Get(_ context.Context, tenantID merchant.ID, name string) (Secret, error) {
	if err := validateSecretRef(tenantID, name); err != nil {
		return Secret{}, err
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	byName, ok := m.data[tenantID.String()]
	if !ok {
		return Secret{}, ErrSecretNotFound
	}
	s, ok := byName[name]
	if !ok {
		return Secret{}, ErrSecretNotFound
	}
	return s, nil
}

func (m *memSecretStore) Put(_ context.Context, tenantID merchant.ID, name, value string) (Secret, error) {
	if err := validateSecretRef(tenantID, name); err != nil {
		return Secret{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	key := tenantID.String()
	byName, ok := m.data[key]
	if !ok {
		byName = make(map[string]Secret)
		m.data[key] = byName
	}
	existing, had := byName[name]
	version := 1
	if had {
		if existing.Value == value {
			return existing, nil // idempotent no-op rotation
		}
		version = existing.Version + 1
	}
	s := Secret{Name: name, Value: value, Version: version}
	byName[name] = s
	return s, nil
}

func (m *memSecretStore) Delete(_ context.Context, tenantID merchant.ID, name string) error {
	if err := validateSecretRef(tenantID, name); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if byName, ok := m.data[tenantID.String()]; ok {
		delete(byName, name)
	}
	return nil
}

func (m *memSecretStore) List(_ context.Context, tenantID merchant.ID) ([]string, error) {
	if tenantID.IsZero() {
		return nil, validateSecretRef(tenantID, "x")
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	byName, ok := m.data[tenantID.String()]
	if !ok {
		return nil, nil
	}
	names := make([]string, 0, len(byName))
	for n := range byName {
		names = append(names, n)
	}
	sort.Strings(names)
	return names, nil
}
