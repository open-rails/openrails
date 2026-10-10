package merchantconfig

import (
	"context"
	"errors"

	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/merchant"
)

// Store reads the settings of the merchant in context through the reader the
// runtime bound to its database handle: the merchant document's, in a file or
// Vault.
type Store struct {
	db *db.DB
}

func NewStore(database *db.DB) *Store {
	return &Store{db: database}
}

// ErrNoSettings means the handle has no merchant configuration bound.
var ErrNoSettings = errors.New("merchant config: no merchant configuration is bound")

// Settings are the validated settings of the merchant in context.
func (s *Store) Settings(ctx context.Context) (Settings, error) {
	id, err := merchant.Require(ctx)
	if err != nil {
		return Settings{}, err
	}
	reader := s.db.MerchantConfig()
	if reader == nil {
		return Settings{}, ErrNoSettings
	}
	name, settings, err := reader.MerchantSettings(ctx, id)
	if err != nil {
		return Settings{}, err
	}
	return Normalize(name, settings)
}

// Get is the configuration of the merchant in context.
func (s *Store) Get(ctx context.Context) (models.MerchantConfiguration, bool, error) {
	settings, err := s.Settings(ctx)
	return settings.Config, err == nil, err
}
