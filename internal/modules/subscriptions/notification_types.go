package subscriptions

import (
	"strings"
	"time"

	"github.com/google/uuid"
)

type PremiumEndReason string

const (
	PremiumEndReasonUserCancel PremiumEndReason = "user_cancel"
	PremiumEndReasonExpired    PremiumEndReason = "expired"
	PremiumEndReasonChargeback PremiumEndReason = "chargeback"
	PremiumEndReasonRefund     PremiumEndReason = "refund"
	PremiumEndReasonAdmin      PremiumEndReason = "admin"
	PremiumEndReasonProcessor  PremiumEndReason = "processor_cancel"
	PremiumEndReasonUnknown    PremiumEndReason = "unknown"
)

func ParsePremiumEndReason(value string) PremiumEndReason {
	switch strings.ToLower(value) {
	case string(PremiumEndReasonUserCancel):
		return PremiumEndReasonUserCancel
	case string(PremiumEndReasonExpired):
		return PremiumEndReasonExpired
	case string(PremiumEndReasonChargeback):
		return PremiumEndReasonChargeback
	case string(PremiumEndReasonRefund):
		return PremiumEndReasonRefund
	case string(PremiumEndReasonAdmin):
		return PremiumEndReasonAdmin
	case string(PremiumEndReasonProcessor):
		return PremiumEndReasonProcessor
	default:
		return PremiumEndReasonUnknown
	}
}

type SubscriptionEmailData struct {
	UserEmail      string
	Username       string
	SubscriptionID uuid.UUID
	ProductName    string
	PriceName      string
	Amount         int64
	Currency       string
	PeriodStart    time.Time
	PeriodEnd      time.Time
	PaymentMethod  string
	TransactionID  string
}
