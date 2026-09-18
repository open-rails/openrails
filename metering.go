package openrails

import (
	"github.com/google/uuid"
	"github.com/open-rails/openrails/pkg/pricing"
	"time"
)

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

type DefaultUsageRateCardDTO struct {
	ID         uuid.UUID           `json:"id"`
	ProductID  ProductID           `json:"product_id"`
	ProductKey string              `json:"product_key"`
	Filter     map[string][]string `json:"filter"`
	Price      pricing.RatePrice   `json:"price"`
	Allowance  *pricing.Allowance  `json:"allowance,omitempty"`
	CreatedAt  time.Time           `json:"created_at"`
	UpdatedAt  time.Time           `json:"updated_at"`
}

type UsageMeterOverrideDTO struct {
	CustomerID CustomerID         `json:"customer_id"`
	Subject    string             `json:"subject,omitempty"`
	Email      string             `json:"email,omitempty"`
	Price      pricing.RatePrice  `json:"price"`
	Allowance  *pricing.Allowance `json:"allowance,omitempty"`
	CreatedAt  time.Time          `json:"created_at"`
	UpdatedAt  time.Time          `json:"updated_at"`
}

type UsageMeterSpec struct {
	Key           string            `json:"key"`
	EventType     string            `json:"event_type"`
	ValueProperty string            `json:"value_property"`
	Aggregation   string            `json:"aggregation"` // sum | count
	Unit          string            `json:"unit,omitempty"`
	GroupBy       map[string]string `json:"group_by,omitempty"`
}

type UsageMeterRequest struct {
	EventType     string            `json:"event_type"`
	ValueProperty string            `json:"value_property"`
	Aggregation   string            `json:"aggregation"`
	Unit          string            `json:"unit,omitempty"`
	GroupBy       map[string]string `json:"group_by,omitempty"`
}

type DefaultUsageRateCardRequest struct {
	ProductID ProductID           `json:"product_id"`
	Filter    map[string][]string `json:"filter"`
	Price     pricing.RatePrice   `json:"price"`
	Allowance *pricing.Allowance  `json:"allowance,omitempty"`
}
