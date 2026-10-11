package hostedcheckout

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	auth "github.com/open-rails/helpers/auth"
	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/billingauth"
	"github.com/open-rails/openrails/internal/merchant"
)

// CredentialKind is a checkout URL's secret, as audit records it.
const CredentialKind billingauth.CredentialKind = "checkout_url"

// Issuer vouches for the customer a checkout secret acts as.
const Issuer = "openrails:checkout"

type ctxKey struct{}

// WithCheckout binds the checkout a request's secret opened.
func WithCheckout(ctx context.Context, c Checkout) context.Context {
	return context.WithValue(ctx, ctxKey{}, c)
}

// FromContext is the checkout the request's secret opened: set only on the
// checkout host.
func FromContext(ctx context.Context) (Checkout, bool) {
	if ctx == nil {
		return Checkout{}, false
	}
	c, ok := ctx.Value(ctxKey{}).(Checkout)
	return c, ok
}

// ResolveHTTP admits the checkout host's API requests by their bearer
// secret and pins its order's merchant: 404 checkout_not_found for an
// unknown or missing one, 410 checkout_expired past its expiry. Every answer
// is no-store. Paths outside /v1/ (the page) pass through untouched.
func ResolveHTTP(store *Store, now func() time.Time) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !strings.HasPrefix(r.URL.Path, "/v1/") {
				next.ServeHTTP(w, r)
				return
			}
			w = noStore{w}
			c, err := store.Resolve(r.Context(), bearer(r.Header.Get("Authorization")), now())
			switch {
			case errors.Is(err, ErrNotFound):
				billingauth.WriteJSONError(w, http.StatusNotFound, "checkout_not_found", "")
				return
			case errors.Is(err, ErrExpired):
				billingauth.WriteJSONError(w, http.StatusGone, "checkout_expired", "")
				return
			case err != nil:
				log.WithContext(r.Context()).WithError(err).Error("hosted checkout: secret lookup failed")
				billingauth.WriteJSONError(w, http.StatusServiceUnavailable, billing.CodeServiceUnavailable, "")
				return
			}
			ctx := merchant.WithHostMerchant(merchant.WithID(WithCheckout(r.Context(), c), c.MerchantID), c.MerchantID)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

func bearer(header string) string {
	scheme, token, ok := strings.Cut(strings.TrimSpace(header), " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return ""
	}
	return strings.TrimSpace(token)
}

// noStore keeps every checkout answer out of caches: what it says depends on
// the secret, which no cache keys on.
type noStore struct{ http.ResponseWriter }

func (w noStore) WriteHeader(status int) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Del("ETag")
	w.ResponseWriter.WriteHeader(status)
}

func (w noStore) Write(b []byte) (int, error) {
	if w.Header().Get("Cache-Control") != "no-store" {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(b)
}

func (w noStore) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// Authenticator says a checkout host request is its order's customer, in
// person: the secret ResolveHTTP admitted.
type Authenticator struct{}

func (Authenticator) Authenticate(r *http.Request) (auth.Verified, error) {
	c, ok := FromContext(r.Context())
	if !ok {
		return nil, auth.ErrUnauthenticated
	}
	return verified{MerchantBinding: billingauth.BindsMerchant(billingauth.Target{MerchantID: c.MerchantID}), c: c}, nil
}

type verified struct {
	billingauth.MerchantBinding
	c Checkout
}

func (v verified) Identity() auth.Identity {
	id := v.c.CustomerID.String()
	return auth.Identity{
		Issuer: Issuer, Subject: id, SubjectKind: auth.SubjectUser,
		Invoker:    auth.Invoker{Issuer: Issuer, ID: id},
		Credential: auth.Credential{Kind: CredentialKind, ID: v.c.OrderID.String()},
	}
}
