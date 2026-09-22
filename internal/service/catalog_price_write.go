package service

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/modules/catalog"
	"github.com/open-rails/openrails/pkg/merchant"
)

// writeCatalogPrice atomically moves the key, writes the immutable financial
// row, and records its movement. Provider resolution completed before entry.
// Nested calls reuse the existing transaction through a savepoint.
func (s *Service) writeCatalogPrice(ctx context.Context, req CreatePriceRequest, product *models.Product, priceID uuid.UUID, rails map[string]map[string]string) (*models.Price, error) {
	var price *models.Price
	err := s.catalogDatabase().MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		prices := catalog.NewPriceService(s.catalogDatabase().NewWithPgxTx(tx))
		tid, err := merchant.Require(ctx)
		if err != nil {
			return err
		}
		now := time.Now().UTC()

		// #774: resolve the price key (explicit or auto-default) and repoint it.
		// A key names AT MOST one non-archived row per merchant
		// (uq_prices_merchant_key_current) — so whatever OTHER row currently holds
		// this key must be archived FIRST (never after), or the create/reactivate
		// below would transiently double-hold the key and violate that index.
		// Declaring the SAME key with a NEW substance is exactly the version-bump
		// semantics: archive the displaced row, create-or-REACTIVATE the substance
		// row (re-declaring a previously-seen substance finds its archived row via
		// the #662 deterministic id and reactivates it — flip-flopping between two
		// amounts forever yields exactly two rows, never a third), re-point the
		// key. Skipped entirely when the caller creates the price pre-archived
		// (never claims the "current" pointer for its key).
		key := resolvePriceKey(product, req)
		if err := lockCatalogKey(ctx, tx, tid, "product", product.Key); err != nil {
			return err
		}
		if err := lockCatalogKey(ctx, tx, tid, "price-key", key); err != nil {
			return err
		}
		var displacedID uuid.UUID
		if !req.Archived {
			displaced, dErr := prices.GetCurrentByKey(ctx, tid.UUID(), key)
			if dErr != nil && !errors.Is(dErr, pgx.ErrNoRows) {
				return fmt.Errorf("resolve current holder of price key %q: %w", key, dErr)
			}
			if displaced != nil && displaced.ProductID != product.ID {
				return ErrCatalogConflict
			}
			if displaced != nil && displaced.ID != priceID {
				if err := prices.SetArchived(ctx, displaced.ID, true); err != nil {
					return fmt.Errorf("archive displaced price %s for key %q: %w", displaced.ID, key, err)
				}
				displacedID = displaced.ID
			}
		}

		existing, existErr := prices.GetByID(ctx, priceID)
		if existErr != nil && !errors.Is(existErr, pgx.ErrNoRows) {
			return existErr
		}
		reactivating := existErr == nil
		// A true no-op: re-declaring the SAME substance under the SAME key with no
		// archived-state change and no key displaced — nothing moved, so no
		// movement-log entry (idempotent, as today).
		trueNoOp := reactivating && existing.Archived == req.Archived && existing.Key == key && displacedID == uuid.Nil

		if reactivating {
			if existing.Archived != req.Archived {
				if err := prices.SetArchived(ctx, priceID, req.Archived); err != nil {
					return fmt.Errorf("reactivate price %s: %w", priceID, err)
				}
			}
			if existing.Key != key {
				if err := prices.SetKey(ctx, priceID, key); err != nil {
					return fmt.Errorf("relabel price %s to key %q: %w", priceID, key, err)
				}
			}
			existing.Archived = req.Archived
			existing.Key = key
			price = existing
		} else {
			price = &models.Price{
				ID:                  priceID,
				MerchantID:          tid.UUID(),
				ProductID:           req.ProductID.UUID(),
				Archived:            req.Archived,
				Amount:              req.UnitAmount,
				Currency:            req.Currency,
				AccessDurationHours: req.AccessDurationHours,
				AutoRenew:           req.AutoRenew,
				TrialUnitAmount:     req.TrialUnitAmount,
				TrialDurationHours:  req.TrialDurationHours,
				PSPLinks:            rails,
				Key:                 key,
				CreatedAt:           now,
				UpdatedAt:           now,
			}
			if err := prices.Create(ctx, price); err != nil {
				return catalogWrite(err)
			}
		}

		if !req.Archived && !trueNoOp {
			// #774 pointer-movement log: key's current pointer moved to priceID.
			if err := prices.RecordKeyMovement(ctx, tid.UUID(), priceID, key, now); err != nil {
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
func lockCatalogKey(ctx context.Context, tx pgx.Tx, mid merchant.ID, kind, key string) error {
	_, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtextextended($1,0))", "catalog-"+kind+":"+mid.String()+":"+key)
	return err
}
