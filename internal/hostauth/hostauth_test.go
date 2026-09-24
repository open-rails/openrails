package hostauth

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	authkit "github.com/open-rails/authkit"
	"github.com/open-rails/authkit/verify"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/permissions"
	"github.com/open-rails/openrails/pkg/billingauth"
)

const boundMerchant = "6a68e70a-4dd9-4b39-a3ba-4657303c6f70"

type hostVerifier struct{ claims verify.Claims }

func (h hostVerifier) VerifyRequest(*http.Request) (verify.Claims, error) { return h.claims, nil }

func req() *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/billing/v1/merchant/settings", nil)
	r.Header.Set("Authorization", "Bearer host.session.token")
	return r
}

// Misconfiguration fails at boot, never as a quiet downgrade at request time.
func TestConstructorsRefuseLoudly(t *testing.T) {
	_, err := NewAuthenticator(nil)
	require.ErrorContains(t, err, "verifier is required")
	_, err = NewDelegatedAuthenticator(nil, boundMerchant)
	require.ErrorContains(t, err, "verifier is required")
	for _, bad := range []string{"", "not-a-uuid", "acme"} {
		_, err = NewDelegatedAuthenticator(hostVerifier{}, bad)
		require.ErrorContains(t, err, "merchant id", bad)
	}
	_, err = NewVerifierAuthenticator(nil, "aud")
	require.ErrorContains(t, err, "auth issuer")
	_, err = NewVerifierDelegatedAuthenticator(nil, "aud", boundMerchant)
	require.ErrorContains(t, err, "auth issuer")
}

func TestOptionsReachThePrincipal(t *testing.T) {
	v := hostVerifier{verify.Claims{UserID: "8b0f9f0e-9a4b-4a5f-9f3a-2f8f0a1b2c3d", Roles: []string{"owner"}, Issuer: "https://auth.host.example"}}
	banned := errors.New("user is banned")
	var admission error
	admit := func(context.Context, *http.Request, verify.Claims) error { return admission }
	a, err := NewDelegatedAuthenticator(v, boundMerchant, nil, WithMerchantSlug("acme"), WithIssuer("openrails:self"),
		WithAdmission(admit),
		WithPermissionResolver(func(context.Context, *http.Request, verify.Claims) ([]string, error) {
			return []string{permissions.MerchantAll}, nil
		}))
	require.NoError(t, err)
	p, err := a.AuthenticateDelegated(t.Context(), req())
	require.NoError(t, err)
	require.Equal(t, []string{boundMerchant, "acme", "openrails:self"}, []string{p.MerchantID, p.MerchantSlug, p.Issuer})
	require.Equal(t, []string{permissions.MerchantAll}, p.Permissions)

	user, err := NewAuthenticator(v, nil, WithoutTokenRoles(), WithUserAdmission(admit))
	require.NoError(t, err)
	uc, err := user.Authenticate(t.Context(), req())
	require.NoError(t, err)
	require.Empty(t, uc.Roles)

	admission = banned
	_, err = a.AuthenticateDelegated(t.Context(), req())
	require.ErrorIs(t, err, billingauth.ErrUnauthenticated)
	_, err = user.Authenticate(t.Context(), req())
	require.ErrorIs(t, err, billingauth.ErrUnauthenticated)
}

type fakeIdentity struct {
	users     map[string]*authkit.AdminUser
	usernames map[string]*authkit.User
	err       error
}

func (f fakeIdentity) AdminGetUser(_ context.Context, id string) (*authkit.AdminUser, error) {
	if u, ok := f.users[id]; ok || f.err != nil {
		return u, f.err
	}
	return nil, authkit.ErrUserNotFound
}

func (f fakeIdentity) GetUserByUsername(_ context.Context, name string) (*authkit.User, error) {
	if u, ok := f.usernames[name]; ok {
		return u, nil
	}
	return nil, authkit.ErrUserNotFound
}

// Billing email goes only to live users with a usable address; lookup misses
// are "no user" but an outage is an error.
func TestDirectoryUsableEmailPolicy(t *testing.T) {
	str := func(s string) *string { return &s }
	gone := time.Now()
	const live, deleted, noEmail, blank = "aaaaaaaa-1111-4111-8111-111111111111", "22222222-2222-4222-8222-222222222222", "33333333-3333-4333-8333-333333333333", "44444444-4444-4444-8444-444444444444"
	d := NewDirectory(fakeIdentity{
		users: map[string]*authkit.AdminUser{
			live:    {ID: live, Email: str("a@example.com"), Username: str("alice")},
			deleted: {ID: deleted, Email: str("d@example.com"), DeletedAt: &gone},
			noEmail: {ID: noEmail},
			blank:   {ID: blank, Email: str("")},
		},
		usernames: map[string]*authkit.User{"alice": {ID: live}, "gone": {ID: deleted, DeletedAt: &gone}},
	})
	ctx := context.Background()

	username, email, ok, err := d.EmailIdentity(ctx, "AAAAAAAA-1111-4111-8111-111111111111")
	require.NoError(t, err)
	require.True(t, ok, "input UUID is canonicalized")
	require.Equal(t, []string{"alice", "a@example.com"}, []string{username, email})
	for _, id := range []string{deleted, noEmail, blank, "55555555-5555-4555-8555-555555555555", "not-a-uuid"} {
		exists, err := d.Exists(ctx, id)
		require.NoError(t, err, id)
		require.False(t, exists, id)
	}
	for _, miss := range []error{pgx.ErrNoRows, authkit.ErrUserNotFound} {
		exists, err := NewDirectory(fakeIdentity{err: miss}).Exists(ctx, live)
		require.NoError(t, err)
		require.False(t, exists)
	}
	outage := errors.New("authkit down")
	_, err = NewDirectory(fakeIdentity{err: outage}).Exists(ctx, live)
	require.ErrorIs(t, err, outage)

	id, err := d.GetUserIDByUsername(ctx, "alice")
	require.NoError(t, err)
	require.Equal(t, live, id)
	for _, name := range []string{"gone", "nobody"} {
		_, err = d.GetUserIDByUsername(ctx, name)
		require.ErrorIs(t, err, authkit.ErrUserNotFound, name)
	}
	require.Nil(t, NewDirectory(nil))
	_, err = (*Directory)(nil).GetUserIDByUsername(ctx, "alice")
	require.ErrorContains(t, err, "client is required")
}
