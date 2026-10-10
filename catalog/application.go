package catalog

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strconv"
	"strings"
)

const (
	// ApplicationSchemaVersion is the document format version an Application
	// declares in schema_version.
	ApplicationSchemaVersion = 1
	// MaxApplicationBytes and MaxApplicationItems bound one document.
	MaxApplicationBytes = 1 << 20
	MaxApplicationItems = 2000
)

// Field distinguishes an omitted update from a value or an explicit null.
// The enclosing declaration uses omitzero so absence survives a Client roundtrip.
type Field[T any] struct {
	Set   bool
	Null  bool
	Value T
}

// Value is a Field set to v.
func Value[T any](v T) Field[T] { return Field[T]{Set: true, Value: v} }

// Null is a Field set to null: it clears the value.
func Null[T any]() Field[T] { return Field[T]{Set: true, Null: true} }

// IsZero reports an omitted field (json omitzero).
func (f Field[T]) IsZero() bool { return !f.Set }
func (f Field[T]) validateField() error {
	if !f.Set && f.Null || (!f.Set || f.Null) && !reflect.ValueOf(&f.Value).Elem().IsZero() {
		return fmt.Errorf("omitted or null field cannot carry a hidden value")
	}
	return nil
}

// MarshalJSON writes the value, or null for an omitted or null field. An int64
// is money and travels as a decimal string.
func (f Field[T]) MarshalJSON() ([]byte, error) {
	if err := f.validateField(); err != nil {
		return nil, err
	}
	if !f.Set || f.Null {
		return []byte("null"), nil
	}
	// All int64 declaration fields are money; keep SDK JSON precision exact.
	if v, ok := any(f.Value).(int64); ok {
		return json.Marshal(strconv.FormatInt(v, 10))
	}
	return json.Marshal(f.Value)
}

// UnmarshalJSON sets the field; null sets it to null. Unknown fields inside
// the value are refused.
func (f *Field[T]) UnmarshalJSON(raw []byte) error {
	*f = Field[T]{Set: true}
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		f.Null = true
		return nil
	}
	if value, ok := any(&f.Value).(*int64); ok {
		text := string(raw)
		if len(raw) > 0 && raw[0] == '"' {
			if err := json.Unmarshal(raw, &text); err != nil {
				return err
			}
		}
		parsed, err := strconv.ParseInt(text, 10, 64)
		if err != nil {
			return fmt.Errorf("money must be an exact int64: %w", err)
		}
		*value = parsed
		return nil
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	return decoder.Decode(&f.Value)
}

// Application is one atomic merchant catalog batch. Products, prices and
// meters are maps keyed by their key. Its canonical content hash is its
// permanent replay identity; omitted records and fields are preserved.
type Application struct {
	SchemaVersion int  `json:"schema_version"`
	Prune         bool `json:"prune,omitempty"`
	// EntitlementReplacements rename or remove a key across every product
	// granting it, before Products apply.
	EntitlementReplacements []EntitlementReplacement `json:"entitlement_replacements,omitempty"`
	Products                map[string]ApplyProduct  `json:"products,omitempty"`
	Meters                  map[string]ApplyMeter    `json:"meters,omitempty"`
}

// MaxEntitlementReplacements bounds the replacements of one application.
const MaxEntitlementReplacements = 100

// EntitlementReplacement moves every product granting From to grant To
// instead; an empty To removes From. A key is not both replaced and a
// replacement in one application.
type EntitlementReplacement struct {
	From string `json:"from"`
	To   string `json:"to,omitempty"`
}

func validateReplacements(pairs []EntitlementReplacement) error {
	if len(pairs) > MaxEntitlementReplacements {
		return fmt.Errorf("at most %d entitlement_replacements", MaxEntitlementReplacements)
	}
	from := map[string]bool{}
	to := map[string]bool{}
	for _, pair := range pairs {
		keys := []string{pair.From}
		if pair.To != "" {
			keys = append(keys, pair.To)
		}
		if _, err := NormalizeEntitlements(keys); err != nil {
			return fmt.Errorf("entitlement_replacements: %w", err)
		}
		if from[pair.From] {
			return fmt.Errorf("entitlement_replacements: duplicate from key %q", pair.From)
		}
		from[pair.From] = true
		if pair.To != "" {
			to[pair.To] = true
		}
	}
	for key := range from {
		if to[key] {
			return fmt.Errorf("entitlement_replacements: key %q is both replaced and a replacement", key)
		}
	}
	return nil
}

// ApplyMeter declares the meter its map key names; omitted fields keep their
// values.
type ApplyMeter struct {
	EventType     Field[string]            `json:"event_type,omitzero"`
	ValueProperty Field[string]            `json:"value_property,omitzero"`
	Aggregation   Field[Aggregation]       `json:"aggregation,omitzero"`
	Unit          Field[string]            `json:"unit,omitzero"`
	GroupBy       Field[map[string]string] `json:"group_by,omitzero"`
}

// ApplyProduct declares the product its map key names, with its prices and
// rate cards; omitted fields keep their values.
type ApplyProduct struct {
	DisplayName Field[string] `json:"display_name,omitzero"`
	Description Field[string] `json:"description,omitzero"`
	TierGroup   Field[string] `json:"tier_group,omitzero"`
	TierRank    Field[int]    `json:"tier_rank,omitzero"`
	Archived    Field[bool]   `json:"archived,omitzero"`
	// Ownership declares the product's rule; null derives it.
	Ownership    Field[Ownership]       `json:"ownership,omitzero"`
	Entitlements Field[[]string]        `json:"entitlements,omitzero"`
	CreditGrant  Field[CreditGrantSpec] `json:"credit_grant,omitzero"`
	Prices       map[string]ApplyPrice  `json:"prices,omitempty"`
	RateCards    Field[[]RateCard]      `json:"rate_cards,omitzero"`
}

// ApplyPrice declares the price its map key names. Its money terms are its
// identity: a declaration with other terms under the same key is a new
// version of the price, and the previous one is archived. ID pins the
// declaration to one existing price.
type ApplyPrice struct {
	ID                   string                              `json:"id,omitempty"`
	Currency             Field[string]                       `json:"currency,omitzero"`
	UnitAmount           Field[int64]                        `json:"unit_amount,omitzero"`
	AccessDurationHours  Field[int]                          `json:"access_duration_hours,omitzero"`
	BillingIntervalHours Field[int]                          `json:"billing_interval_hours,omitzero"`
	Archived             Field[bool]                         `json:"archived,omitzero"`
	TrialUnitAmount      Field[int64]                        `json:"trial_unit_amount,omitzero"`
	TrialDurationHours   Field[int]                          `json:"trial_duration_hours,omitzero"`
	CustomerAmount       Field[CustomerAmount]               `json:"customer_amount,omitzero"`
	Quantity             Field[Quantity]                     `json:"quantity,omitzero"`
	PSPs                 Field[[]string]                     `json:"psps,omitzero"`
	PSPLinks             Field[map[string]map[string]string] `json:"psp_links,omitzero"`
}

// Validate checks the bounded syntax only; row-dependent validation happens
// after replay lookup inside the engine's transaction.
func (a Application) Validate() error {
	if a.SchemaVersion != ApplicationSchemaVersion {
		return fmt.Errorf("unsupported catalog application schema_version %d", a.SchemaVersion)
	}
	if err := validateReplacements(a.EntitlementReplacements); err != nil {
		return err
	}
	count := len(a.Products) + len(a.Meters) + len(a.EntitlementReplacements)
	for _, key := range slices.Sorted(maps.Keys(a.Products)) {
		p := a.Products[key]
		if err := validApplicationKey(key, "product"); err != nil {
			return err
		}
		if err := validateApplicationFields(p); err != nil {
			return fmt.Errorf("product %q: %w", key, err)
		}
		if p.DisplayName.Null || p.Description.Null || p.TierRank.Null || p.Archived.Null {
			return fmt.Errorf("product %q: nonnullable field is null", key)
		}
		if err := p.Ownership.Value.Validate(); err != nil {
			return fmt.Errorf("product %q: %w", key, err)
		}
		if p.Entitlements.Set && (p.Entitlements.Null || p.Entitlements.Value == nil) {
			return fmt.Errorf("product %q: entitlements must be a string list, not null; use [] for none", key)
		}
		if _, err := NormalizeProductEntitlements(p.Entitlements.Value); err != nil {
			return fmt.Errorf("product %q: %w", key, err)
		}
		if len(p.DisplayName.Value) > 1024 || len(p.Description.Value) > 16384 {
			return fmt.Errorf("product %q: text exceeds catalog limit", key)
		}
		count += len(p.Prices) + len(p.RateCards.Value)
		for _, priceKey := range slices.Sorted(maps.Keys(p.Prices)) {
			price := p.Prices[priceKey]
			if err := validApplicationKey(priceKey, "price"); err != nil {
				return fmt.Errorf("product %q: %w", key, err)
			}
			if err := validateApplicationFields(price); err != nil {
				return fmt.Errorf("price %q: %w", priceKey, err)
			}
			if price.Currency.Null || price.UnitAmount.Null || price.Archived.Null {
				return fmt.Errorf("price %q: nonnullable field is null", priceKey)
			}
			if price.UnitAmount.Set && price.UnitAmount.Value < 0 || price.TrialUnitAmount.Set && !price.TrialUnitAmount.Null && price.TrialUnitAmount.Value < 0 {
				return fmt.Errorf("price %q: money cannot be negative", priceKey)
			}
			if price.Quantity.Set && !price.Quantity.Null {
				if err := price.Quantity.Value.Validate(); err != nil {
					return fmt.Errorf("price %q: %w", priceKey, err)
				}
			}
			for _, duration := range []Field[int]{price.BillingIntervalHours, price.AccessDurationHours, price.TrialDurationHours} {
				if duration.Set && !duration.Null && (duration.Value <= 0 || duration.Value > MaxDurationHours) {
					return fmt.Errorf("price %q: duration must be between 1 and %d hours or null", priceKey, MaxDurationHours)
				}
			}
		}
	}
	for _, key := range slices.Sorted(maps.Keys(a.Meters)) {
		if err := validApplicationKey(key, "meter"); err != nil {
			return err
		}
		if err := validateApplicationFields(a.Meters[key]); err != nil {
			return fmt.Errorf("meter %q: %w", key, err)
		}
	}
	if count > MaxApplicationItems {
		return fmt.Errorf("catalog application exceeds %d items", MaxApplicationItems)
	}
	raw, err := json.Marshal(a)
	if err != nil {
		return err
	}
	if len(raw) > MaxApplicationBytes {
		return fmt.Errorf("catalog application exceeds %d bytes", MaxApplicationBytes)
	}
	return nil
}

// Check every presence-aware field before hashing or executing a direct Go
// request. The operator and JSON paths must interpret identical values.
func validateApplicationFields(value any) error {
	v := reflect.ValueOf(value)
	for i := 0; i < v.NumField(); i++ {
		if f, ok := v.Field(i).Interface().(interface{ validateField() error }); ok {
			if err := f.validateField(); err != nil {
				return fmt.Errorf("%s: %w", v.Type().Field(i).Name, err)
			}
		}
	}
	return nil
}

// A map key is the record's key: duplicates are refused while decoding.
func validApplicationKey(key, kind string) error {
	if key == "" || len(key) > 255 || strings.TrimSpace(key) != key {
		return fmt.Errorf("%s key %q must be a nonempty identifier of at most 255 bytes", kind, key)
	}
	return nil
}

// CanonicalDigest depends only on submitted values, never current database
// state. JSON writes map keys sorted, so declaration order never matters.
func (a Application) CanonicalDigest() ([32]byte, error) {
	if err := a.Validate(); err != nil {
		return [32]byte{}, err
	}
	products := make(map[string]ApplyProduct, len(a.Products))
	for key, p := range a.Products {
		p.Entitlements.Value = slices.Clone(p.Entitlements.Value)
		slices.Sort(p.Entitlements.Value)
		prices := make(map[string]ApplyPrice, len(p.Prices))
		for priceKey, price := range p.Prices {
			price.PSPs.Value = slices.Clone(price.PSPs.Value)
			slices.Sort(price.PSPs.Value)
			prices[priceKey] = price
		}
		p.Prices = prices
		products[key] = p
	}
	a.Products = products
	a.EntitlementReplacements = slices.Clone(a.EntitlementReplacements)
	slices.SortFunc(a.EntitlementReplacements, func(x, y EntitlementReplacement) int { return strings.Compare(x.From, y.From) })
	raw, err := json.Marshal(a)
	if err != nil {
		return [32]byte{}, err
	}
	return sha256.Sum256(raw), nil
}
