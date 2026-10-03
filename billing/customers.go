package billing

import (
	"time"
)

// Customer is one merchant-scoped billing record. Its ID is the host's stable
// subject UUID; the same UUID under another merchant is a different customer.
type Customer struct {
	ID         string    `json:"id"`
	CreatedAt  time.Time `json:"created_at"`
	LastSeenAt time.Time `json:"last_seen_at"`
}
