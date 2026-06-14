package models

import (
	"time"

	"github.com/google/uuid"
)

type LinkedWallet struct {
	ID                   uuid.UUID      `json:"id"`
	MerchantID             uuid.UUID      `json:"tenant_id"`
	MerchantSubjectID      uuid.UUID      `json:"tenant_subject_id"`
	Chain                string         `json:"chain"`
	Address              string         `json:"address"`
	VerificationProvider string         `json:"verification_provider"`
	VerifiedAt           time.Time      `json:"verified_at"`
	DisplayName          *string        `json:"display_name,omitempty"`
	Metadata             map[string]any `json:"metadata,omitempty"`
	CreatedAt            time.Time      `json:"created_at"`
	UpdatedAt            time.Time      `json:"updated_at"`
}
