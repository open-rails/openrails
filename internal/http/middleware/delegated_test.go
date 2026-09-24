package middleware

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	auth "github.com/open-rails/helpers/auth"
	"github.com/stretchr/testify/require"

	identity "github.com/open-rails/openrails/internal/billingidentity"
	"github.com/open-rails/openrails/internal/credential"
	"github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/http/router"
	"github.com/open-rails/openrails/permissions"
	"github.com/open-rails/openrails/pkg/billingauth"
	"github.com/open-rails/openrails/pkg/merchant"
)

type fakeResolver struct {
	resolved *credential.ResolvedDelegated
	err      error
}

func (f fakeResolver) ResolveDelegated(*http.Request) (*credential.ResolvedDelegated, error) {
	return f.resolved, f.err
}

var (
	testMerchant = merchant.ID(uuid.MustParse("00000000-0000-4000-8000-00000000abcd"))
	payerID      = uuid.MustParse("33333333-3333-4333-8333-333333333333")
)

// serveNeutral runs mw on GET /v1/customers/{customer_id} and returns the
// response plus the request the handler saw (nil when refused).
func serveNeutral(t *testing.T, path string, setup func(*http.Request), mw ...router.Middleware) (*httptest.ResponseRecorder, *request.Request) {
	t.Helper()
	mux := http.NewServeMux()
	var seen *request.Request
	router.NewMux(mux, "", nil).Handle(http.MethodGet, "/v1/customers/:customer_id", func(r *request.Request) {
		seen = r
		r.Status(http.StatusOK)
	}, mw...)
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set("Authorization", "Bearer eyJ.delegated.jwt")
	if setup != nil {
		setup(req)
	}
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	return w, seen
}

func delegated(perms ...string) *credential.ResolvedDelegated {
	return &credential.ResolvedDelegated{
		Merchant: "acme", MerchantSlug: "acme", MerchantID: testMerchant, CustomerID: payerID,
		DelegatedSubject: "user-123", Email: "user@example.test", EmailVerified: true, Username: "user123", Permissions: perms,
	}
}

type mws = []router.Middleware

// Every delegated/host credential and permission gate fails closed with a stable
// reason. Permissions are namespace-anchored (#567): root:* grants no merchant
// permission. Invoker-scoped principals (or#930) never reach payer surfaces.
func TestDelegatedAuthRefusals(t *testing.T) {
	self := func(r *credential.ResolvedDelegated, err error) router.Middleware {
		return DelegatedSelfRequired(fakeResolver{resolved: r, err: err})
	}
	host := func(p *billingauth.DelegatedPrincipal, err error) router.Middleware {
		return DelegatedPrincipalRequired(billingauth.DelegatedAuthenticatorFunc(func(context.Context, *http.Request) (*billingauth.DelegatedPrincipal, error) { return p, err }))
	}
	invoker := delegated()
	invoker.Invoker = "bot-7"
	settings := RequirePermission(permissions.MerchantCustomerSettingsRead)
	for _, tc := range []struct {
		name, authz string
		mw          mws
		status      int
		body        string
	}{
		{"nil resolver", "Bearer x", mws{DelegatedSelfRequired(nil)}, 500, "not configured"},
		{"no bearer", "", mws{self(delegated(), nil)}, 401, "delegated bearer token required"},
		{"wrong scheme", "Basic eA==", mws{self(delegated(), nil)}, 401, "delegated bearer token required"},
		{"expired", "Bearer x", mws{self(nil, auth.ErrExpired)}, 401, "delegated_token_expired"},
		{"revoked", "DPoP x", mws{self(nil, auth.ErrRevoked)}, 401, "delegated_token_revoked"},
		{"cross merchant", "Bearer x", mws{self(nil, credential.ErrServiceCredentialMerchantUnresolved)}, 403, "delegated_merchant_unresolved"},
		{"unknown issuer", "Bearer x", mws{self(nil, credential.ErrDelegatedIssuerUnknown)}, 403, "delegated_merchant_unresolved"},
		{"missing proof", "Bearer x", mws{self(nil, auth.ErrSenderProofRequired)}, 401, "sender_proof_required"},
		{"verifier down", "Bearer x", mws{self(nil, credential.ErrDelegatedUnavailable)}, 503, "delegated_verification_unavailable"},
		{"verifier unset", "Bearer x", mws{self(nil, credential.ErrDelegatedNotConfigured)}, 500, "not configured"},
		{"service credential or normal sub", "Bearer openrails_st_x", mws{self(nil, credential.ErrDelegatedInvalid)}, 401, "delegated_token_invalid"},
		{"host: nil authenticator", "", mws{DelegatedPrincipalRequired(nil)}, 500, "not configured"},
		{"host: gate error keeps status", "", mws{host(nil, billingauth.GateError{Status: 403, Message: "host says no"})}, 403, "host says no"},
		{"host: unauthenticated", "", mws{host(nil, billingauth.ErrUnauthenticated)}, 401, "authentication required"},
		{"host: non-uuid subject", "", mws{host(&billingauth.DelegatedPrincipal{MerchantID: testMerchant.String(), SubjectID: "user-123"}, nil)}, 401, "delegated_principal_invalid"},
		{"host: no merchant", "", mws{host(&billingauth.DelegatedPrincipal{SubjectID: payerID.String()}, nil)}, 401, "delegated_principal_invalid"},
		{"no principal", "", mws{settings}, 401, "bearer principal required"},
		{"no principal, payer gate", "", mws{PayerScopedRequired()}, 401, "bearer principal required"},
		{"missing permission", "Bearer x", mws{self(delegated(), nil), settings}, 403, "permission_required"},
		{"foreign apex glob", "Bearer x", mws{self(delegated("root:*", "*"), nil), settings}, 403, "permission_required"},
		{"invoker on payer surface", "Bearer x", mws{self(invoker, nil), PayerScopedRequired()}, 403, "invoker_scoped_principal"},
		{"merchant treasury without merchant admin", "Bearer x", mws{self(delegated(permissions.CustomerAll), nil), CustomerScopeRequired()}, 403, "customer_scope_mismatch"},
		{"treasury without principal", "", mws{CustomerScopeRequired()}, 401, "delegated principal required"},
	} {
		w, seen := serveNeutral(t, "/v1/customers/acme", func(r *http.Request) { r.Header.Set("Authorization", tc.authz) }, tc.mw...)
		require.Equal(t, tc.status, w.Code, tc.name)
		require.Contains(t, w.Body.String(), tc.body, tc.name)
		require.Nil(t, seen, tc.name)
	}
	w, _ := serveNeutral(t, "/v1/customers/x", nil, self(nil, auth.ErrSenderProofRequired))
	require.Contains(t, w.Header().Get("WWW-Authenticate"), `DPoP error="invalid_dpop_proof"`)

	for _, mw := range []mws{
		{self(delegated(permissions.MerchantAll), nil), RequirePermission(" " + permissions.MerchantCustomerSettingsRead + " ")},
		{self(delegated(permissions.MerchantCustomerSettingsRead), nil), settings},
		{self(delegated(), nil), PayerScopedRequired()},
	} {
		w, _ := serveNeutral(t, "/v1/customers/x", nil, mw...)
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	}
	w, seen := serveNeutral(t, "/v1/customers/x", nil, host(&billingauth.DelegatedPrincipal{MerchantID: testMerchant.String(), SubjectID: payerID.String(), Invoker: " bot-7 ", Permissions: []string{"customer:*"}}, nil))
	require.Equal(t, http.StatusOK, w.Code)
	p, _ := PrincipalFromRequest(seen)
	require.Equal(t, CredentialHostDelegatedUser, p.CredentialType)
	require.True(t, p.InvokerScoped())
	require.True(t, p.Can(context.Background(), permissions.CustomerAll))
}

func TestDelegatedBinding(t *testing.T) {
	w, seen := serveNeutral(t, "/v1/customers/x", nil, DelegatedSelfRequired(fakeResolver{resolved: delegated()}))
	require.Equal(t, http.StatusOK, w.Code)
	got, _ := merchant.FromContext(seen.Request.Context())
	require.Equal(t, testMerchant, got)
	uc, _ := seen.UserContext()
	require.Equal(t, billingauth.UserContext{UserID: "user-123", Email: "user@example.test", EmailVerified: true, Username: "user123", Merchant: "acme"}, uc)
	p, _ := PrincipalFromRequest(seen)
	require.Equal(t, CredentialDelegatedUser, p.CredentialType)
	require.Equal(t, "user-123", p.Subject)

	// Neither a client assertion nor a runtime binding may select another merchant.
	for _, tc := range []struct {
		header string
		bound  merchant.ID
		want   int
	}{
		{testMerchant.String(), testMerchant, 200},
		{uuid.NewString(), testMerchant, 409},
		{"", merchant.ID(uuid.New()), 409},
		{"invalid", testMerchant, 400},
	} {
		w, _ := serveNeutral(t, "/v1/customers/x", func(r *http.Request) {
			if tc.header != "" {
				r.Header.Set(merchant.BindingHeader, tc.header)
			}
			*r = *r.WithContext(merchant.WithID(r.Context(), tc.bound))
		}, DelegatedSelfRequired(fakeResolver{resolved: delegated()}))
		require.Equal(t, tc.want, w.Code, "%s: %s", tc.header, w.Body.String())
	}
}

// or#916: :customer_id binds the principal's own payable subject, or the
// merchant's treasury only for a merchant-admin principal.
func TestResolveTreasuryPayer(t *testing.T) {
	uuidSubject := delegated()
	uuidSubject.CustomerID, uuidSubject.DelegatedSubject = uuid.Nil, payerID.String()
	opaqueSubject := delegated()
	opaqueSubject.CustomerID = uuid.Nil
	zeroMerchant := delegated(permissions.MerchantAll)
	zeroMerchant.MerchantID = merchant.ID{}
	merchantPayer := &TreasuryPayer{Subject: "user-123", CustomerID: identity.CustomerID(testMerchant.UUID()), MerchantPayer: true}
	subjectPayer := &TreasuryPayer{Subject: "user-123", CustomerID: identity.CustomerID(payerID)}
	admin := delegated(permissions.MerchantAll)

	for _, tc := range []struct {
		customerID string
		resolved   *credential.ResolvedDelegated
		want       *TreasuryPayer
	}{
		{" user-123 ", delegated(), subjectPayer},
		{payerID.String(), delegated(), subjectPayer},
		{payerID.String(), uuidSubject, &TreasuryPayer{Subject: payerID.String(), CustomerID: identity.CustomerID(payerID)}},
		{"user-123", opaqueSubject, nil},
		{uuid.NewString(), admin, nil},
		{"acme", delegated(permissions.CustomerAll), nil},
		{"acme", admin, merchantPayer},
		{testMerchant.String(), admin, merchantPayer},
		{" ", admin, nil},
		{"acme", nil, nil},
		{"acme", zeroMerchant, nil},
	} {
		got, ok := ResolveTreasuryPayer(tc.customerID, tc.resolved)
		require.Equal(t, tc.want != nil, ok, tc.customerID)
		require.Equal(t, tc.want, got, tc.customerID)
	}

	w, seen := serveNeutral(t, "/v1/customers/acme", nil, DelegatedSelfRequired(fakeResolver{resolved: admin}), CustomerScopeRequired())
	require.Equal(t, http.StatusOK, w.Code)
	payer, _ := TreasuryPayerFromRequest(seen)
	require.Equal(t, merchantPayer, payer)
	uc, _ := seen.UserContext()
	require.Equal(t, billingauth.UserContext{UserID: testMerchant.String(), Merchant: "acme", Username: "acme"}, uc, "the merchant payer carries no end-user contact data")
}

// A native session owns its personal account outright; any other target needs
// a live, exact-target authorization decision.
func TestNativeTreasuryAuthority(t *testing.T) {
	personal := payerID.String()
	for _, tc := range []struct {
		customerID string
		authorize  func(context.Context, string, billingauth.Target) error
		status     int
	}{
		{personal, nil, 200},
		{"acme", nil, 403},
		{testMerchant.String(), func(_ context.Context, perm string, target billingauth.Target) error {
			if perm != permissions.CustomerAll || target.CustomerID != testMerchant.String() {
				return errors.New("wrong decision request")
			}
			return nil
		}, 200},
		{"acme", func(context.Context, string, billingauth.Target) error {
			return billingauth.GateError{Status: 409, Message: "no"}
		}, 409},
		{"acme", func(context.Context, string, billingauth.Target) error { return errors.New("down") }, 503},
		{uuid.NewString(), nil, 403},
	} {
		native := func(next router.Handler) router.Handler {
			return func(r *request.Request) {
				SetNativeTreasuryAuthority(r, NativeTreasuryAuthority{CustomerID: personal, Target: billingauth.Target{MerchantID: testMerchant, MerchantSlug: "acme"}, Authorize: tc.authorize})
				next(r)
			}
		}
		w, _ := serveNeutral(t, "/v1/customers/"+tc.customerID, nil,
			DelegatedSelfRequired(fakeResolver{resolved: delegated()}), native, CustomerScopeRequired(), RequirePermission(permissions.CustomerAll))
		require.Equal(t, tc.status, w.Code, "%s: %s", tc.customerID, w.Body.String())
	}
}
