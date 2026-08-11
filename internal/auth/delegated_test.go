package auth

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/open-rails/authkit/verify"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/permissions"
	"github.com/open-rails/openrails/pkg/billingauth"
)

type stubVerifier struct {
	claims verify.Claims
	err    error
}

func (s stubVerifier) Verify(string) (verify.Claims, error) { return s.claims, s.err }

func delegatedReq(bearer string) *http.Request {
	return delegatedReqPath("/billing/v1/me/balance", bearer)
}

func delegatedReqPath(path, bearer string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, path, nil)
	if bearer != "" {
		r.Header.Set("Authorization", "Bearer "+bearer)
	}
	return r
}

const engineMerchantID = "6a68e70a-4dd9-4b39-a3ba-4657303c6f70"

func rolePermissions(mapper func([]string) []string) PermissionResolver {
	return func(_ context.Context, _ *http.Request, cl verify.Claims) ([]string, error) {
		return mapper(cl.Roles), nil
	}
}

// The or#913 contract: the principal's merchant is the ENGINE's bound
// merchant from construction — never anything read from the caller's token
// (tensorhub pinned the caller's org UUID and broke RLS on every request,
// th#1765). Subject is the sub claim; issuer is the verified iss; permissions
// come from the injected resolver.
func TestDelegatedAuthenticator_MapsClaimsOntoEnginePinnedPrincipal(t *testing.T) {
	t.Parallel()

	v := stubVerifier{claims: verify.Claims{
		UserID:        "8b0f9f0e-9a4b-4a5f-9f3a-2f8f0a1b2c3d",
		Roles:         []string{"member"},
		Issuer:        "https://auth.host.example",
		Email:         "user@host.example",
		EmailVerified: true,
		Username:      "user",
	}}
	a := NewDelegatedAuthenticator(DelegatedConfig{
		Verifier:    RequestVerifierFor(v),
		MerchantID:  engineMerchantID,
		Permissions: rolePermissions(func(roles []string) []string { return permissions.ForRoles(roles...) }),
	})

	p, err := a.AuthenticateDelegated(t.Context(), delegatedReq("some.jwt.token"))
	require.NoError(t, err)
	require.NoError(t, p.Validate())
	require.Equal(t, engineMerchantID, p.MerchantID)
	require.Equal(t, "8b0f9f0e-9a4b-4a5f-9f3a-2f8f0a1b2c3d", p.SubjectID)
	require.Equal(t, "https://auth.host.example", p.Issuer)
	require.Equal(t, permissions.ForRoles("member"), p.Permissions)
	require.Equal(t, "user@host.example", p.Email)
	require.True(t, p.EmailVerified)
	require.Equal(t, "user", p.Username)
	require.Empty(t, p.MerchantSlug)
}

func TestDelegatedAuthenticator_FailsClosed(t *testing.T) {
	t.Parallel()

	valid := RequestVerifierFor(stubVerifier{claims: verify.Claims{UserID: "u"}})

	// No bearer at all.
	a := NewDelegatedAuthenticator(DelegatedConfig{Verifier: valid, MerchantID: engineMerchantID})
	_, err := a.AuthenticateDelegated(t.Context(), delegatedReq(""))
	require.ErrorIs(t, err, billingauth.ErrUnauthenticated)

	// Verifier rejects the token.
	bad := errors.New("boom")
	a = NewDelegatedAuthenticator(DelegatedConfig{
		Verifier:   RequestVerifierFor(stubVerifier{err: bad}),
		MerchantID: engineMerchantID,
	})
	_, err = a.AuthenticateDelegated(t.Context(), delegatedReq("x.y.z"))
	require.ErrorIs(t, err, bad)

	// A verified token without a native user subject (e.g. a service
	// credential) has no self to act as.
	a = NewDelegatedAuthenticator(DelegatedConfig{
		Verifier:   RequestVerifierFor(stubVerifier{claims: verify.Claims{UserID: "  "}}),
		MerchantID: engineMerchantID,
	})
	_, err = a.AuthenticateDelegated(t.Context(), delegatedReq("x.y.z"))
	require.ErrorIs(t, err, billingauth.ErrUnauthenticated)

	// Nil resolver grants nothing (still authenticates for /v1/me).
	a = NewDelegatedAuthenticator(DelegatedConfig{Verifier: valid, MerchantID: engineMerchantID})
	p, err := a.AuthenticateDelegated(t.Context(), delegatedReq("x.y.z"))
	require.NoError(t, err)
	require.Empty(t, p.Permissions)
}

// or#918: the admission veto refuses a token that VERIFIES — the shape of a
// liveness gate — and it runs BEFORE the permission resolver, so a banned
// subject never reaches the host's DB-backed grant lookup.
func TestDelegatedAuthenticator_AdmissionVetoPrecedesPermissionResolution(t *testing.T) {
	t.Parallel()

	v := RequestVerifierFor(stubVerifier{claims: verify.Claims{UserID: "8b0f9f0e-9a4b-4a5f-9f3a-2f8f0a1b2c3d"}})
	resolved := 0
	resolver := func(context.Context, *http.Request, verify.Claims) ([]string, error) {
		resolved++
		return []string{permissions.CustomerAll}, nil
	}

	live := true
	a := NewDelegatedAuthenticator(DelegatedConfig{
		Verifier:   v,
		MerchantID: engineMerchantID,
		Admit: func(_ context.Context, _ *http.Request, cl verify.Claims) error {
			if !live {
				return errors.New("user is banned: " + cl.UserID)
			}
			return nil
		},
		Permissions: resolver,
	})

	p, err := a.AuthenticateDelegated(t.Context(), delegatedReq("x.y.z"))
	require.NoError(t, err)
	require.Equal(t, []string{permissions.CustomerAll}, p.Permissions)
	require.Equal(t, 1, resolved)

	live = false
	_, err = a.AuthenticateDelegated(t.Context(), delegatedReq("x.y.z"))
	require.ErrorIs(t, err, billingauth.ErrUnauthenticated, "a refused admission is a 401")
	require.Equal(t, 1, resolved, "the grant lookup must not run for an inadmissible subject")
	require.NotContains(t, err.Error(), "banned", "the host's reason is logged, never returned to the client")
}

// or#918: the resolver sees the REQUEST, which is what a role→permission
// mapper cannot — doujins grants merchant:* only on the admin path so the hot
// self path stays free of authority lookups.
func TestDelegatedAuthenticator_PermissionResolverSeesTheRequest(t *testing.T) {
	t.Parallel()

	var lookups []string
	a := NewDelegatedAuthenticator(DelegatedConfig{
		Verifier:     RequestVerifierFor(stubVerifier{claims: verify.Claims{UserID: "8b0f9f0e-9a4b-4a5f-9f3a-2f8f0a1b2c3d"}}),
		MerchantID:   engineMerchantID,
		MerchantSlug: "acme",
		Issuer:       "openrails:self",
		Permissions: func(_ context.Context, r *http.Request, _ verify.Claims) ([]string, error) {
			perms := []string{permissions.CustomerAll}
			if r.URL.Path == "/billing/v1/merchant/settings" {
				lookups = append(lookups, r.URL.Path)
				perms = append(perms, permissions.MerchantAll)
			}
			return perms, nil
		},
	})

	self, err := a.AuthenticateDelegated(t.Context(), delegatedReqPath("/billing/v1/me/balance", "x.y.z"))
	require.NoError(t, err)
	require.Equal(t, []string{permissions.CustomerAll}, self.Permissions)
	require.Empty(t, lookups, "no authority lookup on the self path")

	admin, err := a.AuthenticateDelegated(t.Context(), delegatedReqPath("/billing/v1/merchant/settings", "x.y.z"))
	require.NoError(t, err)
	require.Contains(t, admin.Permissions, permissions.MerchantAll)
	require.Equal(t, []string{"/billing/v1/merchant/settings"}, lookups)

	// The merchant coordinates and the audit issuer are the host's, on every
	// principal (or#916 addresses the merchant treasury account by slug).
	require.Equal(t, "acme", admin.MerchantSlug)
	require.Equal(t, engineMerchantID, admin.MerchantID)
	require.Equal(t, "openrails:self", admin.Issuer, "WithIssuer overrides the token's iss")

	// A resolver that errors fails the request closed rather than downgrading
	// it to a grant-free principal.
	boom := NewDelegatedAuthenticator(DelegatedConfig{
		Verifier:   RequestVerifierFor(stubVerifier{claims: verify.Claims{UserID: "8b0f9f0e-9a4b-4a5f-9f3a-2f8f0a1b2c3d"}}),
		MerchantID: engineMerchantID,
		Permissions: func(context.Context, *http.Request, verify.Claims) ([]string, error) {
			return nil, errors.New("authority unreachable")
		},
	})
	_, err = boom.AuthenticateDelegated(t.Context(), delegatedReq("x.y.z"))
	require.ErrorIs(t, err, billingauth.ErrUnauthenticated)
}
