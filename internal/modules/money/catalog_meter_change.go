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
// reinterpret recorded events.
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
	return nil
}
