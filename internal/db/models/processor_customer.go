package models

import (
	"time"

	"github.com/google/uuid"
	"github.com/uptrace/bun"
)

type ProcessorCustomer struct {
	bun.BaseModel `bun:"table:billing.processor_customers,alias:pc"`

	ID uuid.UUID `bun:"id,pk,type:uuid" json:"id"`
	// TenantSubjectID is the OpenRails payable tenant subject for this row (#317).
	// Additive during the hard-cut rollout; writers populate it and readers move to
	// it before user_id is dropped. Join billing.tenant_subjects for issuer/subject.
	TenantSubjectID uuid.UUID `bun:"tenant_subject_id,type:uuid,nullzero" json:"tenant_subject_id,omitempty"`
	Processor       string    `bun:"processor,notnull" json:"processor"`
	CustomerID      string    `bun:"customer_id,notnull" json:"customer_id"`

	CreatedAt time.Time `bun:"created_at,notnull" json:"created_at"`
	UpdatedAt time.Time `bun:"updated_at,notnull" json:"updated_at"`
}
