// Package customernetwork is the engine's seam to the customer network
// (openrails.dev): links between an openrails.dev account and merchants'
// customers. The standalone server supplies a Network; an embedded engine
// has none, so it mounts no network route and reads no link.
package customernetwork

import (
	"context"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db/gen"
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
	// Admit refuses an account that has not accepted the network terms.
	Admit(ctx context.Context, account billing.NetworkAccountID) error
	// ActiveLink is one of the account's links while it is active.
	ActiveLink(ctx context.Context, account billing.NetworkAccountID, link billing.CustomerLinkID) (billing.CustomerLink, bool, error)
	// Unlink ends one of the account's links.
	Unlink(ctx context.Context, account billing.NetworkAccountID, link billing.CustomerLinkID) (billing.CustomerLink, error)
}

type accountKey struct{}

// WithAccount binds the openrails.dev account a network route acts for.
func WithAccount(ctx context.Context, account billing.NetworkAccountID) context.Context {
	return context.WithValue(ctx, accountKey{}, account)
}

// AccountFrom is the account a network route acts for.
func AccountFrom(ctx context.Context) (billing.NetworkAccountID, bool) {
	a, ok := ctx.Value(accountKey{}).(billing.NetworkAccountID)
	return a, ok && !a.IsZero()
}

// LinkOf is a stored link as the API answers it.
func LinkOf(row gen.BillingCustomerLink) billing.CustomerLink {
	out := billing.CustomerLink{
		ID: billing.CustomerLinkID(row.ID), MerchantID: billing.MerchantID(row.MerchantID), CustomerID: billing.CustomerID(row.CustomerID),
		AccountID: billing.NetworkAccountID(row.AccountID), Method: billing.CustomerLinkMethod(row.Method), Status: billing.CustomerLinkStatus(row.Status),
		LinkedAt: row.LinkedAt.UTC(),
	}
	if row.RevokedAt != nil {
		at := row.RevokedAt.UTC()
		out.RevokedAt = &at
	}
	if row.RevokedBy != nil {
		by := billing.CustomerLinkRevokedBy(*row.RevokedBy)
		out.RevokedBy = &by
	}
	return out
}
