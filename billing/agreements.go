package billing

import "time"

// Agreement is one charge agreement a customer gave a merchant: a mandate
// that lets the merchant charge a saved card without the customer present.
// A recurring one covers one subscription's renewals; an unscheduled one
// covers charges for usage in one currency. The customer revokes each on its
// own, unilaterally.
type Agreement struct {
	ID     MandateID     `json:"id"`
	Kind   MandateKind   `json:"kind"`
	Status MandateStatus `json:"status"`
	// Currency is what the agreement charges in.
	Currency string `json:"currency"`
	// PaymentMethodID and Card are the card it charges; null once the card
	// of an ended agreement was deleted.
	PaymentMethodID *PaymentMethodID `json:"payment_method_id"`
	Card            *CardDetails     `json:"card"`
	// Subscription is a recurring agreement's terms; null for unscheduled.
	Subscription *AgreementSubscription `json:"subscription"`
	AcceptedAt   time.Time              `json:"accepted_at"`
	EndedAt      *time.Time             `json:"ended_at"`
}

// AgreementSubscription is what a recurring agreement charges: the
// subscription's amount every billing interval, next on NextChargeAt (null
// once the subscription is canceled).
type AgreementSubscription struct {
	ID                   SubscriptionID `json:"id"`
	Product              string         `json:"product"`
	Amount               int64          `json:"amount,string"`
	BillingIntervalHours *int           `json:"billing_interval_hours"`
	NextChargeAt         *time.Time     `json:"next_charge_at"`
}

// AgreementRevocation answers a revoke: the agreement, now revoked; for a
// recurring one its subscription, canceled at the period's end; for an
// unscheduled one the open usage order of what was owed until then, which
// the customer pays themselves (null when nothing was owed).
type AgreementRevocation struct {
	Agreement    Agreement     `json:"agreement"`
	Subscription *Subscription `json:"subscription"`
	UsageOrder   *Order        `json:"usage_order"`
}

// MandateHostEvent is mandate.revoked: which agreement the customer revoked
// and what it covered.
type MandateHostEvent struct {
	MandateID      MandateID       `json:"mandate_id"`
	Kind           MandateKind     `json:"kind"`
	CustomerID     CustomerID      `json:"customer_id"`
	Currency       string          `json:"currency"`
	SubscriptionID *SubscriptionID `json:"subscription_id"`
	RevokedAt      time.Time       `json:"revoked_at"`
}
