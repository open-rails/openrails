package openrails

import "github.com/google/uuid"

// CaptureReceipt is the durable result of settling one admitted request.
// Zero-cost completion has no ledger transfer; identity always comes from admission.
type CaptureReceipt struct {
	RequestID        string     `json:"request_id"`
	CustomerID       string     `json:"customer_id"`
	Currency         string     `json:"currency"`
	Amount           int64      `json:"amount,string"`
	LedgerTransferID *uuid.UUID `json:"ledger_transfer_id"`
	Replayed         bool       `json:"replayed"`
}

// CaptureRequest is shared by the SDK and HTTP handler. Amount is required,
// including for zero-cost completion, and decimal-string encoded on the wire.
type CaptureRequest struct {
	Amount *int64 `json:"amount,string" binding:"required"`
	CaptureUsage
}
