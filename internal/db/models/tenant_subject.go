package models

import (
	"time"

	"github.com/google/uuid"
)

// TenantSubject is the OpenRails payable identity. It stores the OIDC-style
// external principal once; billing tables reference tenant_subject_id and join
// here when they need tenant/issuer/subject.
type TenantSubject struct {
	ID         uuid.UUID `json:"id"`
	TenantID   uuid.UUID `json:"tenant_id"`
	Issuer     string    `json:"issuer"`
	Subject    string    `json:"subject"`
	CreatedAt  time.Time `json:"created_at"`
	LastSeenAt time.Time `json:"last_seen_at"`
}
