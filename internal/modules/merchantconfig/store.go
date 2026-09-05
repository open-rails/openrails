package merchantconfig

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/pkg/merchant"
)

type Store struct {
	db *db.DB
}

func NewStore(database *db.DB) *Store {
	return &Store{db: database}
}

func (s *Store) Upsert(ctx context.Context, cfg models.MerchantConfiguration) error {
	if _, err := AutoTopupSafety(cfg.AutoTopupSafety); err != nil {
		return err
	}
	tid, err := merchant.Require(ctx)
	if err != nil {
		return err
	}
	configJSON, err := json.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("merchant config: encode config: %w", err)
	}
	now := time.Now().UTC()
	return s.db.RunInMerchantConn(ctx, func(ctx context.Context) error {
		return s.db.Gen(ctx).UpsertMerchantConfiguration(ctx, gen.UpsertMerchantConfigurationParams{
			MerchantID: tid.UUID(),
			Config:     configJSON,
			CreatedAt:  now,
			UpdatedAt:  now,
		})
	})
}

func (s *Store) Get(ctx context.Context) (models.MerchantConfiguration, bool, error) {
	tid, err := merchant.Require(ctx)
	if err != nil {
		return models.MerchantConfiguration{}, false, err
	}
	var cfg models.MerchantConfiguration
	found := false
	err = s.db.RunInMerchantConn(ctx, func(ctx context.Context) error {
		row, e := s.db.Gen(ctx).GetMerchantConfiguration(ctx, tid.UUID())
		if errors.Is(e, pgx.ErrNoRows) {
			return nil
		}
		if e != nil {
			return e
		}
		found = true
		if len(row.Config) == 0 {
			return nil
		}
		if uerr := json.Unmarshal(row.Config, &cfg); uerr != nil {
			return fmt.Errorf("merchant config: decode config: %w", uerr)
		}
		return nil
	})
	return cfg, found, err
}
