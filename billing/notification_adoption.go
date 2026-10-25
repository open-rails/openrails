package billing

import (
	"github.com/google/uuid"
	"github.com/open-rails/authkit/notify"
)

// NotificationOrdering is source-owned ordering metadata for one exact issue.
// It is transferred explicitly beside a billing archive, never inferred from
// an inbox read or included in financial history.
type NotificationOrdering struct {
	CustomerID  CustomerID `json:"customer_id"`
	Key         string     `json:"key"`
	Revision    int64      `json:"revision"`
	Withdrawn   bool       `json:"withdrawn"`
	SourceEvent *uuid.UUID `json:"source_event,omitempty"`
}

// NotificationAdoption is an explicit stopped-writer cutover artifact. Adoptions
// rename exact retained legacy receipts; they must not replay history into a
// new inbox. Ordering must accompany the producer if its database is relocated.
type NotificationAdoption struct {
	MerchantID MerchantID             `json:"merchant_id"`
	Adoptions  []notify.Adoption      `json:"adoptions"`
	Ordering   []NotificationOrdering `json:"ordering"`
}
