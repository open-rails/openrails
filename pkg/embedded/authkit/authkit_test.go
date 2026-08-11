package authkit

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

const boundMerchant = "6a68e70a-4dd9-4b39-a3ba-4657303c6f70"

type hostVerifier struct {
	claims verify.Claims
	err    error
	calls  int
}

func (h *hostVerifier) VerifyRequest(*http.Request) (verify.Claims, error) {
	h.calls++
	return h.claims, h.err
}

func req(path string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, path, nil)
	r.Header.Set("Authorization", "Bearer host.session.token")
	return r
}

// The construction-time refusals: a misconfigured seam must fail at boot with
// a message naming the fix, never at request time with a quiet downgrade.
func TestConstructorsRefuseLoudly(t *testing.T) {
	t.Parallel()

	_, err := NewDelegatedAuthenticator(nil, boundMerchant)
	require.ErrorContains(t, err, "verifier is required")

	_, err = NewAuthenticator(nil)
	require.ErrorContains(t, err, "verifier is required")

	_, err = NewDelegatedAuthenticator(&hostVerifier{}, "not-a-uuid")
	require.ErrorContains(t, err, "merchant id")

	_, err = NewVerifierDelegatedAuthenticator(nil, "aud", boundMerchant)
	require.ErrorContains(t, err, "auth issuer")

	// Two answers to the same question: silently preferring one would hide a
	// grant the host believes it configured.
	_, err = NewDelegatedAuthenticator(&hostVerifier{}, boundMerchant,
		WithRolePermissions(func([]string) []string { return nil }),
		WithPermissionResolver(func(context.Context, *http.Request, verify.Claims) ([]string, error) { return nil, nil }),
	)
	require.ErrorContains(t, err, "mutually exclusive")
}

// The default stays the or#913 canonical preset, and the host's verifier is
// the one that runs.
func TestDelegatedAuthenticator_DefaultsToCanonicalPreset(t *testing.T) {
	t.Parallel()

	v := &hostVerifier{claims: verify.Claims{
		UserID: "8b0f9f0e-9a4b-4a5f-9f3a-2f8f0a1b2c3d",
		Roles:  []string{"owner"},
		Issuer: "https://auth.host.example",
	}}
	a, err := NewDelegatedAuthenticator(v, boundMerchant)
	require.NoError(t, err)

	p, err := a.AuthenticateDelegated(t.Context(), req("/billing/v1/me/balance"))
	require.NoError(t, err)
	require.Equal(t, 1, v.calls, "the host's own verifier verifies the request")
	require.Equal(t, permissions.ForRoles("owner"), p.Permissions)
	require.Equal(t, boundMerchant, p.MerchantID)
	require.Equal(t, "https://auth.host.example", p.Issuer)
}

// doujins #803's two blockers, through the exported options: a liveness veto
// on a token that verifies, and a request-scoped grant.
func TestDelegatedAuthenticator_AdmissionAndResolver(t *testing.T) {
	t.Parallel()

	v := &hostVerifier{claims: verify.Claims{UserID: "8b0f9f0e-9a4b-4a5f-9f3a-2f8f0a1b2c3d"}}
	banned := false
	a, err := NewDelegatedAuthenticator(v, boundMerchant,
		WithMerchantSlug("acme"),
		WithIssuer("openrails:self"),
		WithAdmission(func(context.Context, *http.Request, verify.Claims) error {
			if banned {
				return errors.New("user is banned")
			}
			return nil
		}),
		WithPermissionResolver(func(_ context.Context, r *http.Request, _ verify.Claims) ([]string, error) {
			perms := []string{permissions.CustomerAll}
			if r.URL.Path == "/billing/v1/merchant/settings" {
				perms = append(perms, permissions.MerchantAll)
			}
			return perms, nil
		}),
	)
	require.NoError(t, err)

	p, err := a.AuthenticateDelegated(t.Context(), req("/billing/v1/merchant/settings"))
	require.NoError(t, err)
	require.Equal(t, []string{permissions.CustomerAll, permissions.MerchantAll}, p.Permissions)
	require.Equal(t, "acme", p.MerchantSlug)
	require.Equal(t, "openrails:self", p.Issuer)

	banned = true
	_, err = a.AuthenticateDelegated(t.Context(), req("/billing/v1/merchant/settings"))
	require.ErrorIs(t, err, billingauth.ErrUnauthenticated)
}

// The non-delegated twin carries the same veto, plus the #774 posture: a host
// that authorizes from a live authority does not have to pass the token's
// stale role snapshot through.
func TestAuthenticator_AdmissionAndTokenRoles(t *testing.T) {
	t.Parallel()

	claims := verify.Claims{
		UserID: "8b0f9f0e-9a4b-4a5f-9f3a-2f8f0a1b2c3d",
		Roles:  []string{"admin"},
		Email:  "user@host.example",
	}

	withRoles, err := NewAuthenticator(&hostVerifier{claims: claims})
	require.NoError(t, err)
	uc, err := withRoles.Authenticate(t.Context(), req("/billing/v1/checkout"))
	require.NoError(t, err)
	require.Equal(t, []string{"admin"}, uc.Roles)
	require.NoError(t, uc.ValidateSubject())

	noRoles, err := NewAuthenticator(&hostVerifier{claims: claims}, WithoutTokenRoles())
	require.NoError(t, err)
	uc, err = noRoles.Authenticate(t.Context(), req("/billing/v1/checkout"))
	require.NoError(t, err)
	require.Empty(t, uc.Roles)
	require.Equal(t, "user@host.example", uc.Email)

	vetoed, err := NewAuthenticator(&hostVerifier{claims: claims}, WithUserAdmission(
		func(context.Context, *http.Request, verify.Claims) error { return errors.New("not live") },
	))
	require.NoError(t, err)
	_, err = vetoed.Authenticate(t.Context(), req("/billing/v1/checkout"))
	require.ErrorIs(t, err, billingauth.ErrUnauthenticated)
}
