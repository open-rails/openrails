package service

import (
	"context"
	"fmt"

	"github.com/open-rails/openrails/billing"

	"github.com/open-rails/openrails/internal/modules/money"
)

var (
	ErrUsageMeterNotFound       = money.ErrUsageMeterNotFound
	ErrMeterInUse               = money.ErrMeterInUse
	ErrDefaultRateCardNotFound  = money.ErrDefaultRateCardNotFound
	ErrDefaultRateCardRequired  = money.ErrDefaultRateCardRequired
	ErrRateCardHasOverrides     = money.ErrRateCardHasOverrides
	ErrRateCardCurrencyMismatch = money.ErrRateCardCurrencyMismatch
	ErrRateCardProductNotFound  = money.ErrRateCardProductNotFound
	ErrAllowanceMeterNotFound   = money.ErrAllowanceMeterNotFound
	ErrAllowanceSourceInvalid   = money.ErrAllowanceSourceInvalid
	ErrAllowanceSourceInUse     = money.ErrAllowanceSourceInUse
	ErrMeterRateCardConflict    = money.ErrMeterRateCardConflict
	ErrUsageRateCardInvalid     = money.ErrUsageRateCardInvalid
)

// ListUsageMeters returns one page of the merchant's meters, by key.
func (s *Service) ListUsageMeters(ctx context.Context, page billing.PageRequest) (billing.ListPage[billing.Meter], error) {
	ctx, release, pinErr := s.pin(ctx)
	if pinErr != nil {
		return billing.ListPage[billing.Meter]{}, pinErr
	}
	defer release()
	if s == nil || s.rt == nil {
		return billing.ListPage[billing.Meter]{}, fmt.Errorf("service not initialized")
	}
	return s.moneyService().ListUsageMeters(ctx, page)
}

// GetUsageMeter returns one merchant meter by canonical key.
func (s *Service) GetUsageMeter(ctx context.Context, meterKey string) (*billing.Meter, error) {
	ctx, release, pinErr := s.pin(ctx)
	if pinErr != nil {
		return nil, pinErr
	}
	defer release()
	if s == nil || s.rt == nil {
		return nil, fmt.Errorf("service not initialized")
	}
	return s.moneyService().GetUsageMeter(ctx, meterKey)
}
