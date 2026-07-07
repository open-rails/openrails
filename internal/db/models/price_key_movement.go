package models

import (
	"time"

	"github.com/google/uuid"
)

// PriceKeyMovement is one entry in the #774 pointer-movement history log: at
// EffectiveAt, Key's current pointer moved to PriceID. Append-only — a
// reactivated (flip-flopped) row gets a SECOND entry rather than a new row,
// so this log (not row back-references) is the source of "what did key K
// sell on date D" and #773's "all prior versions of key K".
type PriceKeyMovement struct {
	ID          uuid.UUID `json:"id"`
	MerchantID  uuid.UUID `json:"merchant_id"`
	Key         string    `json:"key"`
	PriceID     uuid.UUID `json:"price_id"`
	EffectiveAt time.Time `json:"effective_at"`
	CreatedAt   time.Time `json:"created_at"`
}
