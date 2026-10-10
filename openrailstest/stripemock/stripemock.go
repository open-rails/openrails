// Package stripemock is a fake Stripe for a host's end-to-end tests: it serves
// the Stripe APIs OpenRails calls, sells through hosted Checkout and delivers
// signed webhooks, so a test drives a real OpenRails through a Stripe purchase.
//
// Point a sandbox engine at it, mount OpenRails, and tell the mock where its
// webhooks go:
//
//	stripe := stripemock.New(stripemock.Options{Clock: clock.Now})
//	defer stripe.Close()
//	client, err := openrails.New(ctx, openrails.Config{
//		TestMode:          openrails.Sandbox,
//		ProviderWriteMode: openrails.ProviderWritesFull,
//		ProviderSandbox:   &openrails.ProviderSandboxConfig{StripeAPIURL: stripe.URL()},
//		Merchant: openrails.MerchantDeclaration{Slug: "shop", DisplayName: "Shop", PSPs: map[string]openrails.PSPConfig{
//			"stripe": openrails.StripePSP{AccountID: "acct_test", SecretKey: "sk_test_x", WebhookSigningSecret: "whsec_test"}.PSPConfig(),
//		}},
//	}, openrails.Deps{Postgres: pool, Clock: clock})
//	// … openrailshttp.Mount(mux, client, routes); srv := httptest.NewServer(mux)
//	stripe.SendWebhooksTo(srv.URL+"/v1/webhooks/stripe/acct_test", "whsec_test")
//
// Without a publishable key, paying a checkout session's Stripe option
// redirects to Stripe's hosted Checkout; CompleteCheckoutSession is the
// customer paying there. Deps.StripeTransport takes Transport instead of a
// listener. Only test code may import it.
package stripemock

import (
	"context"
	"net/http"
	"time"

	"github.com/open-rails/openrails/internal/stripemock"
)

// Card is a card as a customer enters it in Stripe's fields. Decline ""
// approves; a decline_code such as "insufficient_funds" declines its charges;
// "auth" makes them wait for Authenticate.
type Card = stripemock.Card

// LedgerEntry is the money one succeeded PaymentIntent moved, in minor units
// (cents), and what of it was refunded.
type LedgerEntry = stripemock.LedgerEntry

// Options configure a Mock.
type Options struct {
	// Clock is Stripe's only time source; nil is time.Now.
	Clock func() time.Time
}

// Mock is a running fake Stripe. Its methods are safe for concurrent use.
type Mock struct{ m *stripemock.Mock }

// CheckoutSession is one hosted Checkout Session.
type CheckoutSession struct {
	ID, URL string
	// Status is open, complete or expired; PaymentStatus unpaid, paid or
	// no_payment_required.
	Status, PaymentStatus string
	// AmountTotal is in minor units (cents).
	AmountTotal int64
	Currency    string
	// PaymentIntent is the payment that paid it, once complete.
	PaymentIntent string
	Metadata      map[string]string
}

// New starts Stripe on a loopback listener; Close stops it.
func New(opts Options) *Mock {
	return &Mock{stripemock.New(stripemock.Options{Clock: opts.Clock})}
}

// URL is the API root: Config.ProviderSandbox.StripeAPIURL.
func (s *Mock) URL() string { return s.m.URL() }

// Transport serves Stripe in process: Deps.StripeTransport.
func (s *Mock) Transport() http.RoundTripper { return s.m }

// Close stops the listener.
func (s *Mock) Close() { s.m.Close() }

// SendWebhooksTo is the host's Stripe webhook endpoint
// ({prefix}/v1/webhooks/stripe/{account_id}) and its signing secret.
func (s *Mock) SendWebhooksTo(endpoint, secret string) { s.m.SendWebhooksTo(endpoint, secret) }

// CheckoutSessions are the Checkout Sessions OpenRails opened, oldest first.
func (s *Mock) CheckoutSessions() []CheckoutSession {
	var out []CheckoutSession
	for _, o := range s.m.CheckoutSessions() {
		session := CheckoutSession{ID: text(o["id"]), URL: text(o["url"]), Status: text(o["status"]), PaymentStatus: text(o["payment_status"]),
			Currency: text(o["currency"]), PaymentIntent: text(o["payment_intent"]), Metadata: map[string]string{}}
		if total, ok := o["amount_total"].(float64); ok {
			session.AmountTotal = int64(total)
		}
		if metadata, ok := o["metadata"].(map[string]any); ok {
			for k, v := range metadata {
				session.Metadata[k] = text(v)
			}
		}
		out = append(out, session)
	}
	return out
}

// LastCheckoutSession is the latest Checkout Session, or nil.
func (s *Mock) LastCheckoutSession() *CheckoutSession {
	sessions := s.CheckoutSessions()
	if len(sessions) == 0 {
		return nil
	}
	return &sessions[len(sessions)-1]
}

// CompleteCheckoutSession is the customer paying session id on Stripe's page:
// Stripe charges it and sends checkout.session.completed to the host. It
// returns the event's id; a refusal by the host is an error.
func (s *Mock) CompleteCheckoutSession(ctx context.Context, id string) (string, error) {
	return s.m.CompleteCheckoutSession(ctx, id)
}

// Redeliver sends event id to the host again, as Stripe retries one.
func (s *Mock) Redeliver(ctx context.Context, id string) error { return s.m.Redeliver(ctx, id) }

// CompleteSetup is the customer saving card in Stripe's fields for a
// SetupIntent OpenRails created; it returns the payment method.
func (s *Mock) CompleteSetup(setupIntent string, card Card) string {
	return s.m.CompleteSetup(setupIntent, card)
}

// Authenticate is the customer passing their issuer's challenge on a
// PaymentIntent that requires it.
func (s *Mock) Authenticate(paymentIntent string) bool { return s.m.Authenticate(paymentIntent) }

// SetDecline sets how the issuer answers future charges of every saved card
// ending in last4 (see Card.Decline).
func (s *Mock) SetDecline(last4, decline string) { s.m.SetDecline(last4, decline) }

// Ledger is the succeeded PaymentIntents of a Stripe customer, or ("") all.
func (s *Mock) Ledger(customer string) []LedgerEntry { return s.m.Ledger(customer) }

// Unexpected lists requests the mock does not model; a test asserts it
// empty.
func (s *Mock) Unexpected() []string { return s.m.Unexpected() }

func text(v any) string {
	s, _ := v.(string)
	return s
}
