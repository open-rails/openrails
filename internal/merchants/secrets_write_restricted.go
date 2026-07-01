package merchants

import (
	"context"
	"fmt"
	"path"
	"strings"

	"github.com/open-rails/openrails/pkg/merchant"
)

type writeRestrictedSecretStore struct {
	inner   MerchantSecretStore
	reasons map[string]string
}

// NewWriteRestrictedSecretStore blocks Put for selected secret names while still
// allowing reads, deletes, and listing. It is used by DB-backed deployments when
// encryption is disabled so especially sensitive keys cannot be newly stored in
// plaintext.
func NewWriteRestrictedSecretStore(inner MerchantSecretStore, reasons map[string]string) MerchantSecretStore {
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

func (s *writeRestrictedSecretStore) Get(ctx context.Context, merchantID merchant.ID, name string) (Secret, error) {
	return s.inner.Get(ctx, merchantID, name)
}

func (s *writeRestrictedSecretStore) Put(ctx context.Context, merchantID merchant.ID, name, value string) (Secret, error) {
	name = strings.TrimSpace(name)
	for pattern, reason := range s.reasons {
		matched, err := path.Match(pattern, name)
		if err != nil {
			matched = pattern == name
		}
		if !matched {
			continue
		}
		if reason == "" {
			reason = "write is restricted"
		}
		return Secret{}, fmt.Errorf("merchants: refusing to store secret %q: %s", name, reason)
	}
	return s.inner.Put(ctx, merchantID, name, value)
}

func (s *writeRestrictedSecretStore) Delete(ctx context.Context, merchantID merchant.ID, name string) error {
	return s.inner.Delete(ctx, merchantID, name)
}

func (s *writeRestrictedSecretStore) List(ctx context.Context, merchantID merchant.ID) ([]string, error) {
	return s.inner.List(ctx, merchantID)
}
