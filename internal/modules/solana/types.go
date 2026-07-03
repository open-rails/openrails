package solana

import (
	"time"

	"github.com/google/uuid"
)

type PayResult struct {
	URL            string
	Reference      string
	Amount         int64 // micros
	Currency       string
	TokenAmount    string
	TokenUnits     uint64
	TokenMint      string
	Recipient      string
	TokenPriceUSD  float64
	FXRate         float64
	FXCurrency     string
	QuotedAt       time.Time
	QuoteExpiresAt time.Time
	Token          string
	ExpiresAt      time.Time
}

type TransactionBuildResponse struct {
	TransactionBase64 string
	Amount            int64 // micros
	TokenAmount       uint64
	TokenSymbol       string
	ExpiresAt         time.Time
	Instructions      string
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
	Amount      int64 // micros
	Currency    string
	// SessionID is the checkout session — the #713 memo local-id stamped on the
	// built transaction (the one field the chain cannot derive). Required.
	SessionID uuid.UUID
}

type PaySessionInfo struct {
	ProductName string
}

type PayTransactionResponse struct {
	TransactionBase64 string
	Message           string
}
