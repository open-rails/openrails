package money

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails/catalog"
	"github.com/open-rails/openrails/internal/catalogrules"
	"github.com/open-rails/openrails/internal/catalogrules/syntax"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/shared/moneyutil"
)

// Host-owned usage meters and rate cards.

// EnsureUsageMeter idempotently upserts a catalog meter. Document edits keep
// partial definitions and validate replacement cards together.
func (s *MoneyService) EnsureUsageMeter(ctx context.Context, meter catalogrules.Meter, replaceRateCards bool) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("money service not initialized")
	}
	if !replaceRateCards {
		if err := catalogrules.ValidateMeter("usage meter", &meter); err != nil {
			return err
		}
	} else if err := syntax.ValidateApplicationKey(meter.Key, "meter"); err != nil {
		return err
	}
	groupByJSON, err := json.Marshal(meter.GroupBy)
	if err != nil {
		return fmt.Errorf("encode usage meter group_by: %w", err)
	}
	tid, err := merchant.Require(ctx)
	if err != nil {
		return err
	}
	return s.db.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		queries := gen.New(tx)
		if err := queries.LockUsageMeterKey(ctx, tid.UUID().String()+":"+meter.Key); err != nil {
			return fmt.Errorf("lock usage meter: %w", err)
		}

		if err := CheckCatalogMeterLimitCounts(ctx, queries, tid.UUID(), meter); err != nil {
			return err
		}
		if replaceRateCards {
			// Document edits validate the resulting cards, not the stored cards.
			if err := CheckCatalogMeterChange(ctx, tx, tid.UUID(), meter.Key, &meter); err != nil {
				return err
			}
		} else {
			row, err := queries.GetUsageMeterForUpdate(ctx, gen.GetUsageMeterForUpdateParams{
				MerchantID: tid.UUID(), MeterKey: meter.Key,
			})
			if err != nil && !errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("load usage meter: %w", err)
			}
			if err == nil {
				existing := catalogrules.Meter{Key: row.Key, EventType: row.EventType,
					ValueProperty: row.ValueProperty, Aggregation: catalog.Aggregation(row.Aggregation), Unit: row.Unit}
				if err := json.Unmarshal(row.GroupBy, &existing.GroupBy); err != nil {
					return fmt.Errorf("decode usage meter group_by: %w", err)
				}
				if usageMeterSemanticsEqual(existing, meter) {
					return nil
				}
				active, err := usageMeterHasActivity(ctx, tx, tid.UUID(), existing, meter)
				if err != nil {
					return err
				}
				if active {
					return ErrMeterInUse
				}
				if err := validateUsageMeterRateCardContracts(ctx, queries, tid.UUID(), meter); err != nil {
					return err
				}
			}
		}
		if err := queries.UpsertUsageMeter(ctx, gen.UpsertUsageMeterParams{
			MerchantID: tid.UUID(), MeterKey: meter.Key, EventType: meter.EventType,
			ValueProperty: meter.ValueProperty, Aggregation: string(meter.Aggregation),
			Unit: meter.Unit, GroupBy: groupByJSON,
		}); err != nil {
			return fmt.Errorf("upsert usage meter: %w", err)
		}
		return nil
	})
}

// UsageRateCardInput declares a meter's usage rate card under the product
// whose invoices carry it.
type UsageRateCardInput struct {
	ProductID *uuid.UUID
	MeterKey  string
	Filter    map[string][]string
	Price     catalog.RatePrice
	Allowance *catalog.Allowance
	// Ordinal zero selects the meter's default card; documents name a slot.
	Ordinal     int64
	PaymentTerm string
	ID          *uuid.UUID
	CreatedAt   *time.Time
}

// SetUsageRateCard upserts a default usage card or a declared product slot.
func (s *MoneyService) SetUsageRateCard(ctx context.Context, in UsageRateCardInput) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("money service not initialized")
	}
	meterKey := in.MeterKey
	if in.Ordinal == 0 {
		if err := syntax.ValidateApplicationKey(meterKey, "meter"); err != nil {
			return invalidUsageRateCard(err)
		}
		if in.ProductID == nil || *in.ProductID == uuid.Nil {
			return invalidUsageRateCard(fmt.Errorf("product_id required for a rate card"))
		}
		if err := catalogrules.ValidateUsagePrice("usage rate card", &in.Price); err != nil {
			return invalidUsageRateCard(err)
		}
		if in.Price.Currency == "" {
			return invalidUsageRateCard(fmt.Errorf("usage rate card: currency is required"))
		}
		if err := moneyutil.ValidateBalanceCurrency(string(in.Price.Currency)); err != nil {
			return invalidUsageRateCard(err)
		}
		if err := catalogrules.ValidateUsageAllowance("usage rate card", in.Price, in.Allowance); err != nil {
			return invalidUsageRateCard(err)
		}
		if err := catalogrules.ValidateFilter("usage rate card", &in.Filter); err != nil {
			return invalidUsageRateCard(err)
		}
	} else {
		if err := catalogrules.ValidateRatePrice("catalog rate card", &in.Price); err != nil {
			return err
		}
		if err := catalogrules.ValidateAllowance("catalog allowance", in.Allowance); err != nil {
			return err
		}
	}
	priceJSON, err := json.Marshal(in.Price)
	if err != nil {
		return fmt.Errorf("encode rate card price: %w", err)
	}
	var allowanceJSON []byte
	if in.Allowance != nil {
		if allowanceJSON, err = json.Marshal(in.Allowance); err != nil {
			return fmt.Errorf("encode rate card allowance: %w", err)
		}
	}
	tid, err := merchant.Require(ctx)
	if err != nil {
		return err
	}
	return s.db.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		queries := gen.New(tx)
		if meterKey != "" {
			meter, err := loadUsageMeterForRateCard(ctx, tx, tid.UUID(), meterKey, in.Ordinal == 0)
			if err != nil {
				return err
			}
			if err := CheckCatalogRateCardContracts(ctx, tx, tid.UUID(), meter, in.Filter, in.Price, in.Ordinal == 0); err != nil {
				return err
			}
		}
		if in.Ordinal == 0 {
			if err := ensureActiveProduct(ctx, tx, tid.UUID(), *in.ProductID); err != nil {
				return err
			}
			if err := queries.LockUsageRateCardProduct(ctx, tid.UUID().String()+":"+in.ProductID.String()); err != nil {
				return fmt.Errorf("lock rate card product: %w", err)
			}
		}
		filterJSON, err := json.Marshal(in.Filter)
		if err != nil {
			return fmt.Errorf("encode rate card filter: %w", err)
		}
		if err := queries.UpsertUsageRateCard(ctx, gen.UpsertUsageRateCardParams{
			MerchantID:  tid.UUID(),
			ProductID:   in.ProductID,
			MeterKey:    meterKey,
			Filter:      filterJSON,
			Allowance:   allowanceJSON,
			Price:       priceJSON,
			Ordinal:     in.Ordinal,
			PaymentTerm: in.PaymentTerm,
			ID:          in.ID,
			CreatedAt:   in.CreatedAt,
		}); err != nil {
			return fmt.Errorf("upsert default usage rate card: %w", err)
		}
		return nil
	})
}

// DeleteDefaultUsageRateCard removes a meter's rate card.
func (s *MoneyService) DeleteDefaultUsageRateCard(ctx context.Context, meterKey string) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("money service not initialized")
	}
	if err := syntax.ValidateApplicationKey(meterKey, "meter"); err != nil {
		return err
	}
	tid, err := merchant.Require(ctx)
	if err != nil {
		return err
	}
	return s.db.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := loadUsageMeterForRateCard(ctx, tx, tid.UUID(), meterKey, true); err != nil {
			return err
		}
		queries := gen.New(tx)
		exists, err := queries.DefaultUsageRateCardExists(
			ctx,
			gen.DefaultUsageRateCardExistsParams{
				MerchantID: tid.UUID(),
				MeterKey:   meterKey,
			},
		)
		if err != nil {
			return fmt.Errorf("load default usage rate card: %w", err)
		}
		if !exists {
			return ErrDefaultRateCardNotFound
		}
		if err := queries.DeleteDefaultUsageRateCard(ctx, gen.DeleteDefaultUsageRateCardParams{
			MerchantID: tid.UUID(),
			MeterKey:   meterKey,
		}); err != nil {
			return fmt.Errorf("delete default usage rate card: %w", err)
		}
		return nil
	})
}
