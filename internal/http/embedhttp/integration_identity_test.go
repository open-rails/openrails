package embedhttp

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/merchanttarget"
	"github.com/open-rails/openrails/internal/requestauth"
	"github.com/open-rails/openrails/permissions"
	"github.com/open-rails/openrails/pkg/billingauth"
	"github.com/open-rails/openrails/pkg/merchant"
	"github.com/stretchr/testify/require"
)

func TestNativeCustomerRequiresExplicitIssuerAwareCanonicalMapping(t *testing.T) {
	target := billingauth.Target{MerchantID: merchant.ID(uuid.New()), MerchantSlug: "store"}
	customerA, customerB := uuid.NewString(), uuid.NewString()
	for _, tc := range []struct {
		issuer, customer string
		valid            bool
	}{
		{"issuer-a", customerA, true}, {"issuer-b", customerB, true},
		{"issuer-a", "", false}, {"issuer-b", "opaque-user", false}, {"", customerA, false},
	} {
		t.Run(tc.issuer+"/"+tc.customer, func(t *testing.T) {
			calls := 0
			auth := &billingauth.Integration{Authentication: billingauth.AuthenticationFunc(func(context.Context, *http.Request) (billingauth.Identity, error) {
				calls++
				return billingauth.Identity{Kind: billingauth.NativeUser, SubjectID: "same-opaque-user", Issuer: tc.issuer, CustomerID: tc.customer, CredentialClass: billingauth.CredentialClassUserSession}, nil
			})}
			r := requestauth.Begin(httptest.NewRequest(http.MethodGet, "/v1/me/invoices", nil))
			p, err := nativeCustomer(auth, target).AuthenticateDelegated(r.Context(), r)
			if !tc.valid {
				require.ErrorIs(t, err, billingauth.ErrUnauthenticated)
				require.Nil(t, p)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.customer, p.SubjectID)
			require.Equal(t, tc.issuer, p.Issuer)
			require.Equal(t, target.MerchantID.String(), p.MerchantID)
			_, err = integrationAuthenticator{auth: auth}.Authenticate(r.Context(), r)
			require.NoError(t, err)
			require.Equal(t, 1, calls, "checkout and customer gates share one verified request")
		})
	}
}

func TestNativeCustomerRejectsCredentialProvenanceEscalation(t *testing.T) {
	target := billingauth.Target{MerchantID: merchant.ID(uuid.New()), MerchantSlug: "store"}
	for _, kind := range []billingauth.PrincipalKind{billingauth.Machine, billingauth.DelegatedUser, "unknown"} {
		auth := &billingauth.Integration{Authentication: billingauth.AuthenticationFunc(func(context.Context, *http.Request) (billingauth.Identity, error) {
			return billingauth.Identity{Kind: kind, SubjectID: "actor", Issuer: "issuer", CustomerID: uuid.NewString(), CredentialClass: billingauth.CredentialClassUserSession}, nil
		})}
		r := requestauth.Begin(httptest.NewRequest(http.MethodGet, "/v1/me/invoices", nil))
		_, err := nativeCustomer(auth, target).AuthenticateDelegated(r.Context(), r)
		require.ErrorIs(t, err, billingauth.ErrUnauthenticated)
	}
}

func TestNativeStaffDoesNotRequirePayableCustomerMapping(t *testing.T) {
	auth := &billingauth.Integration{Authentication: billingauth.AuthenticationFunc(func(context.Context, *http.Request) (billingauth.Identity, error) {
		return billingauth.Identity{Kind: billingauth.NativeUser, SubjectID: "staff-subject", Issuer: "staff-issuer", CredentialClass: billingauth.CredentialClassUserSession}, nil
	})}
	r := requestauth.Begin(httptest.NewRequest(http.MethodGet, "/v2/merchant/products", nil))
	staff, err := authenticateIntegration(r.Context(), r, auth)
	require.NoError(t, err)
	require.Equal(t, "staff-subject", staff.SubjectID)
	require.Empty(t, staff.CustomerID)
	_, err = integrationAuthenticator{auth: auth}.Authenticate(r.Context(), r)
	require.ErrorIs(t, err, billingauth.ErrUnauthenticated, "checkout requires an explicit payable customer mapping")
	_, err = nativeCustomer(auth, billingauth.Target{MerchantID: merchant.ID(uuid.New()), MerchantSlug: "store"}).AuthenticateDelegated(r.Context(), r)
	require.ErrorIs(t, err, billingauth.ErrUnauthenticated, "staff authority never invents personal billing ownership")
}

func TestPersonalCatalogUsesCanonicalIssuerAwareIdentity(t *testing.T) {
	target := billingauth.Target{MerchantID: merchant.ID(uuid.New()), MerchantSlug: "store"}
	for _, tc := range []struct{ issuer, customer string }{{"issuer-a", uuid.NewString()}, {"issuer-b", uuid.NewString()}, {"issuer-c", ""}} {
		t.Run(tc.issuer, func(t *testing.T) {
			auth := &billingauth.Integration{
				Authentication: billingauth.AuthenticationFunc(func(context.Context, *http.Request) (billingauth.Identity, error) {
					return billingauth.Identity{Kind: billingauth.NativeUser, SubjectID: "same-opaque-subject", Issuer: tc.issuer, CustomerID: tc.customer, CredentialClass: billingauth.CredentialClassUserSession}, nil
				}),
				Authorization: billingauth.AuthorizationFunc(func(_ context.Context, _ *http.Request, i billingauth.Identity, _ billingauth.Requirement) error {
					require.Equal(t, "same-opaque-subject", i.SubjectID)
					require.Equal(t, tc.issuer, i.Issuer)
					return nil
				}),
			}
			gate := integrationGate{auth: auth, runtime: &app.Runtime{}}
			r := requestauth.Begin(httptest.NewRequest(http.MethodGet, "/v1/catalog", nil))
			r = r.WithContext(merchanttarget.WithResolved(r.Context(), target))
			principal, err := gate.Authorize(r.Context(), r, permissions.MerchantCatalogOwnRead)
			if tc.customer == "" {
				var denied billingauth.GateError
				require.ErrorAs(t, err, &denied)
				require.Equal(t, 403, denied.Status)
			} else {
				require.NoError(t, err)
				require.Equal(t, tc.customer, principal.Subject)
			}
			principal, err = gate.Authorize(r.Context(), r, permissions.MerchantCatalogRead)
			require.NoError(t, err, "ordinary staff authority needs no payable customer mapping")
			require.Equal(t, "same-opaque-subject", principal.Subject)
		})
	}
}

func TestIntegrationGateDistinguishesDeniedAndUnavailableAuthority(t *testing.T) {
	target := billingauth.Target{MerchantID: merchant.ID(uuid.New()), MerchantSlug: "store"}
	for _, tc := range []struct {
		err    error
		status int
	}{{billingauth.GateError{Status: 403, Message: "denied"}, 403}, {billingauth.GateError{Status: 503, Message: "unavailable"}, 503}, {context.DeadlineExceeded, 503}} {
		auth := &billingauth.Integration{Authentication: billingauth.AuthenticationFunc(func(context.Context, *http.Request) (billingauth.Identity, error) {
			return billingauth.Identity{Kind: billingauth.NativeUser, SubjectID: "staff", Issuer: "issuer", CredentialClass: billingauth.CredentialClassUserSession}, nil
		}), Authorization: billingauth.AuthorizationFunc(func(context.Context, *http.Request, billingauth.Identity, billingauth.Requirement) error {
			return tc.err
		})}
		r := requestauth.Begin(httptest.NewRequest(http.MethodGet, "/v2/merchant/products", nil))
		r = r.WithContext(merchanttarget.WithResolved(r.Context(), target))
		_, err := (integrationGate{auth: auth, runtime: &app.Runtime{}}).Authorize(r.Context(), r, permissions.MerchantCatalogRead)
		var failure billingauth.GateError
		require.ErrorAs(t, err, &failure)
		require.Equal(t, tc.status, failure.Status)
	}
}
