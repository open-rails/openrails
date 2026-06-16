package models

import (
	"time"

	"github.com/google/uuid"
)

// Customer is the OpenRails payable identity. It stores the OIDC-style
// external principal once; billing tables reference customer_id and join
// here when they need merchant/issuer/subject.
type Customer struct {
	ID         uuid.UUID `json:"id"`
	MerchantID uuid.UUID `json:"merchant_id"`
	Issuer     string    `json:"issuer"`
	Subject    string    `json:"subject"`
	CreatedAt  time.Time `json:"created_at"`
	LastSeenAt time.Time `json:"last_seen_at"`
}
