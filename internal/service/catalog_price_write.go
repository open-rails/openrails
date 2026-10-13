package service

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/modules/catalog"
)

// priceWrite is one price version to store: its terms, the id they take, and
// its bindings (a provider price's provider object, or one of ours' Solana
// plan).
type priceWrite struct {
	req           billing.CreatePriceParams
	product       *models.Product
	id            uuid.UUID
	links         map[string]map[string]string
	providerOwned bool
}

// writeCatalogPrice atomically moves the key, writes the immutable financial
// row, and records its movement. Provider reads completed before entry.
// Nested calls reuse the existing transaction through a savepoint.
func (s *Service) writeCatalogPrice(ctx context.Context, w priceWrite) (*models.Price, error) {
	req, product, priceID := w.req, w.product, w.id
	var price *models.Price
	err := s.catalogDatabase().MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		prices := catalog.NewPriceService(s.catalogDatabase().NewWithPgxTx(tx))
		tid, err := merchant.Require(ctx)
		if err != nil {
			return err
		}
		now := time.Now().UTC()

		// A key names at most one non-archived row per product (prices_key_key),
		// so the row holding it is archived first, never after. Same key with new
		// terms archives the displaced row, then creates or reactivates the terms'
		// deterministic row: flip-flopping between two amounts keeps two rows. A
		// price created archived never claims the key.
		key, defaulted := resolvePriceKey(product, req)
		if err := lockCatalogKey(ctx, tx, tid, "product", product.Key); err != nil {
			return err
		}
		if err := lockCatalogKey(ctx, tx, tid, "price-key", key); err != nil {
			return err
		}
		var displacedID uuid.UUID
		if !req.Archived {
			displaced, dErr := prices.GetCurrentByKey(ctx, tid.UUID(), product.ID, key)
			if dErr != nil && !errors.Is(dErr, pgx.ErrNoRows) {
				return fmt.Errorf("resolve current holder of price key %q: %w", key, dErr)
			}
			if displaced != nil && displaced.ProductID != product.ID {
				return ErrCatalogConflict
			}
			if err := defaultKeyCadenceConflict(defaulted, displaced, req, key); err != nil {
				return err
			}
			if displaced != nil && displaced.ID != priceID {
				if err := prices.SetArchived(ctx, displaced.ID, true); err != nil {
					return fmt.Errorf("archive displaced price %s for key %q: %w", displaced.ID, key, err)
				}
				displacedID = displaced.ID
			}
		}

		// The id is the terms': a deleted price of these terms revives.
		revived, err := gen.New(tx).RevivePrice(ctx, gen.RevivePriceParams{MerchantID: tid.UUID(), ID: priceID, Archived: req.Archived})
		if err != nil {
			return fmt.Errorf("revive price %s: %w", priceID, err)
		}
		existing, existErr := prices.GetByID(ctx, priceID)
		if existErr != nil && !errors.Is(existErr, pgx.ErrNoRows) {
			return existErr
		}
		reactivating := existErr == nil
		// Same terms, same key, no archive change, nothing displaced: no movement
		// to log.
		trueNoOp := revived == 0 && reactivating && existing.Archived == req.Archived && existing.Key == key && displacedID == uuid.Nil

		if reactivating {
			if existing.ProviderOwned != w.providerOwned {
				return ErrCatalogConflict
			}
			if existing.Archived != req.Archived {
				if err := prices.SetArchived(ctx, priceID, req.Archived); err != nil {
					return fmt.Errorf("reactivate price %s: %w", priceID, err)
				}
			}
			if !w.providerOwned && !slices.Equal(existing.SellVia, req.SellVia) {
				if err := prices.SetSellVia(ctx, priceID, req.SellVia); err != nil {
					return err
				}
				existing.SellVia = req.SellVia
			}

			existing.Archived = req.Archived
			price = existing
		} else {
			price = &models.Price{
				ID:                   priceID,
				MerchantID:           tid.UUID(),
				ProductID:            req.ProductID.UUID(),
				Archived:             req.Archived,
				Amount:               req.UnitAmount,
				CustomerAmount:       req.CustomerAmount,
				Quantity:             req.Quantity,
				Currency:             req.Currency,
				AccessDurationHours:  req.AccessDurationHours,
				BillingIntervalHours: req.BillingIntervalHours,
				TrialUnitAmount:      req.TrialUnitAmount,
				TrialDurationHours:   req.TrialDurationHours,
				PSPLinks:             w.links,
				ProviderOwned:        w.providerOwned,
				SellVia:              req.SellVia,
				Key:                  key,
				CreatedAt:            now,
				UpdatedAt:            now,
			}
			if err := prices.Create(ctx, price); err != nil {
				return catalogWrite(err)
			}
		}

		if !req.Archived && !trueNoOp {
			// Log the key's move to priceID.
			if err := prices.RecordAuthoredKeyMovement(ctx, tid.UUID(), priceID, key); err != nil {
				return fmt.Errorf("record key movement for %q -> %s: %w", key, priceID, err)
			}
		}

		return nil
	})
	if err != nil {
		return nil, err
	}
	return price, nil
}

// Every catalog creation path locks the product before a price lookup key.
func lockCatalogKey(ctx context.Context, tx pgx.Tx, mid billing.MerchantID, kind, key string) error {
	return gen.New(tx).LockCatalogKey(ctx, "catalog-"+kind+":"+mid.String()+":"+key)
}
