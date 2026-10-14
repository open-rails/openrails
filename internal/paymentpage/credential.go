package paymentpage

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	auth "github.com/open-rails/authkit/auth"
	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/billingauth"
	"github.com/open-rails/openrails/internal/merchant"
)

// CredentialKind is a pay URL's secret, as audit records it.
const CredentialKind billingauth.CredentialKind = "pay_url"

// Issuer vouches for the customer a pay URL secret acts as.
const Issuer = "openrails:pay_url"

type ctxKey struct{}

// WithPayURL binds the pay URL a request's secret opened.
func WithPayURL(ctx context.Context, c PayURL) context.Context {
	return context.WithValue(ctx, ctxKey{}, c)
}

// FromContext is the pay URL the request's secret opened: set only on the
// checkout origin.
func FromContext(ctx context.Context) (PayURL, bool) {
	if ctx == nil {
		return PayURL{}, false
	}
	c, ok := ctx.Value(ctxKey{}).(PayURL)
	return c, ok
}

// ResolveHTTP admits the checkout origin's API requests by their bearer
// secret and pins its order's merchant: 404 pay_url_not_found for an
// unknown or missing one, 410 pay_url_expired past its expiry. Every answer
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
				billingauth.WriteJSONError(w, http.StatusNotFound, "pay_url_not_found", "")
				return
			case errors.Is(err, ErrExpired):
				billingauth.WriteJSONError(w, http.StatusGone, "pay_url_expired", "")
				return
			case err != nil:
				log.WithContext(r.Context()).WithError(err).Error("payment page: secret lookup failed")
				billingauth.WriteJSONError(w, http.StatusServiceUnavailable, billing.CodeServiceUnavailable, "")
				return
			}
			ctx := merchant.WithID(WithPayURL(r.Context(), c), c.MerchantID)
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

// noStore keeps every checkout origin answer out of caches: what it says depends on
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

// Authenticator says a checkout origin request is its order's customer, in
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
	c PayURL
}

func (v verified) Identity() auth.Identity {
	id := v.c.CustomerID.String()
	return auth.Identity{
		Issuer: Issuer, Subject: id, SubjectKind: auth.SubjectUser,
		Invoker:    auth.Invoker{Issuer: Issuer, ID: id},
		Credential: auth.Credential{Kind: CredentialKind, ID: v.c.OrderID.String()},
	}
}
