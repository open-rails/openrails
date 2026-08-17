package money

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/pkg/merchant"
	"github.com/open-rails/openrails/pkg/pricing"
)

const (
	defaultMeteringPageSize = 50
	maxMeteringPageSize     = 200
)

var (
	ErrUsageMeterNotFound       = errors.New("usage meter not found")
	ErrMeterInUse               = errors.New("usage meter has recorded activity")
	ErrDefaultRateCardNotFound  = errors.New("default usage rate card not found")
	ErrDefaultRateCardRequired  = errors.New("default usage rate card required")
	ErrRateCardHasOverrides     = errors.New("usage rate card has payer overrides")
	ErrRateCardCurrencyMismatch = errors.New("usage rate card currency must match default")
	ErrRateCardProductNotFound  = errors.New("usage rate card product not found")
	ErrAllowanceMeterNotFound   = errors.New("usage rate card allowance meter not found")
)

// UsageMeter is one merchant-scoped usage stream and its optional default
// rate card, projected with billing activity and override state.
type UsageMeter struct {
	Key                string                `json:"key"`
	EventType          string                `json:"event_type,omitempty"`
	EffectiveEventType string                `json:"effective_event_type"`
	ValueProperty      string                `json:"value_property,omitempty"`
	Aggregation        string                `json:"aggregation"`
	Unit               string                `json:"unit,omitempty"`
	GroupBy            map[string]string     `json:"group_by"`
	BillingSupported   bool                  `json:"billing_supported"`
	DefaultRateCard    *DefaultUsageRateCard `json:"default_rate_card,omitempty"`
	OverrideCount      int64                 `json:"override_count"`
	HasActivity        bool                  `json:"has_activity"`
	LastEventAt        *time.Time            `json:"last_event_at,omitempty"`
	CreatedAt          time.Time             `json:"created_at"`
	UpdatedAt          time.Time             `json:"updated_at"`
}

// DefaultUsageRateCard is the merchant-default in-arrears price for a meter.
type DefaultUsageRateCard struct {
	ID         uuid.UUID           `json:"id"`
	ProductID  uuid.UUID           `json:"product_id"`
	ProductKey string              `json:"product_key"`
	Filter     map[string][]string `json:"filter"`
	Price      pricing.RatePrice   `json:"price"`
	Allowance  *pricing.Allowance  `json:"allowance,omitempty"`
	CreatedAt  time.Time           `json:"created_at"`
	UpdatedAt  time.Time           `json:"updated_at"`
}

// UsageMeterPage is a bounded offset page of meters.
type UsageMeterPage struct {
	Items  []UsageMeter
	Total  int64
	Limit  int
	Offset int
}

// UsageMeterOverride is one negotiated payer price for a meter.
type UsageMeterOverride struct {
	CustomerID uuid.UUID          `json:"customer_id"`
	Subject    string             `json:"subject,omitempty"`
	Email      string             `json:"email,omitempty"`
	Price      pricing.RatePrice  `json:"price"`
	Allowance  *pricing.Allowance `json:"allowance,omitempty"`
	CreatedAt  time.Time          `json:"created_at"`
	UpdatedAt  time.Time          `json:"updated_at"`
}

// UsageMeterOverridePage is a bounded offset page of payer overrides.
type UsageMeterOverridePage struct {
	Items  []UsageMeterOverride
	Total  int64
	Limit  int
	Offset int
}

// ListUsageMeters returns meters ordered by their canonical key.
func (s *MoneyService) ListUsageMeters(ctx context.Context, limit, offset int) (UsageMeterPage, error) {
	page := UsageMeterPage{}
	if s == nil || s.db == nil {
		return page, fmt.Errorf("money service not initialized")
	}
	page.Limit, page.Offset = normalizeMeteringPage(limit, offset)
	tenant, err := merchant.Require(ctx)
	if err != nil {
		return page, err
	}

	err = s.db.RunInMerchantConn(ctx, func(ctx context.Context) error {
		queries := gen.New(s.db.Qx(ctx))
		page.Total, err = queries.CountUsageMeters(ctx, tenant.UUID())
		if err != nil {
			return fmt.Errorf("count usage meters: %w", err)
		}

		rows, err := queries.ListUsageMetersWithCatalog(ctx, gen.ListUsageMetersWithCatalogParams{
			MerchantID: tenant.UUID(),
			PageLimit:  int32(page.Limit),
			PageOffset: int32(page.Offset),
		})
		if err != nil {
			return fmt.Errorf("list usage meters: %w", err)
		}

		page.Items = make([]UsageMeter, 0, len(rows))
		for _, row := range rows {
			meter, err := usageMeterFromListRow(row)
			if err != nil {
				return err
			}
			page.Items = append(page.Items, meter)
		}
		return nil
	})
	return page, err
}

// GetUsageMeter returns one meter and its optional default rate card.
func (s *MoneyService) GetUsageMeter(ctx context.Context, meterKey string) (*UsageMeter, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("money service not initialized")
	}
	meterKey = pricing.NormalizeKey(meterKey)
	if meterKey == "" {
		return nil, fmt.Errorf("meter key required")
	}
	tenant, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}

	var meter UsageMeter
	err = s.db.RunInMerchantConn(ctx, func(ctx context.Context) error {
		row, queryErr := gen.New(s.db.Qx(ctx)).GetUsageMeterWithCatalog(
			ctx,
			gen.GetUsageMeterWithCatalogParams{MerchantID: tenant.UUID(), MeterKey: meterKey},
		)
		if queryErr != nil {
			if errors.Is(queryErr, pgx.ErrNoRows) {
				return ErrUsageMeterNotFound
			}
			return queryErr
		}
		meter, queryErr = usageMeterFromGetRow(row)
		return queryErr
	})
	if err != nil {
		return nil, err
	}
	return &meter, nil
}

// ListUsageMeterOverrides returns negotiated payer prices for one meter.
func (s *MoneyService) ListUsageMeterOverrides(
	ctx context.Context,
	meterKey string,
	limit int,
	offset int,
) (UsageMeterOverridePage, error) {
	page := UsageMeterOverridePage{}
	if s == nil || s.db == nil {
		return page, fmt.Errorf("money service not initialized")
	}
	meterKey = pricing.NormalizeKey(meterKey)
	if meterKey == "" {
		return page, fmt.Errorf("meter key required")
	}
	page.Limit, page.Offset = normalizeMeteringPage(limit, offset)
	tenant, err := merchant.Require(ctx)
	if err != nil {
		return page, err
	}

	err = s.db.RunInMerchantConn(ctx, func(ctx context.Context) error {
		queries := gen.New(s.db.Qx(ctx))
		exists, err := queries.UsageMeterExists(ctx, gen.UsageMeterExistsParams{
			MerchantID: tenant.UUID(),
			MeterKey:   meterKey,
		})
		if err != nil {
			return fmt.Errorf("check usage meter: %w", err)
		}
		if !exists {
			return ErrUsageMeterNotFound
		}

		page.Total, err = queries.CountUsageMeterOverrides(ctx, gen.CountUsageMeterOverridesParams{
			MerchantID: tenant.UUID(),
			MeterKey:   meterKey,
		})
		if err != nil {
			return fmt.Errorf("count usage meter overrides: %w", err)
		}

		rows, err := queries.ListUsageMeterOverrides(ctx, gen.ListUsageMeterOverridesParams{
			MerchantID: tenant.UUID(),
			MeterKey:   meterKey,
			PageLimit:  int32(page.Limit),
			PageOffset: int32(page.Offset),
		})
		if err != nil {
			return fmt.Errorf("list usage meter overrides: %w", err)
		}

		page.Items = make([]UsageMeterOverride, 0, len(rows))
		for _, row := range rows {
			if row.CustomerID == nil {
				return fmt.Errorf("usage meter override has no customer")
			}
			item := UsageMeterOverride{
				CustomerID: *row.CustomerID,
				Subject:    row.Subject,
				CreatedAt:  row.CreatedAt,
				UpdatedAt:  row.UpdatedAt,
			}
			if row.Email != nil {
				item.Email = *row.Email
			}
			if err := decodeRateCard(row.Price, row.Allowance, &item.Price, &item.Allowance); err != nil {
				return fmt.Errorf("decode usage meter override for customer %s: %w", item.CustomerID, err)
			}
			page.Items = append(page.Items, item)
		}
		return nil
	})
	return page, err
}

type usageMeterRecord struct {
	key                string
	eventType          string
	effectiveEventType string
	valueProperty      string
	aggregation        string
	unit               string
	groupBy            []byte
	createdAt          time.Time
	updatedAt          time.Time
	overrideCount      int64
	hasActivity        bool
	lastEventAt        *time.Time
	cardID             *uuid.UUID
	productID          *uuid.UUID
	productKey         *string
	filter             []byte
	price              []byte
	allowance          []byte
	cardCreatedAt      *time.Time
	cardUpdatedAt      *time.Time
}

func usageMeterFromListRow(row gen.ListUsageMetersWithCatalogRow) (UsageMeter, error) {
	return usageMeterFromRecord(usageMeterRecord{
		key: row.Key, eventType: row.EventType, effectiveEventType: row.EffectiveEventType,
		valueProperty: row.ValueProperty, aggregation: row.Aggregation, unit: row.Unit,
		groupBy: row.GroupBy, createdAt: row.CreatedAt, updatedAt: row.UpdatedAt,
		overrideCount: row.OverrideCount, hasActivity: row.HasActivity, lastEventAt: row.LastEventAt,
		cardID: row.CardID, productID: row.ProductID, productKey: row.ProductKey,
		filter: row.Filter, price: row.Price, allowance: row.Allowance,
		cardCreatedAt: row.CardCreatedAt, cardUpdatedAt: row.CardUpdatedAt,
	})
}

func usageMeterFromGetRow(row gen.GetUsageMeterWithCatalogRow) (UsageMeter, error) {
	return usageMeterFromRecord(usageMeterRecord{
		key: row.Key, eventType: row.EventType, effectiveEventType: row.EffectiveEventType,
		valueProperty: row.ValueProperty, aggregation: row.Aggregation, unit: row.Unit,
		groupBy: row.GroupBy, createdAt: row.CreatedAt, updatedAt: row.UpdatedAt,
		overrideCount: row.OverrideCount, hasActivity: row.HasActivity, lastEventAt: row.LastEventAt,
		cardID: row.CardID, productID: row.ProductID, productKey: row.ProductKey,
		filter: row.Filter, price: row.Price, allowance: row.Allowance,
		cardCreatedAt: row.CardCreatedAt, cardUpdatedAt: row.CardUpdatedAt,
	})
}

func usageMeterFromRecord(row usageMeterRecord) (UsageMeter, error) {
	var meter UsageMeter
	meter.Key = row.key
	meter.EventType = row.eventType
	meter.EffectiveEventType = row.effectiveEventType
	meter.ValueProperty = row.valueProperty
	meter.Aggregation = row.aggregation
	meter.Unit = row.unit
	meter.CreatedAt = row.createdAt
	meter.UpdatedAt = row.updatedAt
	meter.OverrideCount = row.overrideCount
	meter.HasActivity = row.hasActivity
	meter.LastEventAt = row.lastEventAt
	if err := json.Unmarshal(row.groupBy, &meter.GroupBy); err != nil {
		return meter, fmt.Errorf("decode meter %q group_by: %w", meter.Key, err)
	}
	if meter.GroupBy == nil {
		meter.GroupBy = map[string]string{}
	}
	meter.BillingSupported = pricing.BillingSupported(meter.Aggregation)
	if row.cardID == nil {
		return meter, nil
	}
	if row.productID == nil || row.productKey == nil || row.cardCreatedAt == nil || row.cardUpdatedAt == nil {
		return meter, fmt.Errorf("meter %q default rate card is incomplete", meter.Key)
	}
	card := DefaultUsageRateCard{
		ID:         *row.cardID,
		ProductID:  *row.productID,
		ProductKey: *row.productKey,
		CreatedAt:  *row.cardCreatedAt,
		UpdatedAt:  *row.cardUpdatedAt,
	}
	if err := json.Unmarshal(row.filter, &card.Filter); err != nil {
		return meter, fmt.Errorf("decode meter %q rate card filter: %w", meter.Key, err)
	}
	if card.Filter == nil {
		card.Filter = map[string][]string{}
	}
	if err := decodeRateCard(row.price, row.allowance, &card.Price, &card.Allowance); err != nil {
		return meter, fmt.Errorf("decode meter %q default rate card: %w", meter.Key, err)
	}
	meter.DefaultRateCard = &card
	return meter, nil
}

func decodeRateCard(
	priceJSON []byte,
	allowanceJSON []byte,
	price *pricing.RatePrice,
	allowance **pricing.Allowance,
) error {
	if err := json.Unmarshal(priceJSON, price); err != nil {
		return fmt.Errorf("decode price: %w", err)
	}
	if len(allowanceJSON) == 0 {
		return nil
	}
	var value pricing.Allowance
	if err := json.Unmarshal(allowanceJSON, &value); err != nil {
		return fmt.Errorf("decode allowance: %w", err)
	}
	*allowance = &value
	return nil
}

func normalizeMeteringPage(limit, offset int) (int, int) {
	if limit <= 0 {
		limit = defaultMeteringPageSize
	}
	if limit > maxMeteringPageSize {
		limit = maxMeteringPageSize
	}
	if offset < 0 {
		offset = 0
	}
	return limit, offset
}

func usageMeterSemanticsEqual(left, right pricing.Meter) bool {
	return left.EventType == right.EventType &&
		left.ValueProperty == right.ValueProperty &&
		left.Aggregation == right.Aggregation &&
		left.Unit == right.Unit &&
		maps.Equal(left.GroupBy, right.GroupBy)
}

func usageMeterHasActivity(
	ctx context.Context,
	tx pgx.Tx,
	merchantID uuid.UUID,
	existing pricing.Meter,
	replacement pricing.Meter,
) (bool, error) {
	eventTypes := []string{effectiveMeterEventType(existing)}
	replacementEventType := effectiveMeterEventType(replacement)
	if replacementEventType != eventTypes[0] {
		eventTypes = append(eventTypes, replacementEventType)
	}
	// Meter corrections are rare control-plane writes. Hold inserts while the
	// activity predicate is checked so a concurrent report cannot slip between
	// the check and the semantic update and then be reinterpreted.
	queries := gen.New(tx)
	if err := queries.LockUsageEventsForMeterCorrection(ctx); err != nil {
		return false, fmt.Errorf("lock usage activity: %w", err)
	}
	hasActivity, err := queries.UsageEventsExistForTypes(ctx, gen.UsageEventsExistForTypesParams{
		MerchantID: merchantID,
		EventTypes: eventTypes,
	})
	if err != nil {
		return false, fmt.Errorf("check usage meter activity: %w", err)
	}
	return hasActivity, nil
}

func effectiveMeterEventType(meter pricing.Meter) string {
	if meter.EventType != "" {
		return meter.EventType
	}
	return meter.Key
}

func loadUsageMeterForRateCard(
	ctx context.Context,
	tx pgx.Tx,
	merchantID uuid.UUID,
	meterKey string,
) (pricing.Meter, error) {
	var meter pricing.Meter
	row, err := gen.New(tx).GetUsageMeterForUpdate(ctx, gen.GetUsageMeterForUpdateParams{
		MerchantID: merchantID,
		MeterKey:   meterKey,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return meter, ErrUsageMeterNotFound
	}
	if err != nil {
		return meter, fmt.Errorf("load usage meter: %w", err)
	}
	meter.Key = row.Key
	meter.EventType = row.EventType
	meter.ValueProperty = row.ValueProperty
	meter.Aggregation = row.Aggregation
	meter.Unit = row.Unit
	if err := json.Unmarshal(row.GroupBy, &meter.GroupBy); err != nil {
		return meter, fmt.Errorf("decode usage meter group_by: %w", err)
	}
	if meter.GroupBy == nil {
		meter.GroupBy = map[string]string{}
	}
	if err := pricing.ValidateMeter("usage meter", &meter); err != nil {
		return meter, err
	}
	return meter, nil
}

func ensureAllowanceMeter(
	ctx context.Context,
	tx pgx.Tx,
	merchantID uuid.UUID,
	allowance *pricing.Allowance,
) error {
	if allowance == nil || allowance.AccrueFrom == "" {
		return nil
	}
	exists, err := gen.New(tx).UsageMeterExists(ctx, gen.UsageMeterExistsParams{
		MerchantID: merchantID,
		MeterKey:   allowance.AccrueFrom,
	})
	if err != nil {
		return fmt.Errorf("check allowance source meter: %w", err)
	}
	if !exists {
		return fmt.Errorf("%w: %q", ErrAllowanceMeterNotFound, allowance.AccrueFrom)
	}
	return nil
}

func loadDefaultRateCardCurrency(
	ctx context.Context,
	tx pgx.Tx,
	merchantID uuid.UUID,
	meterKey string,
) (string, error) {
	priceJSON, err := gen.New(tx).GetDefaultUsageRateCardPriceForUpdate(
		ctx,
		gen.GetDefaultUsageRateCardPriceForUpdateParams{MerchantID: merchantID, MeterKey: meterKey},
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrDefaultRateCardRequired
	}
	if err != nil {
		return "", fmt.Errorf("load default usage rate card: %w", err)
	}
	var price pricing.RatePrice
	if err := json.Unmarshal(priceJSON, &price); err != nil {
		return "", fmt.Errorf("decode default usage rate card price: %w", err)
	}
	return strings.ToUpper(strings.TrimSpace(price.Currency)), nil
}

func ensureActiveProduct(
	ctx context.Context,
	tx pgx.Tx,
	merchantID uuid.UUID,
	productID uuid.UUID,
) error {
	_, err := gen.New(tx).GetActiveMeteringProductForShare(ctx, gen.GetActiveMeteringProductForShareParams{
		MerchantID: merchantID,
		ProductID:  productID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrRateCardProductNotFound
	}
	if err != nil {
		return fmt.Errorf("check rate card product: %w", err)
	}
	return nil
}
