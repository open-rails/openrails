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
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/catalog"
	identity "github.com/open-rails/openrails/internal/billingidentity"
	"github.com/open-rails/openrails/internal/catalogrules"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/pagination"
)

var (
	ErrUsageMeterNotFound       = errors.New("usage meter not found")
	ErrMeterInUse               = errors.New("usage meter is in use")
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

// meterKeyPosition is the keyset position of a list ordered by one key.
type meterKeyPosition struct {
	Key string `json:"k"`
}

func afterMeterKey(cursor string) (*string, error) {
	var position meterKeyPosition
	present, err := pagination.Decode(cursor, &position)
	if err != nil || !present {
		return nil, err
	}
	if position.Key == "" {
		return nil, pagination.ErrInvalidCursor
	}
	return &position.Key, nil
}

// ListUsageMeters returns one keyset page of meters, by key.
func (s *MoneyService) ListUsageMeters(ctx context.Context, page billing.PageRequest) (billing.ListPage[billing.Meter], error) {
	var out billing.ListPage[billing.Meter]
	if s == nil || s.db == nil {
		return out, fmt.Errorf("money service not initialized")
	}
	limit, err := pagination.Limit(page)
	if err != nil {
		return out, err
	}
	after, err := afterMeterKey(page.Cursor)
	if err != nil {
		return out, err
	}
	tenant, err := merchant.Require(ctx)
	if err != nil {
		return out, err
	}
	err = s.db.RunInMerchantConn(ctx, func(ctx context.Context) error {
		rows, err := gen.New(s.db.Qx(ctx)).ListUsageMetersWithCatalog(ctx, gen.ListUsageMetersWithCatalogParams{
			MerchantID: tenant.UUID(), AfterKey: after, FetchLimit: pagination.Fetch(limit),
		})
		if err != nil {
			return fmt.Errorf("list usage meters: %w", err)
		}
		meters := make([]billing.Meter, 0, len(rows))
		for _, row := range rows {
			meter, err := usageMeterFromListRow(row)
			if err != nil {
				return err
			}
			meters = append(meters, meter)
		}
		out = pagination.Cut(meters, limit, func(m billing.Meter) any { return meterKeyPosition{Key: m.Key} })
		return nil
	})
	return out, err
}

// GetUsageMeter returns one meter and its optional default rate card.
func (s *MoneyService) GetUsageMeter(ctx context.Context, meterKey string) (*billing.Meter, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("money service not initialized")
	}
	meterKey = catalogrules.NormalizeKey(meterKey)
	if meterKey == "" {
		return nil, fmt.Errorf("meter key required")
	}
	tenant, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}

	var meter billing.Meter
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

// rateOverride reads one customer rate card row.
func rateOverride(customer *uuid.UUID, meterKey *string, price, allowance []byte, createdAt, updatedAt time.Time) (billing.RateOverride, error) {
	if customer == nil || meterKey == nil {
		return billing.RateOverride{}, fmt.Errorf("rate override has no customer or meter")
	}
	out := billing.RateOverride{CustomerID: billing.CustomerID(*customer), MeterKey: *meterKey, CreatedAt: createdAt, UpdatedAt: updatedAt}
	if err := decodeRateCard(price, allowance, &out.Price, &out.Allowance); err != nil {
		return out, fmt.Errorf("decode rate override for customer %s meter %q: %w", *customer, *meterKey, err)
	}
	return out, nil
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

func usageMeterFromListRow(row gen.ListUsageMetersWithCatalogRow) (billing.Meter, error) {
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

func usageMeterFromGetRow(row gen.GetUsageMeterWithCatalogRow) (billing.Meter, error) {
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

// usageMeterFromRecord reads a meter row; its event type is the effective
// one (the key when none is declared).
func usageMeterFromRecord(row usageMeterRecord) (billing.Meter, error) {
	meter := billing.Meter{
		Key: row.key, EventType: row.effectiveEventType, ValueProperty: row.valueProperty,
		Aggregation: catalog.Aggregation(row.aggregation), Unit: row.unit,
		OverrideCount: row.overrideCount, HasActivity: row.hasActivity, LastEventAt: row.lastEventAt,
		CreatedAt: row.createdAt, UpdatedAt: row.updatedAt,
	}
	if err := json.Unmarshal(row.groupBy, &meter.GroupBy); err != nil {
		return meter, fmt.Errorf("decode meter %q group_by: %w", meter.Key, err)
	}
	if meter.GroupBy == nil {
		meter.GroupBy = map[string]string{}
	}
	meter.BillingSupported = catalogrules.BillingSupported(meter.Aggregation)
	if row.cardID == nil {
		return meter, nil
	}
	if row.productID == nil || row.productKey == nil || row.cardCreatedAt == nil || row.cardUpdatedAt == nil {
		return meter, fmt.Errorf("meter %q rate card is incomplete", meter.Key)
	}
	card := billing.MeterRateCard{
		ProductID:  billing.ProductID(*row.productID),
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
		return meter, fmt.Errorf("decode meter %q rate card: %w", meter.Key, err)
	}
	meter.RateCard = &card
	return meter, nil
}

func decodeRateCard(
	priceJSON []byte,
	allowanceJSON []byte,
	price *catalog.RatePrice,
	allowance **catalog.Allowance,
) error {
	if err := json.Unmarshal(priceJSON, price); err != nil {
		return fmt.Errorf("decode price: %w", err)
	}
	if len(allowanceJSON) == 0 {
		return nil
	}
	var value catalog.Allowance
	if err := json.Unmarshal(allowanceJSON, &value); err != nil {
		return fmt.Errorf("decode allowance: %w", err)
	}
	*allowance = &value
	return nil
}

func usageMeterSemanticsEqual(left, right catalogrules.Meter) bool {
	return effectiveMeterEventType(left) == effectiveMeterEventType(right) &&
		left.ValueProperty == right.ValueProperty &&
		left.Aggregation == right.Aggregation &&
		left.Unit == right.Unit &&
		maps.Equal(left.GroupBy, right.GroupBy)
}

func usageMeterHasActivity(
	ctx context.Context,
	tx pgx.Tx,
	merchantID uuid.UUID,
	existing catalogrules.Meter,
	replacement catalogrules.Meter,
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

func effectiveMeterEventType(meter catalogrules.Meter) string {
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
) (catalogrules.Meter, error) {
	var meter catalogrules.Meter
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
	meter.Aggregation = catalog.Aggregation(row.Aggregation)
	meter.Unit = row.Unit
	if err := json.Unmarshal(row.GroupBy, &meter.GroupBy); err != nil {
		return meter, fmt.Errorf("decode usage meter group_by: %w", err)
	}
	if meter.GroupBy == nil {
		meter.GroupBy = map[string]string{}
	}
	if err := catalogrules.ValidateMeter("usage meter", &meter); err != nil {
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
	allowance *catalog.Allowance,
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

	var defaultPrice *catalog.RatePrice
	var payerPrice *catalog.RatePrice
	prices := make([]catalog.RatePrice, 0, len(rows))
	for _, row := range rows {
		var price catalog.RatePrice
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
	replacement catalogrules.Meter,
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
		var price catalog.RatePrice
		if err := json.Unmarshal(row.Price, &price); err != nil {
			return fmt.Errorf("decode usage meter rate card price: %w", err)
		}
		if err := catalogrules.ValidateDimensions("usage rate card", replacement.GroupBy, filter, &price); err != nil {
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
		var price catalog.RatePrice
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
	meter catalogrules.Meter,
	price catalog.RatePrice,
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
	meter catalogrules.Meter,
	price catalog.RatePrice,
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

func validateAllowanceSourcePrice(meter catalogrules.Meter, price catalog.RatePrice, currency string) error {
	if !catalogrules.BillingSupported(meter.Aggregation) {
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
	if price.Model != catalog.ModelPerUnit || price.PerUnit == nil || price.PerUnit.Matrix == nil {
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
	var price catalog.RatePrice
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
