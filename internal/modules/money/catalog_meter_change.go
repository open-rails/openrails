package money

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/catalog"
	"github.com/open-rails/openrails/internal/catalogrules"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/shared/apperr"
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
func CheckCatalogRateCardContracts(meter catalogrules.Meter, filter map[string][]string, price catalog.RatePrice) error {
	if err := catalogrules.ValidateMeterPrice("rate card", meter.Aggregation, price); err != nil {
		return invalidUsageRateCard(err)
	}
	if err := catalogrules.ValidateDimensions("default usage rate card", meter.GroupBy, filter, &price); err != nil {
		return meterRateCardConflict(err)
	}
	return nil
}

func CheckCatalogMeterLimitCounts(ctx context.Context, q *gen.Queries, merchantID uuid.UUID, meter catalogrules.Meter) error {
	if catalogrules.BillingSupported(meter.Aggregation) {
		return nil
	}
	counted, err := q.ListCatalogLimitsUsingMeters(ctx, gen.ListCatalogLimitsUsingMetersParams{MerchantID: merchantID, MeterKeys: []string{meter.Key}})
	if err != nil {
		return err
	}
	if len(counted) > 0 {
		return apperr.New(http.StatusConflict, billing.CodeMeterInUse, fmt.Sprintf("limit %q counts meter %q; its aggregation must remain sum or count", counted[0], meter.Key))
	}
	return nil
}
