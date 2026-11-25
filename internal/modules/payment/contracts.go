package payment

import (
	"fmt"
	"time"

	"github.com/google/uuid"
	identity "github.com/open-rails/openrails/internal/billingidentity"
)

type UserIdentity struct {
	ID       string
	Email    *string
	Username string
	Roles    []string
	// ClientIP is the customer's client address; empty when unknown.
	ClientIP string
}

// customerIDFromUser derives the payable customer id from the verified
// caller's user id. The auth boundary already refuses non-UUID subjects, so a
// zero derivation means a broken boundary: the write must not attribute money
// rows to uuid.Nil or another subject.
func customerIDFromUser(userID string) (uuid.UUID, error) {
	id := identity.CustomerIDFromString(userID)
	if id.IsZero() {
		return uuid.Nil, fmt.Errorf("payer subject %q is not a UUID: payable identities are UUID-only", userID)
	}
	return id.UUID(), nil
}

type PurchaseResponse struct {
	Status string `json:"status"`
	Action string `json:"action,omitempty"`
	// Code is the machine-readable reason for a blocked purchase (the
	// Conflict* consts), so clients can route to the right remedy.
	Code           string     `json:"code,omitempty"`
	Message        string     `json:"message,omitempty"`
	SubscriptionID *uuid.UUID `json:"subscription_id,omitempty"`
	PaymentID      *uuid.UUID `json:"payment_id,omitempty"`
	TransactionID  string     `json:"transaction_id,omitempty"`
	RedirectURL    string     `json:"redirect_url,omitempty"`
	DelayedStart   *time.Time `json:"delayed_start,omitempty"`
}
