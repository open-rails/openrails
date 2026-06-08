package models

import (
	"time"

	"github.com/google/uuid"
	"github.com/uptrace/bun"
)

type LinkedWallet struct {
	bun.BaseModel `bun:"table:billing.linked_wallets,alias:lw"`

	ID                   uuid.UUID      `bun:"id,pk,type:uuid" json:"id"`
	TenantID             uuid.UUID      `bun:"tenant_id,type:uuid,nullzero" json:"tenant_id"`
	TenantSubjectID      uuid.UUID      `bun:"tenant_subject_id,type:uuid,notnull" json:"tenant_subject_id"`
	Chain                string         `bun:"chain,notnull" json:"chain"`
	Address              string         `bun:"address,notnull" json:"address"`
	VerificationProvider string         `bun:"verification_provider,notnull" json:"verification_provider"`
	VerifiedAt           time.Time      `bun:"verified_at,notnull" json:"verified_at"`
	DisplayName          *string        `bun:"display_name,nullzero" json:"display_name,omitempty"`
	Metadata             map[string]any `bun:"metadata,type:jsonb,notnull" json:"metadata,omitempty"`
	CreatedAt            time.Time      `bun:"created_at,notnull,default:current_timestamp" json:"created_at"`
	UpdatedAt            time.Time      `bun:"updated_at,notnull,default:current_timestamp" json:"updated_at"`
}
