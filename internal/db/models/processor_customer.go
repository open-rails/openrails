package models

import (
	"time"

	"github.com/google/uuid"
)

type ProcessorCustomer struct {
	ID uuid.UUID `json:"id"`
	// CustomerID is the OpenRails payable customer for this row (#317).
	// Join openrails.customers for issuer/subject.
	CustomerID uuid.UUID `json:"customer_id,omitempty"`
	Processor  string    `json:"processor"`
	// ProcessorCustomerID is the processor's own external customer id (e.g. Stripe cus_*).
	ProcessorCustomerID string `json:"processor_customer_id"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}
