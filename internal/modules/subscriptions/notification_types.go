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
	// PremiumEndReasonAccessEnded: the converge pass saw the customer's last
	// entitlement window close with no transition-site email. Neutral copy, no
	// charge or dunning language.
	PremiumEndReasonAccessEnded PremiumEndReason = "access_ended"
	// PremiumEndReasonNonRecoverable (bucket 3): the issuer withdrew the
	// recurring mandate or the instrument is dead. The rail schedule was
	// canceled, the stored payment method untouched; the copy invites a
	// re-subscribe.
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
