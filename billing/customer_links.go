package billing

import (
	"time"

	"github.com/google/uuid"
)

// CustomerLinkID names one customer link; on the wire "lnk_<uuid>".
type CustomerLinkID uuid.UUID

const CustomerLinkIDPrefix = "lnk_"

func ParseCustomerLinkID(s string) (CustomerLinkID, error) {
	u, err := parsePrefixedID("customer link", CustomerLinkIDPrefix, s)
	return CustomerLinkID(u), err
}

func (id CustomerLinkID) UUID() uuid.UUID { return uuid.UUID(id) }
func (id CustomerLinkID) IsZero() bool    { return uuid.UUID(id) == uuid.Nil }
func (id CustomerLinkID) String() string {
	return formatPrefixedID(CustomerLinkIDPrefix, uuid.UUID(id))
}
func (id CustomerLinkID) MarshalText() ([]byte, error) { return []byte(id.String()), nil }
func (id *CustomerLinkID) UnmarshalText(b []byte) error {
	v, err := ParseCustomerLinkID(string(b))
	*id = v
	return err
}

// NetworkAccountID is an openrails.dev account: the one stable public id
// every merchant receives as its OIDC sub, spelled as the plain UUID.
type NetworkAccountID uuid.UUID

// ParseNetworkAccountID reads the UUID spelling of a network account id.
func ParseNetworkAccountID(s string) (NetworkAccountID, error) {
	u, err := parsePrefixedID("network account", "", s)
	return NetworkAccountID(u), err
}

func (id NetworkAccountID) UUID() uuid.UUID              { return uuid.UUID(id) }
func (id NetworkAccountID) IsZero() bool                 { return uuid.UUID(id) == uuid.Nil }
func (id NetworkAccountID) String() string               { return formatPrefixedID("", uuid.UUID(id)) }
func (id NetworkAccountID) MarshalText() ([]byte, error) { return []byte(id.String()), nil }
func (id *NetworkAccountID) UnmarshalText(b []byte) error {
	v, err := ParseNetworkAccountID(string(b))
	*id = v
	return err
}

// CustomerLinkMethod is how a link was proven.
type CustomerLinkMethod string

const (
	// CustomerLinkPaymentPage: on the payment page, by the order's pay URL
	// and a network sign-in there.
	CustomerLinkPaymentPage CustomerLinkMethod = "payment_page"
	// CustomerLinkConnect: from the merchant's site, by the customer's own
	// token there and a network sign-in through the merchant's client.
	CustomerLinkConnect CustomerLinkMethod = "connect"
	// CustomerLinkSignIn: the merchant signs its customers in with
	// openrails.dev, so the customer is the account itself.
	CustomerLinkSignIn CustomerLinkMethod = "sign_in"
)

// CustomerLinkStatus is whether a link stands.
type CustomerLinkStatus string

const (
	CustomerLinkActive  CustomerLinkStatus = "active"
	CustomerLinkRevoked CustomerLinkStatus = "revoked"
)

// CustomerLinkRevokedBy is what ended a link.
type CustomerLinkRevokedBy string

const (
	// CustomerLinkRevokedByCustomer: the account unlinked the merchant, or
	// revoked its consent to the merchant's client.
	CustomerLinkRevokedByCustomer CustomerLinkRevokedBy = "customer"
	// CustomerLinkRevokedByAccountDeleted: the account was deleted.
	CustomerLinkRevokedByAccountDeleted CustomerLinkRevokedBy = "account_deleted"
	// CustomerLinkRevokedByMerchantLeft: the merchant was deleted.
	CustomerLinkRevokedByMerchantLeft CustomerLinkRevokedBy = "merchant_left"
)

// CustomerLink is an openrails.dev account linked to one merchant's
// customer.
type CustomerLink struct {
	ID         CustomerLinkID         `json:"id"`
	MerchantID MerchantID             `json:"merchant_id"`
	CustomerID CustomerID             `json:"customer_id"`
	AccountID  NetworkAccountID       `json:"account_id"`
	Method     CustomerLinkMethod     `json:"method"`
	Status     CustomerLinkStatus     `json:"status"`
	LinkedAt   time.Time              `json:"linked_at"`
	RevokedAt  *time.Time             `json:"revoked_at"`
	RevokedBy  *CustomerLinkRevokedBy `json:"revoked_by"`
}

// NetworkLinkChallenge is what the customer's network sign-in answers: its
// Nonce is the OIDC authorization request's nonce. It is bound to the
// merchant and customer that asked for it, lasts until ExpiresAt (5
// minutes) and is redeemed once.
type NetworkLinkChallenge struct {
	ID        string    `json:"id"`
	Nonce     string    `json:"nonce"`
	ExpiresAt time.Time `json:"expires_at"`
}

// CreateNetworkLinkParams redeems a challenge with the ID token of the
// network sign-in that answered it.
type CreateNetworkLinkParams struct {
	ChallengeID string `json:"challenge_id"`
	IDToken     string `json:"id_token"`
}

// NetworkCustomerListParams finds the merchant's customer linked to an
// openrails.dev account: at most one.
type NetworkCustomerListParams struct {
	NetworkAccountID NetworkAccountID `form:"-"`
}

// CustomerNetworkHostEvent is a link made or ended: customer.network_linked
// carries Method, customer.network_unlinked RevokedBy.
type CustomerNetworkHostEvent struct {
	CustomerID CustomerID             `json:"customer_id"`
	AccountID  NetworkAccountID       `json:"account_id"`
	Method     *CustomerLinkMethod    `json:"method"`
	RevokedBy  *CustomerLinkRevokedBy `json:"revoked_by"`
}

// ErrNetworkLinkInUse refuses unlinking a customer whose subscriptions at
// the merchant still charge its cards; it is also ErrConflict.
var ErrNetworkLinkInUse = newCodedError(CodeNetworkLinkInUse, ErrConflict)
