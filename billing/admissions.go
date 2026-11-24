package billing

import (
	"github.com/google/uuid"
	"github.com/open-rails/openrails/catalog"
)

// InvokerType says whose credential an invoker presents.
type InvokerType string

const (
	// InvokerTypeCustomer is the paying customer's own credential: failed usage
	// draws the customer's grace, then is charged.
	InvokerTypeCustomer InvokerType = "customer"
	// InvokerTypeDelegated is another party acting for the customer: a gate
	// refuses its spend, and its failed usage meets flat cutoffs.
	InvokerTypeDelegated InvokerType = "delegated"
)

// AdmitParams is one request a Gate decides against a customer's money
// (payer capacity, spend windows and failed-usage cutoffs) and the usage
// limits its usage counts toward, on the gate's view: nothing is held on
// the server.
type AdmitParams struct {
	// RequestID names the request: its capture's SourceID by default.
	RequestID   string      `json:"request_id"`
	CustomerID  CustomerID  `json:"customer_id"`
	Invoker     string      `json:"invoker"`
	InvokerType InvokerType `json:"invoker_type"`
	// TrustLevel selects money policy; empty uses the customer's stored level.
	TrustLevel string `json:"trust_level,omitempty"`
	// Resource is attribution only.
	Resource string           `json:"resource,omitempty"`
	Currency catalog.Currency `json:"currency"`
	// EstimatedAmount is a host-stated cost. Usage instead states the usage
	// the request expects, which the meters' rate cards price; the two are
	// exclusive, and with neither the request costs nothing.
	EstimatedAmount int64          `json:"estimated_amount,string"`
	Usage           *UsageEstimate `json:"usage,omitempty"`
	Source          string         `json:"source,omitempty"`
	// Roles are the invoker's role UUIDs; role-scoped spend delegations gate
	// on them.
	Roles []uuid.UUID `json:"roles,omitempty"`
}

// UsageEstimate is the usage a request expects, e.g. {requests: 1} for a
// call or {input_tokens: <prompt>, output_tokens: <max_tokens>} for an LLM
// request: the gate costs it by its view's counts and price bounds.
type UsageEstimate struct {
	// Amount is what a dynamic card charges for this estimate.
	Amount     *int64           `json:"amount,omitempty,string"`
	EventType  string           `json:"event_type"`
	Dimensions map[string]int64 `json:"dimensions,omitempty"`
	Metadata   map[string]any   `json:"metadata,omitempty"`
}

// CaptureUsage is the usage a ticket's capture records: its quantities
// (Dimensions, Metadata) and its idempotency coordinate, Source ("admit"
// when empty) and SourceID (the request id when empty).
type CaptureUsage struct {
	EventType  string           `json:"event_type"`
	Resource   string           `json:"resource,omitempty"`
	Metadata   map[string]any   `json:"metadata,omitempty"`
	Source     string           `json:"source,omitempty"`
	SourceID   string           `json:"source_id,omitempty"`
	Dimensions map[string]int64 `json:"dimensions,omitempty"`
}
