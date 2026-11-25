package models

import (
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// PurchaseKind is the offer category used by processor routing policies.
// Invoice attempts themselves are owned by invoices and carry no mode.
type PurchaseKind string

const (
	PurchaseOneOff       PurchaseKind = "one_off"
	PurchaseSubscription PurchaseKind = "subscription"
)

type InvoiceAttemptStatus string

const (
	InvoiceAttemptStatusCreated        InvoiceAttemptStatus = "created"
	InvoiceAttemptStatusRequiresAction InvoiceAttemptStatus = "requires_action"
	InvoiceAttemptStatusSucceeded      InvoiceAttemptStatus = "succeeded"
	InvoiceAttemptStatusFailed         InvoiceAttemptStatus = "failed"
	InvoiceAttemptStatusExpired        InvoiceAttemptStatus = "expired"
	InvoiceAttemptStatusCanceled       InvoiceAttemptStatus = "canceled"
)

type InvoiceAttempt struct {
	ID uuid.UUID `json:"id"`
	// CustomerID is the host's subject UUID within MerchantID.
	CustomerID uuid.UUID `json:"customer_id,omitempty"`

	InvoiceID *uuid.UUID `json:"invoice_id,omitempty"`

	Rail   Rail                 `json:"rail"`
	Status InvoiceAttemptStatus `json:"status"`

	Amount   *int64  `json:"amount"`
	Currency *string `json:"currency"`

	ExpiresAt *time.Time `json:"expires_at,omitempty"`
	Reference *string    `json:"reference,omitempty"`

	TransactionID  *string    `json:"transaction_id,omitempty"`
	PaymentID      *uuid.UUID `json:"payment_id,omitempty"`
	SubscriptionID *uuid.UUID `json:"subscription_id,omitempty"`

	Metadata map[string]string `json:"metadata,omitempty"`

	RailFields map[string]any `json:"rail_fields,omitempty"`
	RailState  map[string]any `json:"rail_state,omitempty"`

	// IdempotencyKey is request-scoped; the row has no such column.
	IdempotencyKey *string `json:"idempotency_key,omitempty"`
	// PspID is the PSP selected for this checkout, so provider sessions are
	// never confused across accounts.
	PspID     uuid.UUID `json:"psp_id"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`

	Price      *Price  `json:"price,omitempty"`
	LastFour   *string `json:"last_four,omitempty"`
	CardType   *string `json:"card_type,omitempty"`
	ExpiryDate *string `json:"expiry_date,omitempty"`
}

// PSP routing policy sources: the trace names who decided.
const (
	// PSPRoutingPolicyExplicit: the request named the PSP. Honoured
	// verbatim — an explicitly named processor is NEVER silently switched,
	// because the browser has already committed to that rail's flow.
	PSPRoutingPolicyExplicit = "explicit"
	// PSPRoutingPolicyMerchant: a declared merchant routing rule matched.
	PSPRoutingPolicyMerchant = "merchant"
	// PSPRoutingPolicyDefault: no policy declared (or none matched) — the
	// built-in preference order decided.
	PSPRoutingPolicyDefault = "default"
)

// PSP routing skip classes: why a candidate ahead of the winner was
// passed over. Pre-charge availability only: a decline never re-routes.
const (
	// PSPRoutingSkipUnknownSelector: not a declared PSP key or rail kind.
	PSPRoutingSkipUnknownSelector = "unknown_selector"
	// PSPRoutingSkipAmbiguousSelector: a bare rail kind with more than one
	// armed PSP; the selector must name the PSP key.
	PSPRoutingSkipAmbiguousSelector = "ambiguous_selector"
	// PSPRoutingSkipNotArmed: no active non-archived psps row.
	PSPRoutingSkipNotArmed = "not_armed"
	// PSPRoutingSkipCredentialsMissing: armed, but the credentials the rail
	// needs to charge are absent or malformed.
	PSPRoutingSkipCredentialsMissing = "credentials_missing"
	// PSPRoutingSkipLinkMissing: the price has no object this PSP sells it
	// through: a CCBill FlexForm, a Solana plan.
	PSPRoutingSkipLinkMissing = "link_missing"
	// PSPRoutingSkipNotSoldVia: the price's sell_via leaves out this PSP's
	// method (card, solana).
	PSPRoutingSkipNotSoldVia = "not_sold_via"
	// PSPRoutingSkipNotChargePSP: a card PSP the merchant's charge_psps
	// leaves out.
	PSPRoutingSkipNotChargePSP = "not_a_charge_psp"
	// PSPRoutingSkipProviderPrice: a provider price, which sells only through
	// its own provider and only when that provider still sells new (CCBill).
	PSPRoutingSkipProviderPrice = "provider_price"
	// PSPRoutingSkipModeUnsupported: the rail cannot serve this checkout
	// mode for this price: the rail registry says it cannot make this kind of
	// new sale (CCBill new subscriptions, any trial first phase).
	PSPRoutingSkipModeUnsupported = "mode_unsupported"
	// PSPRoutingSkipCurrencyUnsupported means this rail cannot charge the price denomination.
	PSPRoutingSkipCurrencyUnsupported = "currency_unsupported"
	// PSPRoutingSkipPostureDisarmed: the PSP's credentials failed their
	// posture verification, so its mutations are refused.
	PSPRoutingSkipPostureDisarmed = "posture_disarmed"
	// PSPRoutingSkipServiceUnavailable: the runtime service backing the
	// rail is not wired in this deployment.
	PSPRoutingSkipServiceUnavailable = "service_unavailable"
	// PSPRoutingSkipResolveFailed: the selector could not be resolved at
	// all (backend error). Fail closed: skip, never guess.
	PSPRoutingSkipResolveFailed = "resolve_failed"
)

// ValidateTerms checks the invoice's frozen payment amount and currency.
func (s *InvoiceAttempt) ValidateTerms() error {
	if s == nil || s.InvoiceID == nil || *s.InvoiceID == uuid.Nil {
		return fmt.Errorf("payment attempt requires its invoice")
	}
	if s.Amount == nil || s.Currency == nil || strings.TrimSpace(*s.Currency) == "" {
		return fmt.Errorf("payment attempt requires amount and currency")
	}
	return nil
}
