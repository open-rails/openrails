package models

import (
	"time"

	"github.com/google/uuid"
)

// PriceKeyMovement is one move of a price key, from its configuration
// history: from EffectiveAt the key named PriceID or, when Archived, nothing.
// A reactivated price moves the key again, so the moves answer what key K
// sold on date D.
type PriceKeyMovement struct {
	ID          uuid.UUID `json:"id"`
	PriceID     uuid.UUID `json:"price_id"`
	EffectiveAt time.Time `json:"effective_at"`
	Archived    bool      `json:"archived"`
}
