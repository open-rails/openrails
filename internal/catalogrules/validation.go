package catalogrules

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/open-rails/openrails/catalog"
)

// Meter is a meter's definition: what it counts and how it aggregates.
type Meter struct {
	Key           string
	EventType     string
	ValueProperty string
	Aggregation   catalog.Aggregation
	Unit          string
	GroupBy       map[string]string
}

var (
	invalidKeyChars   = regexp.MustCompile(`[^a-z0-9_-]+`)
	validAggregations = map[catalog.Aggregation]struct{}{
		catalog.AggregationSum:         {},
		catalog.AggregationCount:       {},
		catalog.AggregationMax:         {},
		catalog.AggregationMin:         {},
		catalog.AggregationUniqueCount: {},
		catalog.AggregationLatest:      {},
	}
	validRoundModes = map[catalog.Round]struct{}{
		"": {}, catalog.RoundHalfUp: {}, catalog.RoundUp: {}, catalog.RoundDown: {},
	}
)

// NormalizeKey returns the canonical merchant-scoped key used by catalog
// resources.
func NormalizeKey(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	value = invalidKeyChars.ReplaceAllString(value, "-")
	return strings.Trim(value, "-_")
}

// ValidateMeter normalizes and validates a meter definition.
func ValidateMeter(where string, meter *Meter) error {
	if meter == nil {
		return fmt.Errorf("%s: meter is required", where)
	}
	meter.Key = NormalizeKey(meter.Key)
	if meter.Key == "" {
		return fmt.Errorf("%s: key is required", where)
	}
	meter.EventType = strings.TrimSpace(meter.EventType)
	meter.ValueProperty = strings.TrimSpace(meter.ValueProperty)
	meter.Aggregation = catalog.Aggregation(strings.ToLower(strings.TrimSpace(string(meter.Aggregation))))
	meter.Unit = strings.TrimSpace(meter.Unit)
	if meter.Aggregation == "" {
		return fmt.Errorf("%s: aggregation is required", where)
	}
	if _, ok := validAggregations[meter.Aggregation]; !ok {
		return fmt.Errorf("%s: aggregation must be one of sum/count/max/min/unique_count/latest", where)
	}
	if meter.Aggregation == catalog.AggregationSum && meter.ValueProperty == "" {
		return fmt.Errorf("%s: aggregation sum requires value_property", where)
	}
	if meter.Aggregation == catalog.AggregationCount && meter.ValueProperty != "" {
		return fmt.Errorf("%s: aggregation count must not set value_property", where)
	}
	groupBy := make(map[string]string, len(meter.GroupBy))
	for rawDimension, rawProperty := range meter.GroupBy {
		dimension := strings.TrimSpace(rawDimension)
		property := strings.TrimSpace(rawProperty)
		if dimension == "" || property == "" {
			return fmt.Errorf("%s: group_by dimensions and properties must be non-empty", where)
		}
		if _, exists := groupBy[dimension]; exists {
			return fmt.Errorf("%s: duplicate group_by dimension %q", where, dimension)
		}
		groupBy[dimension] = property
	}
	meter.GroupBy = groupBy
	return nil
}

// BillingSupported reports whether the rater supports a meter's aggregation.
func BillingSupported(aggregation catalog.Aggregation) bool {
	switch catalog.Aggregation(strings.ToLower(strings.TrimSpace(string(aggregation)))) {
	case catalog.AggregationSum, catalog.AggregationCount:
		return true
	default:
		return false
	}
}

// ValidateRatePrice normalizes and validates one charge-model price.
func ValidateRatePrice(where string, price *catalog.RatePrice) error {
	if price == nil {
		return fmt.Errorf("%s: price is required", where)
	}
	price.Model = catalog.Model(strings.ToLower(strings.TrimSpace(string(price.Model))))
	if price.Currency != "" {
		price.Currency = strings.ToUpper(strings.TrimSpace(price.Currency))
		if !validRateCurrency(price.Currency) {
			return fmt.Errorf("%s: currency must be an ISO money currency", where)
		}
	}

	blocks := 0
	if price.Flat != nil {
		blocks++
	}
	if price.PerUnit != nil {
		blocks++
	}
	if price.Tiered != nil {
		blocks++
	}
	if price.Package != nil {
		blocks++
	}
	if blocks != 1 {
		return fmt.Errorf("%s: price model requires exactly one matching sub-block", where)
	}

	switch price.Model {
	case catalog.ModelFlat:
		if price.Flat == nil {
			return fmt.Errorf("%s: model flat requires flat block", where)
		}
		if price.Flat.Amount <= 0 {
			return fmt.Errorf("%s: flat price requires a positive amount", where)
		}
	case catalog.ModelPerUnit:
		if price.PerUnit == nil {
			return fmt.Errorf("%s: model per_unit requires per_unit block", where)
		}
		price.PerUnit.Round = catalog.Round(strings.ToLower(strings.TrimSpace(string(price.PerUnit.Round))))
		if _, ok := validRoundModes[price.PerUnit.Round]; !ok {
			return fmt.Errorf("%s: round must be up, down or half_up, got %q", where, price.PerUnit.Round)
		}
		if price.PerUnit.MaximumAmount < 0 {
			return fmt.Errorf("%s: maximum_amount must be >= 0", where)
		}
		if price.PerUnit.Matrix == nil && price.PerUnit.UnitAmount < 0 {
			return fmt.Errorf("%s: per_unit unit_amount must be >= 0", where)
		}
		if price.PerUnit.DivideBy < 0 {
			return fmt.Errorf("%s: divide_by must be >= 0", where)
		}
		if price.PerUnit.Matrix != nil {
			if err := validateMatrix(where, price.PerUnit.Matrix); err != nil {
				return err
			}
		}
	case catalog.ModelTiered:
		if price.Tiered == nil {
			return fmt.Errorf("%s: model tiered requires tiered block", where)
		}
		price.Tiered.Mode = catalog.TierMode(strings.ToLower(strings.TrimSpace(string(price.Tiered.Mode))))
		if price.Tiered.Mode != catalog.TierModeVolume && price.Tiered.Mode != catalog.TierModeGraduated {
			return fmt.Errorf(
				"%s: tiered mode must be %q or %q, got %q",
				where,
				catalog.TierModeVolume,
				catalog.TierModeGraduated,
				price.Tiered.Mode,
			)
		}
		if err := validateTiers(where, price.Tiered.Tiers); err != nil {
			return err
		}
	case catalog.ModelPackage:
		if price.Package == nil {
			return fmt.Errorf("%s: model package requires package block", where)
		}
		if price.Package.PackageSize <= 0 {
			return fmt.Errorf("%s: package_size must be > 0", where)
		}
		if price.Package.Amount <= 0 {
			return fmt.Errorf("%s: package amount must be > 0", where)
		}
		if price.Package.FreeUnits < 0 {
			return fmt.Errorf("%s: free_units must be >= 0", where)
		}
	default:
		return fmt.Errorf("%s: model must be one of flat/per_unit/tiered/package, got %q", where, price.Model)
	}
	return nil
}

// ValidateUsagePrice rejects flat prices, which are not meter-linked usage
// prices, after applying the canonical charge-model validation.
func ValidateUsagePrice(where string, price *catalog.RatePrice) error {
	if err := ValidateRatePrice(where, price); err != nil {
		return err
	}
	if price.Model == catalog.ModelFlat {
		return fmt.Errorf("%s: flat prices cannot be attached to a meter", where)
	}
	return nil
}

// ValidateAllowance normalizes and validates included usage.
func ValidateAllowance(where string, allowance *catalog.Allowance) error {
	if allowance == nil {
		return nil
	}
	if allowance.Included < 0 {
		return fmt.Errorf("%s: allowance.included must be >= 0", where)
	}
	allowance.AccrueFrom = NormalizeKey(allowance.AccrueFrom)
	if allowance.Cap != "" {
		if _, err := ParseDurationSpec(allowance.Cap); err != nil {
			return fmt.Errorf("%s: allowance.cap: %w", where, err)
		}
	}
	return nil
}

// ValidateFilter normalizes a meter filter and rejects ambiguous or empty
// dimensions and values.
func ValidateFilter(where string, filter *map[string][]string) error {
	if filter == nil {
		return fmt.Errorf("%s: filter is required", where)
	}
	normalized := make(map[string][]string, len(*filter))
	for rawKey, rawValues := range *filter {
		key := strings.TrimSpace(rawKey)
		if key == "" {
			return fmt.Errorf("%s: filter keys must be non-empty", where)
		}
		if _, exists := normalized[key]; exists {
			return fmt.Errorf("%s: duplicate filter key %q", where, key)
		}
		values := make([]string, 0, len(rawValues))
		seen := make(map[string]struct{}, len(rawValues))
		for _, rawValue := range rawValues {
			value := strings.TrimSpace(rawValue)
			if value == "" {
				return fmt.Errorf("%s: filter values must be non-empty", where)
			}
			if _, exists := seen[value]; exists {
				continue
			}
			seen[value] = struct{}{}
			values = append(values, value)
		}
		if len(values) == 0 {
			return fmt.Errorf("%s: filter %q must contain at least one value", where, key)
		}
		normalized[key] = values
	}
	*filter = normalized
	return nil
}

// ValidateDimensions verifies that filter and matrix dimensions belong to the
// meter's declared group_by registry.
func ValidateDimensions(
	where string,
	groupBy map[string]string,
	filter map[string][]string,
	price *catalog.RatePrice,
) error {
	if price != nil && price.PerUnit != nil && price.PerUnit.Matrix != nil {
		dimension := price.PerUnit.Matrix.Dimension
		if _, ok := groupBy[dimension]; !ok {
			return fmt.Errorf("%s matrix dimension %q is not a group_by key", where, dimension)
		}
	}
	for key := range filter {
		if _, ok := groupBy[key]; !ok {
			return fmt.Errorf("%s filter key %q is not a group_by key", where, key)
		}
	}
	return nil
}

// ParseDurationSpec parses the catalog duration grammar used by usage windows,
// allowances and metered gauge denominators: whole hours or whole days only.
// "once" is accepted by callers that model non-recurring prices.
func ParseDurationSpec(value string) (time.Duration, error) {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" {
		return 0, fmt.Errorf("duration is required")
	}
	unit := value[len(value)-1:]
	n, err := strconv.ParseInt(strings.TrimSpace(value[:len(value)-1]), 10, 64)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("duration %q must be a positive whole h or d value", value)
	}
	switch unit {
	case "h":
		return time.Duration(n) * time.Hour, nil
	case "d":
		return time.Duration(n) * 24 * time.Hour, nil
	default:
		return 0, fmt.Errorf("duration %q must use h or d", value)
	}
}

func validateTiers(where string, tiers []catalog.RateTier) error {
	if len(tiers) == 0 {
		return fmt.Errorf("%s: tiered price requires tiers", where)
	}
	var previous int64
	for i := range tiers {
		tier := &tiers[i]
		if tier.UnitAmount < 0 || tier.FlatAmount < 0 {
			return fmt.Errorf("%s: tier #%d amounts must be >= 0", where, i+1)
		}
		last := i == len(tiers)-1
		switch {
		case tier.UpTo == nil && !last:
			return fmt.Errorf("%s: only the last tier may be unbounded (up_to omitted)", where)
		case tier.UpTo != nil && last:
			return fmt.Errorf("%s: the last tier must be unbounded (omit up_to)", where)
		case tier.UpTo != nil:
			if *tier.UpTo <= previous {
				return fmt.Errorf(
					"%s: tier up_to values must strictly ascend (got %d after %d)",
					where,
					*tier.UpTo,
					previous,
				)
			}
			previous = *tier.UpTo
		}
	}
	return nil
}

func validateMatrix(where string, matrix *catalog.Matrix) error {
	matrix.Dimension = strings.TrimSpace(matrix.Dimension)
	if matrix.Dimension == "" {
		return fmt.Errorf("%s: matrix requires a dimension", where)
	}
	if len(matrix.Cells) == 0 {
		return fmt.Errorf("%s: matrix requires at least one cell", where)
	}
	for key, cell := range matrix.Cells {
		if cell.UnitAmount < 0 || cell.MaximumAmount < 0 || cell.Included < 0 {
			return fmt.Errorf("%s: matrix cell %q amounts must be >= 0", where, key)
		}
	}
	return nil
}

// validRateCurrency accepts a three-letter upper-case code.
func validRateCurrency(value string) bool {
	if len(value) != 3 {
		return false
	}
	for _, r := range value {
		if r < 'A' || r > 'Z' {
			return false
		}
	}
	return true
}

// ValidateRateCard normalizes and validates one rate card: a flat fee with no
// meter, or a usage price on a meter. Meters are checked to exist by the
// caller.
func ValidateRateCard(where string, rc *catalog.RateCard) error {
	rc.Meter = NormalizeKey(rc.Meter)
	rc.PaymentTerm = catalog.PaymentTerm(strings.ToLower(strings.TrimSpace(string(rc.PaymentTerm))))
	switch rc.PaymentTerm {
	case "", catalog.PaymentInAdvance, catalog.PaymentInArrears:
	default:
		return fmt.Errorf("%s: payment_term must be %q or %q", where, catalog.PaymentInAdvance, catalog.PaymentInArrears)
	}
	if err := ValidateRatePrice(where, &rc.Price); err != nil {
		return err
	}
	if rc.Price.Model == catalog.ModelFlat {
		if rc.Meter != "" {
			return fmt.Errorf("%s: a flat rate card must not reference a meter", where)
		}
		if rc.Allowance != nil {
			return fmt.Errorf("%s: a flat rate card cannot have an allowance", where)
		}
		return nil
	}
	if rc.Meter == "" {
		return fmt.Errorf("%s: a %s rate card requires a meter", where, rc.Price.Model)
	}
	return ValidateAllowance(where, rc.Allowance)
}
