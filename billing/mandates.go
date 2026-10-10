package billing

import "time"

// MandateKind is the agreement a mandate records.
type MandateKind string

const (
	// MandateRecurring covers one subscription's merchant-initiated renewals.
	MandateRecurring MandateKind = "recurring"
	// MandateUnscheduled covers merchant-initiated collection in one currency.
	MandateUnscheduled MandateKind = "unscheduled"
	// MandateCardOnFile is the customer's consent to reuse the card for
	// one-click buys; it never covers a merchant-initiated charge.
	MandateCardOnFile MandateKind = "card_on_file"
)

// MandateStatus is whether a mandate can authorize charges.
type MandateStatus string

const (
	MandateActive MandateStatus = "active"
	// MandateRequiresReconsent: merchant-initiated charges wait for the
	// customer's fresh consent.
	MandateRequiresReconsent MandateStatus = "requires_reconsent"
	// MandateRevoked: the customer withdrew it.
	MandateRevoked MandateStatus = "revoked"
	MandateEnded   MandateStatus = "ended"
)

// MandateEndReason is why a mandate stopped.
type MandateEndReason string

const (
	MandateEndReplaced          MandateEndReason = "replaced"
	MandateEndBrandChanged      MandateEndReason = "brand_changed"
	MandateEndClosed            MandateEndReason = "closed"
	MandateEndCustomerRevoked   MandateEndReason = "customer_revoked"
	MandateEndSubscriptionEnded MandateEndReason = "subscription_ended"
	// MandateEndPaymentMethodRemoved: the card was deleted.
	MandateEndPaymentMethodRemoved MandateEndReason = "payment_method_removed"
)

// Mandate is a customer's consent to stored-credential use of one saved card
// and the network references its storing transaction established: agreement
// evidence for issuers, acquirers and chargebacks. Merchant-initiated charges
// run only under an active mandate and send its references; they belong to
// the PSP account that ran the storing transaction.
type Mandate struct {
	ID         MandateID  `json:"id"`
	CustomerID CustomerID `json:"customer_id"`
	// PaymentMethodID is null once an ended mandate's card was deleted.
	PaymentMethodID *PaymentMethodID `json:"payment_method_id"`
	PSPID           PSPID            `json:"psp_id"`
	Kind            MandateKind      `json:"kind"`
	// SubscriptionID scopes a recurring mandate; Currency an unscheduled one.
	SubscriptionID *SubscriptionID   `json:"subscription_id"`
	Currency       *string           `json:"currency"`
	Status         MandateStatus     `json:"status"`
	EndReason      *MandateEndReason `json:"end_reason"`
	EndedAt        *time.Time        `json:"ended_at"`
	// CardBrand is the brand the agreement was established on.
	CardBrand *string `json:"card_brand"`
	// InitialTransactionID is the provider's id of the storing transaction
	// (NMI transactionid, Stripe pi_ or seti_); null until one is approved.
	InitialTransactionID *string `json:"initial_transaction_id"`
	// NetworkTransactionID (Visa TID, Mastercard Trace ID) and
	// TransactionLinkID (Mastercard TLID) are the scheme's references, where
	// the provider returns them.
	NetworkTransactionID *string `json:"network_transaction_id"`
	TransactionLinkID    *string `json:"transaction_link_id"`
	// StoringAttemptID is the storing transaction's payment attempt while it
	// is retained.
	StoringAttemptID *PaymentAttemptID `json:"storing_attempt_id"`
	AcceptedAt       time.Time         `json:"accepted_at"`
	CreatedAt        time.Time         `json:"created_at"`
}
