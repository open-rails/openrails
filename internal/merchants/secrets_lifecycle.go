package merchants

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/pkg/merchant"
)

// lifecycleSecretStore serializes runtime Vault writes with merchant tombstoning.
// Reads/list/delete retain UUID addressing so committed cleanup can run after
// tombstoning. Put holds the merchant row lock through the backend and cache write.
type lifecycleSecretStore struct {
	MerchantSecretStore
	database *db.DB
}

// NewLifecycleSecretStore protects external writes through the runtime store.
// DB.MerchantTx reuses the request's pinned merchant connection; it does not
// acquire a second pool connection while that request is holding the first.
func NewLifecycleSecretStore(database *db.DB, inner MerchantSecretStore) MerchantSecretStore {
	return &lifecycleSecretStore{MerchantSecretStore: inner, database: database}
}

func (s *lifecycleSecretStore) Put(ctx context.Context, id merchant.ID, name, value string) (Secret, error) {
	if err := validateSecretRef(id, name); err != nil {
		return Secret{}, err
	}
	if s.database == nil || s.MerchantSecretStore == nil {
		return Secret{}, ErrSecretBackendUnavailable
	}
	var result Secret
	err := s.database.MerchantTx(merchant.WithID(ctx, id), func(ctx context.Context, tx pgx.Tx) error {
		if _, err := gen.New(tx).LockLiveMerchantForSecretWrite(ctx, id.UUID()); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrMerchantNotFound
			}
			return fmt.Errorf("lock merchant for secret write: %w", err)
		}
		var err error
		result, err = s.MerchantSecretStore.Put(ctx, id, name, value)
		return err
	})
	return result, err
}
