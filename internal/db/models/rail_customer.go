package models

import (
	"time"

	"github.com/google/uuid"
)

type RailCustomer struct {
	ID uuid.UUID `json:"id"`
	// CustomerID is the OpenRails payable customer for this row (#317).
	// Join openrails.customers for issuer/subject.
	CustomerID uuid.UUID `json:"customer_id,omitempty"`
	Rail       string    `json:"rail"`
	// RailCustomerID is the rail's own external customer id (e.g. Stripe cus_*).
	RailCustomerID string `json:"rail_customer_id"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}
