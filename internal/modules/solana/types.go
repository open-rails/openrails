package solana

import (
	"time"

	"github.com/google/uuid"
)

type TransactionBuildResponse struct {
	TransactionBase64 string
	Amount            int64 // native price units
	TokenAmount       uint64
	TokenSymbol       string
	ExpiresAt         time.Time
	Instructions      string
	// LastValidBlockHeight: past it the transaction can never land.
	LastValidBlockHeight uint64
}

type PaymentTransactionBuildRequest struct {
	UserID      string
	PriceID     uuid.UUID
	TokenSymbol string
	UserWallet  string
	Reference   *string
	TokenAmount uint64
	TokenMint   string
	Recipient   string
	Amount      int64 // native units of Currency
	Currency    string
	// SessionID is the order attempt, stamped on the built transaction as
	// the memo (the one field the chain cannot derive). Required.
	SessionID uuid.UUID
}

type PaySessionInfo struct {
	ProductName string
}

type PayTransactionResponse struct {
	TransactionBase64 string
	Message           string
}
