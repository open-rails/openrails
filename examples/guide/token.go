package guide

import (
	"context"
	"crypto"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
)

// CustomerToken mints a short-lived token the customer's browser calls
// /v1/me with. The backend's signing key is registered as the merchant's
// application.
func CustomerToken(ctx context.Context, apiURL string, merchant billing.MerchantID, apiKey, issuer, keyID string, key crypto.Signer, user billing.CustomerID, email string) (*billing.CustomerToken, error) {
	// guide:customer-token
	remote, err := openrails.NewRemote(apiURL, openrails.RemoteOptions{
		Credentials: map[billing.MerchantID]openrails.RemoteCredential{merchant: {
			APIKey:    apiKey,
			Assertion: &openrails.AssertionKey{Issuer: issuer, KeyID: keyID, Signer: key, TokenURL: apiURL + "/oauth2/token"},
		}},
	})
	if err != nil {
		return nil, err
	}
	shop, err := remote.Merchant(merchant)
	if err != nil {
		return nil, err
	}
	// An RFC 7523 assertion redeemed for an openrails:self token, about 15 minutes.
	return shop.CustomerToken(ctx, billing.CustomerTokenParams{CustomerID: user, Email: email, EmailVerified: true})
	// guide:end
}
