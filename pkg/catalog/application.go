package catalog

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

const (
	ApplicationSchemaVersion = 1
	MaxApplicationBytes      = 1 << 20
	MaxApplicationItems      = 2000
)

// Field distinguishes an omitted update from a value or an explicit null.
// The enclosing declaration uses omitzero so absence survives a Client roundtrip.
type Field[T any] struct {
	Set   bool
	Null  bool
	Value T
}

func Value[T any](v T) Field[T] { return Field[T]{Set: true, Value: v} }
func Null[T any]() Field[T]     { return Field[T]{Set: true, Null: true} }
func (f Field[T]) IsZero() bool { return !f.Set }
func (f Field[T]) MarshalJSON() ([]byte, error) {
	if !f.Set || f.Null {
		return []byte("null"), nil
	}
	// All int64 declaration fields are money; keep SDK JSON precision exact.
	if v, ok := any(f.Value).(int64); ok {
		return json.Marshal(strconv.FormatInt(v, 10))
	}
	return json.Marshal(f.Value)
}
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

// Application is one merchant-authorized batch, not continuously enforced state.
type Application struct {
	SchemaVersion    int            `json:"schema_version"`
	ApplicationID    string         `json:"application_id"`
	ExpectedRevision *int64         `json:"expected_revision"`
	CatalogID        string         `json:"catalog_id,omitempty"`
	Prune            bool           `json:"prune,omitempty"`
	Products         []ApplyProduct `json:"products,omitempty"`
	Meters           []ApplyMeter   `json:"meters,omitempty"`
}

type ApplyMeter struct {
	Key           string                   `json:"key"`
	EventType     Field[string]            `json:"event_type,omitzero"`
	ValueProperty Field[string]            `json:"value_property,omitzero"`
	Aggregation   Field[string]            `json:"aggregation,omitzero"`
	Unit          Field[string]            `json:"unit,omitzero"`
	GroupBy       Field[map[string]string] `json:"group_by,omitzero"`
}

type ApplyProduct struct {
	Key              string                 `json:"key"`
	DisplayName      Field[string]          `json:"display_name,omitzero"`
	Description      Field[string]          `json:"description,omitzero"`
	TierGroup        Field[string]          `json:"tier_group,omitzero"`
	TierRank         Field[int]             `json:"tier_rank,omitzero"`
	Archived         Field[bool]            `json:"archived,omitzero"`
	EntitlementsSpec Field[map[string]*int] `json:"entitlements_spec,omitzero"`
	Prices           []ApplyPrice           `json:"prices,omitempty"`
	RateCards        Field[[]RateCard]      `json:"rate_cards,omitzero"`
}

type ApplyPrice struct {
	Key                 string                              `json:"key"`
	ID                  string                              `json:"id,omitempty"`
	Currency            Field[string]                       `json:"currency,omitzero"`
	UnitAmount          Field[int64]                        `json:"unit_amount,omitzero"`
	AccessDurationHours Field[int]                          `json:"access_duration_hours,omitzero"`
	AutoRenew           Field[bool]                         `json:"auto_renew,omitzero"`
	Archived            Field[bool]                         `json:"archived,omitzero"`
	TrialUnitAmount     Field[int64]                        `json:"trial_unit_amount,omitzero"`
	TrialDurationHours  Field[int]                          `json:"trial_duration_hours,omitzero"`
	PSPs                Field[[]string]                     `json:"psps,omitzero"`
	PSPLinks            Field[map[string]map[string]string] `json:"psp_links,omitzero"`
}

// Validate checks the bounded syntax only; row-dependent validation happens
// after replay lookup inside the engine's transaction.
func (a Application) Validate() error {
	if a.SchemaVersion != ApplicationSchemaVersion {
		return fmt.Errorf("unsupported catalog application schema_version %d", a.SchemaVersion)
	}
	if a.ApplicationID == "" || len(a.ApplicationID) > 128 || strings.TrimSpace(a.ApplicationID) != a.ApplicationID {
		return fmt.Errorf("application_id must be a nonempty identifier of at most 128 bytes")
	}
	if a.ExpectedRevision == nil || *a.ExpectedRevision < 0 {
		return fmt.Errorf("nonnegative expected_revision is required")
	}
	products, prices, meters := map[string]bool{}, map[string]bool{}, map[string]bool{}
	count := len(a.Products) + len(a.Meters)
	for _, p := range a.Products {
		if err := uniqueApplicationKey(products, p.Key, "product"); err != nil {
			return err
		}
		if p.DisplayName.Null || p.Description.Null || p.TierRank.Null || p.Archived.Null {
			return fmt.Errorf("product %q: nonnullable field is null", p.Key)
		}
		if len(p.DisplayName.Value) > 1024 || len(p.Description.Value) > 16384 {
			return fmt.Errorf("product %q: text exceeds catalog limit", p.Key)
		}
		count += len(p.Prices) + len(p.RateCards.Value)
		for _, price := range p.Prices {
			if err := uniqueApplicationKey(prices, price.Key, "price"); err != nil {
				return err
			}
			if price.Currency.Null || price.UnitAmount.Null || price.AutoRenew.Null || price.Archived.Null {
				return fmt.Errorf("price %q: nonnullable field is null", price.Key)
			}
			if price.UnitAmount.Set && price.UnitAmount.Value < 0 || price.TrialUnitAmount.Set && !price.TrialUnitAmount.Null && price.TrialUnitAmount.Value < 0 {
				return fmt.Errorf("price %q: money cannot be negative", price.Key)
			}
			if price.AccessDurationHours.Set && !price.AccessDurationHours.Null && price.AccessDurationHours.Value <= 0 || price.TrialDurationHours.Set && !price.TrialDurationHours.Null && price.TrialDurationHours.Value <= 0 {
				return fmt.Errorf("price %q: duration must be positive or null", price.Key)
			}
		}
	}
	for _, m := range a.Meters {
		if err := uniqueApplicationKey(meters, m.Key, "meter"); err != nil {
			return err
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

func uniqueApplicationKey(seen map[string]bool, key, kind string) error {
	if key == "" || len(key) > 255 || strings.TrimSpace(key) != key {
		return fmt.Errorf("%s key must be a nonempty identifier of at most 255 bytes", kind)
	}
	if seen[key] {
		return fmt.Errorf("duplicate %s key %q", kind, key)
	}
	seen[key] = true
	return nil
}

// CanonicalDigest depends only on submitted values, never current database state.
func (a Application) CanonicalDigest() ([32]byte, error) {
	if err := a.Validate(); err != nil {
		return [32]byte{}, err
	}
	a.Products = append([]ApplyProduct(nil), a.Products...)
	for i := range a.Products {
		a.Products[i].Prices = append([]ApplyPrice(nil), a.Products[i].Prices...)
		for j := range a.Products[i].Prices {
			p := &a.Products[i].Prices[j]
			p.PSPs.Value = append([]string(nil), p.PSPs.Value...)
			sort.Strings(p.PSPs.Value)
		}
		sort.Slice(a.Products[i].Prices, func(j, k int) bool { return a.Products[i].Prices[j].Key < a.Products[i].Prices[k].Key })
	}
	sort.Slice(a.Products, func(i, j int) bool { return a.Products[i].Key < a.Products[j].Key })
	a.Meters = append([]ApplyMeter(nil), a.Meters...)
	sort.Slice(a.Meters, func(i, j int) bool { return a.Meters[i].Key < a.Meters[j].Key })
	raw, err := json.Marshal(a)
	if err != nil {
		return [32]byte{}, err
	}
	return sha256.Sum256(raw), nil
}
