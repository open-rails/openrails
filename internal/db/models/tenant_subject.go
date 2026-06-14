package models

import (
	"time"

	"github.com/google/uuid"
)

// MerchantSubject is the OpenRails payable identity. It stores the OIDC-style
// external principal once; billing tables reference tenant_subject_id and join
// here when they need tenant/issuer/subject.
type MerchantSubject struct {
	ID         uuid.UUID `json:"id"`
	MerchantID   uuid.UUID `json:"tenant_id"`
	Issuer     string    `json:"issuer"`
	Subject    string    `json:"subject"`
	CreatedAt  time.Time `json:"created_at"`
	LastSeenAt time.Time `json:"last_seen_at"`
}
