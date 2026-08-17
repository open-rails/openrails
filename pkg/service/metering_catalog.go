package service

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/open-rails/openrails/pkg/pricing"
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
)

// UsageMeterDTO is one merchant-scoped usage stream and its billing state.
type UsageMeterDTO struct {
	Key                string                   `json:"key"`
	EventType          string                   `json:"event_type,omitempty"`
	EffectiveEventType string                   `json:"effective_event_type"`
	ValueProperty      string                   `json:"value_property,omitempty"`
	Aggregation        string                   `json:"aggregation"`
	Unit               string                   `json:"unit,omitempty"`
	GroupBy            map[string]string        `json:"group_by"`
	BillingSupported   bool                     `json:"billing_supported"`
	DefaultRateCard    *DefaultUsageRateCardDTO `json:"default_rate_card,omitempty"`
	OverrideCount      int64                    `json:"override_count"`
	HasActivity        bool                     `json:"has_activity"`
	LastEventAt        *time.Time               `json:"last_event_at,omitempty"`
	CreatedAt          time.Time                `json:"created_at"`
	UpdatedAt          time.Time                `json:"updated_at"`
}

// DefaultUsageRateCardDTO is the merchant-default in-arrears price for a meter.
type DefaultUsageRateCardDTO struct {
	ID         uuid.UUID           `json:"id"`
	ProductID  uuid.UUID           `json:"product_id"`
	ProductKey string              `json:"product_key"`
	Filter     map[string][]string `json:"filter"`
	Price      pricing.RatePrice   `json:"price"`
	Allowance  *pricing.Allowance  `json:"allowance,omitempty"`
	CreatedAt  time.Time           `json:"created_at"`
	UpdatedAt  time.Time           `json:"updated_at"`
}

// UsageMeterOverrideDTO is one negotiated payer price for a meter.
type UsageMeterOverrideDTO struct {
	CustomerID uuid.UUID          `json:"customer_id"`
	Subject    string             `json:"subject,omitempty"`
	Email      string             `json:"email,omitempty"`
	Price      pricing.RatePrice  `json:"price"`
	Allowance  *pricing.Allowance `json:"allowance,omitempty"`
	CreatedAt  time.Time          `json:"created_at"`
	UpdatedAt  time.Time          `json:"updated_at"`
}

// ListUsageMeters returns a deterministic page of merchant meters.
func (s *Service) ListUsageMeters(
	ctx context.Context,
	options PaginationOptions,
) (PaginatedResult[UsageMeterDTO], error) {
	ctx, release, pinErr := s.pin(ctx)
	if pinErr != nil {
		return PaginatedResult[UsageMeterDTO]{}, pinErr
	}
	defer release()

	if s == nil || s.rt == nil {
		return PaginatedResult[UsageMeterDTO]{}, fmt.Errorf("service not initialized")
	}
	page, err := s.moneyService().ListUsageMeters(ctx, options.Limit, options.Offset)
	if err != nil {
		return PaginatedResult[UsageMeterDTO]{}, err
	}
	items := make([]UsageMeterDTO, 0, len(page.Items))
	for _, meter := range page.Items {
		items = append(items, usageMeterDTO(meter))
	}
	return PaginatedResult[UsageMeterDTO]{
		Data:       items,
		TotalItems: page.Total,
		Limit:      page.Limit,
		Offset:     page.Offset,
	}, nil
}

// GetUsageMeter returns one merchant meter by canonical key.
func (s *Service) GetUsageMeter(ctx context.Context, meterKey string) (*UsageMeterDTO, error) {
	ctx, release, pinErr := s.pin(ctx)
	if pinErr != nil {
		return nil, pinErr
	}
	defer release()

	if s == nil || s.rt == nil {
		return nil, fmt.Errorf("service not initialized")
	}
	meter, err := s.moneyService().GetUsageMeter(ctx, meterKey)
	if err != nil {
		return nil, err
	}
	dto := usageMeterDTO(*meter)
	return &dto, nil
}

// ListUsageMeterOverrides returns negotiated payer prices for one meter.
func (s *Service) ListUsageMeterOverrides(
	ctx context.Context,
	meterKey string,
	options PaginationOptions,
) (PaginatedResult[UsageMeterOverrideDTO], error) {
	ctx, release, pinErr := s.pin(ctx)
	if pinErr != nil {
		return PaginatedResult[UsageMeterOverrideDTO]{}, pinErr
	}
	defer release()

	if s == nil || s.rt == nil {
		return PaginatedResult[UsageMeterOverrideDTO]{}, fmt.Errorf("service not initialized")
	}
	page, err := s.moneyService().ListUsageMeterOverrides(
		ctx,
		meterKey,
		options.Limit,
		options.Offset,
	)
	if err != nil {
		return PaginatedResult[UsageMeterOverrideDTO]{}, err
	}
	items := make([]UsageMeterOverrideDTO, 0, len(page.Items))
	for _, override := range page.Items {
		items = append(items, UsageMeterOverrideDTO{
			CustomerID: override.CustomerID,
			Subject:    override.Subject,
			Email:      override.Email,
			Price:      override.Price,
			Allowance:  override.Allowance,
			CreatedAt:  override.CreatedAt,
			UpdatedAt:  override.UpdatedAt,
		})
	}
	return PaginatedResult[UsageMeterOverrideDTO]{
		Data:       items,
		TotalItems: page.Total,
		Limit:      page.Limit,
		Offset:     page.Offset,
	}, nil
}

func usageMeterDTO(meter money.UsageMeter) UsageMeterDTO {
	dto := UsageMeterDTO{
		Key:                meter.Key,
		EventType:          meter.EventType,
		EffectiveEventType: meter.EffectiveEventType,
		ValueProperty:      meter.ValueProperty,
		Aggregation:        meter.Aggregation,
		Unit:               meter.Unit,
		GroupBy:            meter.GroupBy,
		BillingSupported:   meter.BillingSupported,
		OverrideCount:      meter.OverrideCount,
		HasActivity:        meter.HasActivity,
		LastEventAt:        meter.LastEventAt,
		CreatedAt:          meter.CreatedAt,
		UpdatedAt:          meter.UpdatedAt,
	}
	if meter.DefaultRateCard != nil {
		dto.DefaultRateCard = &DefaultUsageRateCardDTO{
			ID:         meter.DefaultRateCard.ID,
			ProductID:  meter.DefaultRateCard.ProductID,
			ProductKey: meter.DefaultRateCard.ProductKey,
			Filter:     meter.DefaultRateCard.Filter,
			Price:      meter.DefaultRateCard.Price,
			Allowance:  meter.DefaultRateCard.Allowance,
			CreatedAt:  meter.DefaultRateCard.CreatedAt,
			UpdatedAt:  meter.DefaultRateCard.UpdatedAt,
		}
	}
	return dto
}
