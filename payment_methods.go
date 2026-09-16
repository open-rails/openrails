package openrails

import "time"

type PaymentMethodSubscription struct {
	ID          string    `json:"id"`
	DisplayName string    `json:"display_name"`
	Description string    `json:"description"`
	CreatedAt   time.Time `json:"created_at"`
}

type PaymentMethod struct {
	PSPID                       string                      `json:"psp_id"`
	ID                          string                      `json:"id"`
	Object                      string                      `json:"object"`
	Type                        string                      `json:"type"`
	Rail                        string                      `json:"rail"`
	Customer                    *string                     `json:"customer,omitempty"`
	BillingDetails              *BillingDetails             `json:"billing_details,omitempty"`
	Card                        *CardDetails                `json:"card,omitempty"`
	Metadata                    map[string]string           `json:"metadata,omitempty"`
	Created                     int64                       `json:"created"`
	Health                      *PaymentMethodHealth        `json:"health,omitempty"`
	Subscriptions               []PaymentMethodSubscription `json:"subscriptions,omitempty"`
	CollectionDefaultCurrencies []string                    `json:"collection_default_currencies,omitempty"`
}

type PaymentMethodHealth struct {
	ExpiryStatus      string     `json:"expiry_status,omitempty"`       // card only: valid|expiring_soon|expired
	LastChargedAt     *time.Time `json:"last_charged_at,omitempty"`     // most recent charge time
	LastChargeOutcome string     `json:"last_charge_outcome,omitempty"` // success|failed|refunded|pending
	Active            bool       `json:"active"`                        // usable: not expired and last charge not failed
}

type BillingDetails struct {
	Name    *string         `json:"name,omitempty"`
	Email   *string         `json:"email,omitempty"`
	Phone   *string         `json:"phone,omitempty"`
	Address *BillingAddress `json:"address,omitempty"`
}

type BillingAddress struct {
	Line1      *string `json:"line1,omitempty"`
	Line2      *string `json:"line2,omitempty"`
	City       *string `json:"city,omitempty"`
	State      *string `json:"state,omitempty"`
	PostalCode *string `json:"postal_code,omitempty"`
	Country    *string `json:"country,omitempty"`
}

type CardDetails struct {
	Brand    *string `json:"brand,omitempty"`
	Last4    *string `json:"last4,omitempty"`
	ExpMonth *int    `json:"exp_month,omitempty"`
	ExpYear  *int    `json:"exp_year,omitempty"`
}
