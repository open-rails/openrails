// Package customerscope is the customer a customer route acts as. Only the
// route gate binds it, from the identity Required admitted; nothing takes the
// acting customer from the path, query or body. Loads are scoped to (merchant,
// customer) in SQL, so another customer's resource reads as missing.
package customerscope

import (
	"context"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/billingauth"
)

// Scope is one customer route's customer: the admitted subject, and the
// invoker acting for it, if any. The zero value is no scope.
type Scope struct {
	merchant billing.MerchantID
	customer billing.CustomerID
	invoker  string
	present  bool
}

type contextKey struct{}

// Bind attaches the scope the route gate admitted: customer is the subject,
// invoker the party acting (the customer's own id when it acts itself),
// present whether the subject acts in person. The route gate is its only call site
// (TestBindersHaveOneSite).
func Bind(ctx context.Context, merchant billing.MerchantID, customer billing.CustomerID, invoker string, present bool) context.Context {
	return context.WithValue(ctx, contextKey{}, Scope{merchant: merchant, customer: customer, invoker: invoker, present: present})
}

// From is the request's scope; false off customer routes.
func From(ctx context.Context) (Scope, bool) {
	if ctx == nil {
		return Scope{}, false
	}
	s, ok := ctx.Value(contextKey{}).(Scope)
	return s, ok && s.Valid()
}

// Valid reports a scope with its merchant and customer.
func (s Scope) Valid() bool { return !s.merchant.IsZero() && !s.customer.IsZero() }

// Merchant is the merchant the customer buys from.
func (s Scope) Merchant() billing.MerchantID { return s.merchant }

// Customer is the customer: the admitted subject, whose account and money
// the request uses.
func (s Scope) Customer() billing.CustomerID { return s.customer }

// Invoker is the party acting: the customer's own id when it acts itself.
// Spend limits are metered per invoker.
func (s Scope) Invoker() string { return s.invoker }

// SelfActing reports the customer acting itself.
func (s Scope) SelfActing() bool { return s.invoker == s.customer.String() }

// Present reports the customer acting in person: only then does it start a
// payment.
func (s Scope) Present() bool { return s.present && s.SelfActing() }

// Payer is the scope as the acceptance a payment records: interactive only
// when the customer acts in person.
func (s Scope) Payer(c billingauth.Identity) billingauth.Payer {
	class := billingauth.CredentialClassAutomation
	if s.Present() {
		class = billingauth.CredentialClassUserSession
	}
	invoker := ""
	if !s.SelfActing() {
		invoker = s.invoker
	}
	return billingauth.Payer{
		CredentialClass: class, MerchantID: s.merchant, SubjectID: s.customer.String(), Invoker: invoker,
		Issuer: c.Issuer, Email: c.Email, EmailVerified: c.EmailVerified, Username: c.Username,
	}
}
