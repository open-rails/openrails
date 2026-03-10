package solana

import "time"

type PayResult struct {
	URL            string
	Reference      string
	Amount         int64
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
	Amount            int64
	TokenAmount       uint64
	TokenSymbol       string
	ExpiresAt         time.Time
	Instructions      string
}

type PaySessionInfo struct {
	ProductName string
}

type PayTransactionResponse struct {
	TransactionBase64 string
	Message           string
}
