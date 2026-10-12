package paymentpage

import (
	_ "embed"
	"net/http"
)

// ApplePayDomainAssociationPath is where Apple reads a domain's association
// file.
const ApplePayDomainAssociationPath = "/.well-known/apple-developer-merchantid-domain-association"

// applePayDomainAssociation is Stripe's file, byte for byte, from
// https://stripe.com/files/apple-pay/apple-developer-merchantid-domain-association:
// Stripe registers the payment origin for Apple Pay on each merchant's
// account, and Apple verifies the host by it.
//
//go:embed applepay/apple-developer-merchantid-domain-association
var applePayDomainAssociation []byte

// ApplePayDomainAssociation serves Stripe's Apple Pay domain-association file.
func ApplePayDomainAssociation() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Cache-Control", "public, max-age=3600")
		_, _ = w.Write(applePayDomainAssociation)
	})
}
