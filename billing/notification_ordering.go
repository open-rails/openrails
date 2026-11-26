package billing

import "github.com/google/uuid"

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

// NotificationOrderingSnapshot is a merchant's notification ordering, which
// must accompany its billing archive when the producer's database moves.
type NotificationOrderingSnapshot struct {
	MerchantID MerchantID             `json:"merchant_id"`
	Ordering   []NotificationOrdering `json:"ordering"`
}
