package money

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/pkg/pricing"
)

// CheckCatalogMeterChange holds the ordinary meter/activity locks through the
// caller's transaction. Nil replacement means removal. Publishing must not
// reinterpret recorded events or orphan negotiated pricing/allowance sources.
func CheckCatalogMeterChange(ctx context.Context, tx pgx.Tx, merchantID uuid.UUID, key string, replacement *pricing.Meter) error {
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
	overrides, err := queries.CountUsageMeterOverrides(ctx, gen.CountUsageMeterOverridesParams{MerchantID: merchantID, MeterKey: key})
	if err != nil {
		return err
	}
	if overrides > 0 {
		return fmt.Errorf("meter %q has customer pricing overrides: %w", key, ErrMeterInUse)
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

// CheckCatalogRateCardRemoval retains the default-card deletion protections.
func CheckCatalogRateCardRemoval(ctx context.Context, tx pgx.Tx, merchantID uuid.UUID, key string) error {
	_, err := loadUsageMeterForRateCard(ctx, tx, merchantID, key)
	if errors.Is(err, ErrUsageMeterNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	queries := gen.New(tx)
	state, err := queries.GetDefaultUsageRateCardDeleteState(ctx, gen.GetDefaultUsageRateCardDeleteStateParams{MerchantID: merchantID, MeterKey: key})
	if err != nil {
		return err
	}
	if state.OverrideCount > 0 {
		return ErrRateCardHasOverrides
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

// CheckCatalogRateCardContracts validates the resulting default and retained
// negotiated prices against the resulting meter, never the old default. The
// meter/price locks serialize override writers through the caller's transaction.
func CheckCatalogRateCardContracts(ctx context.Context, tx pgx.Tx, merchantID uuid.UUID, meter pricing.Meter, filter map[string][]string, price pricing.RatePrice) error {
	if !pricing.BillingSupported(meter.Aggregation) {
		return invalidUsageRateCard(fmt.Errorf("meter %q does not support billing", meter.Key))
	}
	if err := pricing.ValidateDimensions("default usage rate card", meter.GroupBy, filter, &price); err != nil {
		return meterRateCardConflict(err)
	}
	_, err := loadUsageMeterForRateCard(ctx, tx, merchantID, meter.Key)
	if errors.Is(err, ErrUsageMeterNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	queries := gen.New(tx)
	if err := validateRateCardAsAllowanceSource(ctx, queries, merchantID, meter, price); err != nil {
		return err
	}
	rows, err := queries.ListUsageRateCardPricesForUpdate(ctx, gen.ListUsageRateCardPricesForUpdateParams{MerchantID: merchantID, MeterKey: meter.Key})
	if err != nil {
		return err
	}
	for _, row := range rows {
		if row.CustomerID == nil {
			continue
		}
		var negotiated pricing.RatePrice
		if err := json.Unmarshal(row.Price, &negotiated); err != nil {
			return err
		}
		if negotiated.Currency != price.Currency {
			return ErrRateCardCurrencyMismatch
		}
		if err := pricing.ValidateDimensions("negotiated usage rate card", meter.GroupBy, filter, &negotiated); err != nil {
			return meterRateCardConflict(err)
		}
		if err := validateRateCardAsAllowanceSource(ctx, queries, merchantID, meter, negotiated); err != nil {
			return err
		}
	}
	return nil
}
