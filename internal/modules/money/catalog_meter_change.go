package money

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails/catalog"
	"github.com/open-rails/openrails/internal/catalogrules"
	"github.com/open-rails/openrails/internal/db/gen"
)

// CheckCatalogMeterChange holds the ordinary meter/activity locks through the
// caller's transaction. Nil replacement means removal. Publishing must not
// reinterpret recorded events or orphan an allowance source.
func CheckCatalogMeterChange(ctx context.Context, tx pgx.Tx, merchantID uuid.UUID, key string, replacement *catalogrules.Meter) error {
	queries := gen.New(tx)
	if err := queries.LockUsageMeterKey(ctx, merchantID.String()+":"+key); err != nil {
		return err
	}
	current, err := loadUsageMeterForRateCard(ctx, tx, merchantID, key)
	if errors.Is(err, ErrUsageMeterNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	next := current
	if replacement != nil {
		next = *replacement
		current.EventType = effectiveMeterEventType(current)
		next.EventType = effectiveMeterEventType(next)
		if usageMeterSemanticsEqual(current, next) {
			return nil
		}
	}
	active, err := usageMeterHasActivity(ctx, tx, merchantID, current, next)
	if err != nil {
		return err
	}
	if active {
		return fmt.Errorf("meter %q has recorded usage: %w", key, ErrMeterInUse)
	}
	if replacement != nil {
		return nil
	}
	dependencies, err := queries.GetUsageRateCardAllowanceDependencyCurrencies(ctx, gen.GetUsageRateCardAllowanceDependencyCurrenciesParams{MerchantID: merchantID, MeterKey: key})
	if err != nil {
		return err
	}
	if len(dependencies) > 0 {
		return ErrAllowanceSourceInUse
	}
	return nil
}

// CheckCatalogRateCardRemoval refuses to remove a rate card another meter's
// allowance accrues from.
func CheckCatalogRateCardRemoval(ctx context.Context, tx pgx.Tx, merchantID uuid.UUID, key string) error {
	_, err := loadUsageMeterForRateCard(ctx, tx, merchantID, key)
	if errors.Is(err, ErrUsageMeterNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	dependencies, err := gen.New(tx).GetUsageRateCardAllowanceDependencyCurrencies(ctx, gen.GetUsageRateCardAllowanceDependencyCurrenciesParams{MerchantID: merchantID, MeterKey: key})
	if err != nil {
		return err
	}
	if len(dependencies) > 0 {
		return ErrAllowanceSourceInUse
	}
	return nil
}

// CheckCatalogRateCardContracts validates the resulting rate card against the
// resulting meter, never the old card.
func CheckCatalogRateCardContracts(ctx context.Context, tx pgx.Tx, merchantID uuid.UUID, meter catalogrules.Meter, filter map[string][]string, price catalog.RatePrice) error {
	if !catalogrules.BillingSupported(meter.Aggregation) {
		return invalidUsageRateCard(fmt.Errorf("meter %q does not support billing", meter.Key))
	}
	if err := catalogrules.ValidateDimensions("default usage rate card", meter.GroupBy, filter, &price); err != nil {
		return meterRateCardConflict(err)
	}
	_, err := loadUsageMeterForRateCard(ctx, tx, merchantID, meter.Key)
	if errors.Is(err, ErrUsageMeterNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	return validateRateCardAsAllowanceSource(ctx, gen.New(tx), merchantID, meter, price)
}
