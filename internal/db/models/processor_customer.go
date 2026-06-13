package models

import (
	"time"

	"github.com/google/uuid"
)

type ProcessorCustomer struct {
	ID uuid.UUID `json:"id"`
	// TenantSubjectID is the OpenRails payable tenant subject for this row (#317).
	// Additive during the hard-cut rollout; writers populate it and readers move to
	// it before user_id is dropped. Join openrails.tenant_subjects for issuer/subject.
	TenantSubjectID uuid.UUID `json:"tenant_subject_id,omitempty"`
	Processor       string    `json:"processor"`
	CustomerID      string    `json:"customer_id"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}
