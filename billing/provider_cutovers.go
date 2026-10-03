package billing

import (
	"time"

	"github.com/google/uuid"
)

// ProviderCutoverRequest asserts both accounts and names the customer's newly
// vaulted card. No provider reference or paid-through date comes from the caller.
type ProviderCutoverRequest struct {
	TargetPaymentMethodID PaymentMethodID `json:"target_payment_method_id"`
	ExpectedSourcePSPID   uuid.UUID       `json:"expected_source_psp_id"`
	ExpectedTargetPSPID   uuid.UUID       `json:"expected_target_psp_id"`
}

type ProviderCutover struct {
	ID                    uuid.UUID       `json:"id"`
	MerchantID            uuid.UUID       `json:"merchant_id"`
	SubscriptionID        SubscriptionID  `json:"subscription_id"`
	SourcePSPID           uuid.UUID       `json:"source_psp_id"`
	TargetPSPID           uuid.UUID       `json:"target_psp_id"`
	TargetPaymentMethodID PaymentMethodID `json:"target_payment_method_id"`
	TargetSubscriptionID  string          `json:"target_subscription_id"`
	Anchor                time.Time       `json:"anchor"`
	Status                string          `json:"status"`
	Stage                 string          `json:"stage"`
	Reason                string          `json:"reason"`
}
