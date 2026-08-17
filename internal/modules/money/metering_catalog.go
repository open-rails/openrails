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
	"github.com/open-rails/openrails/pkg/identity"
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
	ErrAllowanceSourceInvalid   = errors.New("usage rate card allowance source is invalid")
	ErrAllowanceSourceInUse     = errors.New("usage rate card is an allowance source")
	ErrMeterRateCardConflict    = errors.New("usage meter change conflicts with its rate cards")
	ErrUsageRateCardInvalid     = errors.New("usage rate card is invalid")
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

func ensureAllowanceSource(
	ctx context.Context,
	tx pgx.Tx,
	merchantID uuid.UUID,
	targetMeterKey string,
	payer *identity.CustomerID,
	currency string,
	allowance *pricing.Allowance,
) error {
	if allowance == nil || allowance.AccrueFrom == "" {
		return nil
	}
	if allowance.AccrueFrom == targetMeterKey {
		return allowanceSourceInvalid(fmt.Errorf("meter %q cannot accrue an allowance from itself", targetMeterKey))
	}
	sourceMeter, err := loadUsageMeterForRateCard(ctx, tx, merchantID, allowance.AccrueFrom)
	if errors.Is(err, ErrUsageMeterNotFound) {
		return fmt.Errorf("%w: %q", ErrAllowanceMeterNotFound, allowance.AccrueFrom)
	}
	if err != nil {
		return fmt.Errorf("load allowance source meter: %w", err)
	}

	rows, err := gen.New(tx).ListUsageRateCardPricesForUpdate(
		ctx,
		gen.ListUsageRateCardPricesForUpdateParams{
			MerchantID: merchantID,
			MeterKey:   allowance.AccrueFrom,
		},
	)
	if err != nil {
		return fmt.Errorf("load allowance source rate cards: %w", err)
	}

	var defaultPrice *pricing.RatePrice
	var payerPrice *pricing.RatePrice
	prices := make([]pricing.RatePrice, 0, len(rows))
	for _, row := range rows {
		var price pricing.RatePrice
		if err := json.Unmarshal(row.Price, &price); err != nil {
			return fmt.Errorf("decode allowance source rate card: %w", err)
		}
		prices = append(prices, price)
		if row.CustomerID == nil {
			defaultPrice = &prices[len(prices)-1]
		}
		if payer != nil && !payer.IsZero() && row.CustomerID != nil && *row.CustomerID == payer.UUID() {
			payerPrice = &prices[len(prices)-1]
		}
	}
	if defaultPrice == nil {
		return allowanceSourceInvalid(fmt.Errorf("source meter %q has no default rate card", allowance.AccrueFrom))
	}
	if payer != nil && !payer.IsZero() {
		if payerPrice != nil {
			return validateAllowanceSourcePrice(sourceMeter, *payerPrice, currency)
		}
		return validateAllowanceSourcePrice(sourceMeter, *defaultPrice, currency)
	}
	for _, price := range prices {
		if err := validateAllowanceSourcePrice(sourceMeter, price, currency); err != nil {
			return err
		}
	}
	return nil
}

func validateUsageMeterRateCardContracts(
	ctx context.Context,
	queries *gen.Queries,
	merchantID uuid.UUID,
	replacement pricing.Meter,
) error {
	state, err := queries.GetDefaultUsageRateCardStateForUpdate(
		ctx,
		gen.GetDefaultUsageRateCardStateForUpdateParams{
			MerchantID: merchantID,
			MeterKey:   replacement.Key,
		},
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("load usage meter rate card: %w", err)
	}
	var filter map[string][]string
	if err := json.Unmarshal(state.Filter, &filter); err != nil {
		return fmt.Errorf("decode usage meter rate card filter: %w", err)
	}
	prices, err := queries.ListUsageRateCardPricesForUpdate(
		ctx,
		gen.ListUsageRateCardPricesForUpdateParams{
			MerchantID: merchantID,
			MeterKey:   replacement.Key,
		},
	)
	if err != nil {
		return fmt.Errorf("load usage meter rate card prices: %w", err)
	}
	for _, row := range prices {
		var price pricing.RatePrice
		if err := json.Unmarshal(row.Price, &price); err != nil {
			return fmt.Errorf("decode usage meter rate card price: %w", err)
		}
		if err := pricing.ValidateDimensions("usage rate card", replacement.GroupBy, filter, &price); err != nil {
			return meterRateCardConflict(err)
		}
	}

	dependencyCurrencies, err := queries.GetUsageRateCardAllowanceDependencyCurrencies(
		ctx,
		gen.GetUsageRateCardAllowanceDependencyCurrenciesParams{
			MerchantID: merchantID,
			MeterKey:   replacement.Key,
		},
	)
	if err != nil {
		return fmt.Errorf("load allowance dependencies: %w", err)
	}
	for _, row := range prices {
		var price pricing.RatePrice
		if err := json.Unmarshal(row.Price, &price); err != nil {
			return fmt.Errorf("decode allowance source rate card price: %w", err)
		}
		if err := validateAllowanceSourceDependencies(replacement, price, dependencyCurrencies); err != nil {
			return meterRateCardConflict(err)
		}
	}
	return nil
}

func validateRateCardAsAllowanceSource(
	ctx context.Context,
	queries *gen.Queries,
	merchantID uuid.UUID,
	meter pricing.Meter,
	price pricing.RatePrice,
) error {
	dependencyCurrencies, err := queries.GetUsageRateCardAllowanceDependencyCurrencies(
		ctx,
		gen.GetUsageRateCardAllowanceDependencyCurrenciesParams{
			MerchantID: merchantID,
			MeterKey:   meter.Key,
		},
	)
	if err != nil {
		return fmt.Errorf("load allowance dependencies: %w", err)
	}
	if err := validateAllowanceSourceDependencies(meter, price, dependencyCurrencies); err != nil {
		return allowanceSourceInvalid(err)
	}
	return nil
}

func validateAllowanceSourceDependencies(
	meter pricing.Meter,
	price pricing.RatePrice,
	dependencyCurrencies []string,
) error {
	if len(dependencyCurrencies) == 0 {
		return nil
	}
	for _, currency := range dependencyCurrencies {
		if err := validateAllowanceSourcePrice(meter, price, currency); err != nil {
			return err
		}
	}
	return nil
}

func validateAllowanceSourcePrice(meter pricing.Meter, price pricing.RatePrice, currency string) error {
	if !pricing.BillingSupported(meter.Aggregation) {
		return allowanceSourceInvalid(fmt.Errorf(
			"source meter %q aggregation %q is not supported for billing",
			meter.Key,
			meter.Aggregation,
		))
	}
	if price.Currency != currency {
		return allowanceSourceInvalid(fmt.Errorf(
			"source meter %q currency %q does not match %q",
			meter.Key,
			price.Currency,
			currency,
		))
	}
	if price.Model != pricing.ModelPerUnit || price.PerUnit == nil || price.PerUnit.Matrix == nil {
		return allowanceSourceInvalid(fmt.Errorf(
			"source meter %q must use per-unit matrix pricing",
			meter.Key,
		))
	}
	matrix := price.PerUnit.Matrix
	if propertyKey(meter.GroupBy[matrix.Dimension]) == "" || propertyKey(meter.GroupBy["resource_id"]) == "" {
		return allowanceSourceInvalid(fmt.Errorf(
			"source meter %q requires group_by %q and resource_id",
			meter.Key,
			matrix.Dimension,
		))
	}
	for _, cell := range matrix.Cells {
		if cell.Included > 0 {
			return nil
		}
	}
	return allowanceSourceInvalid(fmt.Errorf(
		"source meter %q matrix must include allowance units",
		meter.Key,
	))
}

func invalidUsageRateCard(err error) error {
	return fmt.Errorf("%w: %v", ErrUsageRateCardInvalid, err)
}

func allowanceSourceInvalid(err error) error {
	if errors.Is(err, ErrAllowanceSourceInvalid) {
		return err
	}
	return fmt.Errorf("%w: %v", ErrAllowanceSourceInvalid, err)
}

func meterRateCardConflict(err error) error {
	return fmt.Errorf("%w: %v", ErrMeterRateCardConflict, err)
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
