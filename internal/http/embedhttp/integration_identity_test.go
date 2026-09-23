package embedhttp

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/requestauth"
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
