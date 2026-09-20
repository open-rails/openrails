package money

import (
	"context"
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

// CheckCatalogRateCardChange reuses the default-card deletion/currency guards.
// The meter row lock also serializes negotiated override writers.
func CheckCatalogRateCardChange(ctx context.Context, tx pgx.Tx, merchantID uuid.UUID, key string, replacement *pricing.RatePrice) error {
	meter, err := loadUsageMeterForRateCard(ctx, tx, merchantID, key)
	if errors.Is(err, ErrUsageMeterNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	queries := gen.New(tx)
	if replacement != nil {
		conflict, err := queries.UsageRateCardCurrencyConflict(ctx, gen.UsageRateCardCurrencyConflictParams{MerchantID: merchantID, MeterKey: key, Currency: replacement.Currency})
		if err != nil {
			return err
		}
		if conflict {
			return ErrRateCardCurrencyMismatch
		}
		return validateRateCardAsAllowanceSource(ctx, queries, merchantID, meter, *replacement)
	}
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
