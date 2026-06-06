package models

import (
	"time"

	"github.com/google/uuid"
	"github.com/uptrace/bun"
)

// TenantSubject is the OpenRails payable identity. It stores the OIDC-style
// external principal once; billing tables reference tenant_subject_id and join
// here when they need tenant/issuer/subject.
type TenantSubject struct {
	bun.BaseModel `bun:"table:billing.tenant_subjects,alias:tsub"`

	ID         uuid.UUID `bun:"id,pk,type:uuid" json:"id"`
	TenantID   uuid.UUID `bun:"tenant_id,type:uuid,notnull" json:"tenant_id"`
	Issuer     string    `bun:"issuer,notnull" json:"issuer"`
	Subject    string    `bun:"subject,notnull" json:"subject"`
	CreatedAt  time.Time `bun:"created_at,notnull" json:"created_at"`
	LastSeenAt time.Time `bun:"last_seen_at,notnull" json:"last_seen_at"`
}
