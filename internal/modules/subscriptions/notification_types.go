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
	PremiumEndReasonRail       PremiumEndReason = "rail_cancel"
	// PremiumEndReasonAccessEnded (#789): the converge NOTIFY pass detected the
	// customer's last entitlement window closed with no transition-site email —
	// neutral "access ended" copy, no charge/dunning language.
	PremiumEndReasonAccessEnded PremiumEndReason = "access_ended"
	// PremiumEndReasonNonRecoverable (or#870 bucket 3): the issuer withdrew the
	// recurring mandate, or the instrument is permanently dead. We cancelled the
	// schedule at the rail — their stored payment method is untouched — and the
	// copy invites them to re-subscribe.
	PremiumEndReasonNonRecoverable PremiumEndReason = "non_recoverable"
	PremiumEndReasonUnknown        PremiumEndReason = "unknown"
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
	case string(PremiumEndReasonRail):
		return PremiumEndReasonRail
	case string(PremiumEndReasonAccessEnded):
		return PremiumEndReasonAccessEnded
	case string(PremiumEndReasonNonRecoverable):
		return PremiumEndReasonNonRecoverable
	default:
		return PremiumEndReasonUnknown
	}
}

type SubscriptionEmailData struct {
	UserEmail      string
	Username       string
	SubscriptionID uuid.UUID
	ProductName    string
	Amount         int64
	Currency       string
	PeriodStart    time.Time
	PeriodEnd      time.Time
	PaymentMethod  string
	TransactionID  string
}
