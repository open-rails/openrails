package models

import (
	"time"

	"github.com/google/uuid"
	"github.com/uptrace/bun"
)

type ProcessorCustomer struct {
	bun.BaseModel `bun:"table:billing.processor_customers,alias:pc"`

	ID         uuid.UUID `bun:"id,pk,type:uuid" json:"id"`
	UserID     string    `bun:"user_id,notnull" json:"user_id"`
	Processor  string    `bun:"processor,notnull" json:"processor"`
	CustomerID string    `bun:"customer_id,notnull" json:"customer_id"`

	CreatedAt time.Time `bun:"created_at,notnull" json:"created_at"`
	UpdatedAt time.Time `bun:"updated_at,notnull" json:"updated_at"`
}
