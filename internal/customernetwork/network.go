// Package customernetwork is the engine's seam to the customer network
// (openrails.dev): links between an openrails.dev account and merchants'
// customers. The standalone server supplies a Network; an embedded engine
// has none, so it mounts no network route and reads no link.
package customernetwork

import (
	"context"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/billing"
)

// Credential is the merchant-side proof a request carries.
type Credential string

const (
	// CustomerToken: the customer's own token at the merchant.
	CustomerToken Credential = "customer_token"
	// CheckoutURL: the secret of an order's checkout URL, which the
	// merchant's backend created for its customer.
	CheckoutURL Credential = "checkout_url"
)

// Proof is the merchant side of a link: the customer a request is, at its
// merchant, and by what. Order is a checkout URL's order.
type Proof struct {
	Merchant   billing.MerchantID
	Customer   billing.CustomerID
	Credential Credential
	Order      billing.OrderID
}

// Network is the customer network, as the engine's routes ask it.
type Network interface {
	// Challenge issues a link challenge bound to proof.
	Challenge(ctx context.Context, proof Proof) (billing.NetworkLinkChallenge, error)
	// Link redeems a challenge with the ID token of the network sign-in that
	// answered it, presented with the same proof. created is false when the
	// customer was already linked to that account.
	Link(ctx context.Context, proof Proof, params billing.CreateNetworkLinkParams) (link billing.CustomerLink, created bool, err error)
	// Networks are the merchant's customers' active links, by customer.
	Networks(ctx context.Context, merchant billing.MerchantID, customers []uuid.UUID) (map[uuid.UUID]billing.CustomerNetwork, error)
	// CustomerOf is the merchant's customer linked to account.
	CustomerOf(ctx context.Context, merchant billing.MerchantID, account billing.NetworkAccountID) (billing.CustomerID, bool, error)
	// AdmitNative admits the deployment's own account subject as the
	// merchant's customer through admit: refused while the account is linked
	// to another customer there; linked when it accepted the network terms.
	AdmitNative(ctx context.Context, merchant billing.MerchantID, subject billing.CustomerID, admit func(context.Context) error) error
}
