package tenancy

import (
	"context"
	"fmt"
	"strings"

	"github.com/open-rails/openrails/pkg/tenant"
)

type writeRestrictedSecretStore struct {
	inner   TenantSecretStore
	reasons map[string]string
}

// NewWriteRestrictedSecretStore blocks Put for selected secret names while still
// allowing reads, deletes, and listing. It is used by DB-backed deployments when
// encryption is disabled so especially sensitive keys cannot be newly stored in
// plaintext.
func NewWriteRestrictedSecretStore(inner TenantSecretStore, reasons map[string]string) TenantSecretStore {
	if inner == nil || len(reasons) == 0 {
		return inner
	}
	clean := make(map[string]string, len(reasons))
	for name, reason := range reasons {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		clean[name] = strings.TrimSpace(reason)
	}
	if len(clean) == 0 {
		return inner
	}
	return &writeRestrictedSecretStore{inner: inner, reasons: clean}
}

func (s *writeRestrictedSecretStore) Get(ctx context.Context, tenantID tenant.ID, name string) (Secret, error) {
	return s.inner.Get(ctx, tenantID, name)
}

func (s *writeRestrictedSecretStore) Put(ctx context.Context, tenantID tenant.ID, name, value string) (Secret, error) {
	if reason, ok := s.reasons[strings.TrimSpace(name)]; ok {
		if reason == "" {
			reason = "write is restricted"
		}
		return Secret{}, fmt.Errorf("tenancy: refusing to store secret %q: %s", name, reason)
	}
	return s.inner.Put(ctx, tenantID, name, value)
}

func (s *writeRestrictedSecretStore) Delete(ctx context.Context, tenantID tenant.ID, name string) error {
	return s.inner.Delete(ctx, tenantID, name)
}

func (s *writeRestrictedSecretStore) List(ctx context.Context, tenantID tenant.ID) ([]string, error) {
	return s.inner.List(ctx, tenantID)
}
