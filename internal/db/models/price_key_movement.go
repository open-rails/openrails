package models

import (
	"time"

	"github.com/google/uuid"
)

// PriceKeyMovement is one entry in a price key's pointer history: at
// EffectiveAt, Key moved to PriceID. Append-only (a reactivated row gets a
// second entry), so this log answers what key K sold on date D.
type PriceKeyMovement struct {
	Archived    bool      `json:"archived"`
	ID          uuid.UUID `json:"id"`
	MerchantID  uuid.UUID `json:"merchant_id"`
	Key         string    `json:"key"`
	PriceID     uuid.UUID `json:"price_id"`
	EffectiveAt time.Time `json:"effective_at"`
	CreatedAt   time.Time `json:"created_at"`
}
