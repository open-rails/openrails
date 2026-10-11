package billing

import (
	"time"

	"github.com/open-rails/openrails/catalog"
)

// Meter is a stream of usage events, aggregated per period into the
// quantity its rate card prices: events of EventType, measured by the
// ValueProperty of each event, grouped by GroupBy's dimensions. RateCard
// prices every customer's usage.
type Meter struct {
	Key           string              `json:"key"`
	EventType     string              `json:"event_type"`
	ValueProperty string              `json:"value_property"`
	Aggregation   catalog.Aggregation `json:"aggregation"`
	Unit          string              `json:"unit"`
	GroupBy       map[string]string   `json:"group_by"`
	// BillingSupported is whether usage of this aggregation can be billed
	// (sum and count).
	BillingSupported bool           `json:"billing_supported"`
	RateCard         *MeterRateCard `json:"rate_card"`
	// Revision advances on every change to the meter or its rate card.
	Revision    int64      `json:"revision"`
	HasActivity bool       `json:"has_activity"`
	LastEventAt *time.Time `json:"last_event_at"`
	CreatedAt   time.Time  `json:"created_at"`
	UpdatedAt   time.Time  `json:"updated_at"`
}

// SetMeterParams declares a meter. EventType defaults to the meter's key; a
// meter with recorded usage keeps its definition. RateCard sets the rate card
// that prices its usage; null removes it, and omitted keeps it.
type SetMeterParams struct {
	EventType     string                             `json:"event_type,omitempty"`
	ValueProperty string                             `json:"value_property,omitempty"`
	Aggregation   catalog.Aggregation                `json:"aggregation"`
	Unit          string                             `json:"unit,omitempty"`
	GroupBy       map[string]string                  `json:"group_by,omitempty"`
	RateCard      catalog.Field[MeterRateCardParams] `json:"rate_card,omitzero"`
	// ExpectedRevision refuses the change with revision_mismatch unless the
	// meter is at this revision; 0 expects no meter.
	ExpectedRevision *int64 `json:"expected_revision,omitempty"`
}

// MeterRateCard is the rate card that prices a meter's usage, under the
// product whose invoices carry it.
type MeterRateCard struct {
	ProductID  ProductID           `json:"product_id"`
	ProductKey string              `json:"product_key"`
	Filter     map[string][]string `json:"filter"`
	Price      catalog.RatePrice   `json:"price"`
	Allowance  *catalog.Allowance  `json:"allowance"`
	CreatedAt  time.Time           `json:"created_at"`
	UpdatedAt  time.Time           `json:"updated_at"`
}

// MeterRateCardParams is a meter's rate card: its price, under the product
// whose invoices carry it.
type MeterRateCardParams struct {
	ProductID ProductID           `json:"product_id"`
	Filter    map[string][]string `json:"filter,omitempty"`
	Price     catalog.RatePrice   `json:"price"`
	Allowance *catalog.Allowance  `json:"allowance,omitempty"`
}
