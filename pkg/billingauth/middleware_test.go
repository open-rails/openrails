package billingauth

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

const testUserID = "5d8a3f6e-9c1b-4a7d-8e2f-0b1c2d3e4f5a"

func TestRequiredAndOptionalGateOnUUIDSubject(t *testing.T) {
	fixed := func(uc UserContext, err error) Authenticator {
		return AuthenticatorFunc(func(context.Context, *http.Request) (UserContext, error) { return uc, err })
	}
	for _, tc := range []struct {
		authn      Authenticator
		reqStatus  int
		reqMessage string
	}{
		{fixed(UserContext{UserID: testUserID, Merchant: "acme"}, nil), 200, ""},
		{fixed(UserContext{}, ErrUnauthenticated), 401, "authentication required"},
		{fixed(UserContext{}, errors.New("token expired")), 401, "token expired"},
		{fixed(UserContext{UserID: "legacy-user-123"}, nil), 401, `subject "legacy-user-123" is not a UUID`},
		{nil, 500, "authentication disabled"},
	} {
		var seen UserContext
		var found, called bool
		next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called = true; seen, found = FromContext(r.Context()) })

		rec := httptest.NewRecorder()
		Required(tc.authn)(next).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/x", nil))
		require.Equal(t, tc.reqStatus, rec.Code)
		require.Equal(t, tc.reqStatus == 200, called && found)
		if tc.reqStatus == 200 {
			require.Equal(t, UserContext{UserID: testUserID, Merchant: "acme"}, seen)
		} else {
			var body struct {
				Error struct{ Type, Code, Message string } `json:"error"`
			}
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
			require.Contains(t, body.Error.Message, tc.reqMessage)
			if tc.reqStatus == 401 {
				require.Equal(t, "authentication_error", body.Error.Type)
				require.Equal(t, "unauthorized", body.Error.Code)
			}
		}

		called, found = false, false
		rec = httptest.NewRecorder()
		Optional(tc.authn)(next).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/x", nil))
		require.Equal(t, 200, rec.Code, "optional never rejects")
		require.True(t, called)
		require.Equal(t, tc.reqStatus == 200, found, "failures proceed anonymously")
	}
}

func TestCookieCredentialsRequireOptInAndExactOrigin(t *testing.T) {
	const origin = "https://merchant.example"
	wrap, err := CookieAuthentication(origin)
	require.NoError(t, err)
	authn := AuthenticatorFunc(func(_ context.Context, r *http.Request) (UserContext, error) {
		if c, err := r.Cookie("session"); r.Header.Get("Authorization") == "Bearer approved" || err == nil && c.Value == "approved" {
			return UserContext{UserID: testUserID}, nil
		}
		return UserContext{}, ErrUnauthenticated
	})
	mutations := 0
	endpoint := ExplicitCredentials(Required(authn)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mutations++
		w.WriteHeader(http.StatusNoContent)
	})))
	const attacker = "https://attacker.example"
	for _, tc := range []struct {
		name, method, credential string
		origins                  []string
		optin                    bool
		want                     int
	}{
		{name: "default ignores cookie", origins: []string{origin}, want: 401},
		{name: "same origin", origins: []string{origin}, optin: true, want: 204},
		{name: "cross origin", origins: []string{attacker}, optin: true, want: 403},
		{name: "same-site sibling", origins: []string{"https://other.merchant.example"}, optin: true, want: 403},
		{name: "missing origin", optin: true, want: 403},
		{name: "opaque origin", origins: []string{"null"}, optin: true, want: 403},
		{name: "duplicate origins", origins: []string{origin, origin}, optin: true, want: 403},
		{name: "safe method", method: http.MethodGet, origins: []string{attacker}, optin: true, want: 204},
		{name: "explicit bearer", origins: []string{attacker}, credential: "Bearer approved", optin: true, want: 204},
		{name: "invalid bearer never falls back to cookie", origins: []string{origin}, credential: "Bearer wrong", optin: true, want: 401},
	} {
		method := tc.method
		if method == "" {
			method = http.MethodPost
		}
		r := httptest.NewRequest(method, origin+"/billing/v1/me/action", strings.NewReader(`{}`))
		r.AddCookie(&http.Cookie{Name: "session", Value: "approved"})
		for _, o := range tc.origins {
			r.Header.Add("Origin", o)
		}
		if tc.credential != "" {
			r.Header.Set("Authorization", tc.credential)
		}
		h := endpoint
		if tc.optin {
			h = wrap(h)
		}
		before := mutations
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		require.Equal(t, tc.want, w.Code, tc.name)
		require.Equal(t, tc.want == 204, mutations > before, tc.name)
	}
}

func TestCookieOriginMustBeBrowserCanonical(t *testing.T) {
	for _, ok := range []string{"https://merchant.example", "https://merchant.example:8443", "https://[2001:db8::1]", "http://localhost:8080", "http://tenant.localhost:8080", "http://127.0.0.1:8080", "http://[::1]:8080"} {
		_, err := CookieAuthentication(ok)
		require.NoError(t, err, ok)
	}
	for _, bad := range []string{
		"", "*", "null", "https://merchant.example/", "https://user@merchant.example", "https://merchant.example?q=1",
		"https://merchant.example#part", "https://example.com?", "https://example.com#", "//merchant.example",
		"ftp://merchant.example", "https://*.example", "https://:443", "https://EXAMPLE.com", "https://example.com:443",
		"http://localhost:80", "https://example.com:0443", "https://example.com:70000", "http://example.com",
		"http://10.0.0.1:8080", "https://127.1", "https://2130706433", "https://0x7f000001", "https://[0:0::1]",
		"https://[::ffff:127.0.0.1]", "https://-bad.example",
	} {
		_, err := CookieAuthentication(bad)
		require.Error(t, err, bad)
	}
}

func TestUserContextMatchingAndEntitlementFreshness(t *testing.T) {
	uc := UserContext{UserID: testUserID, Roles: []string{"Admin"}, Entitlements: []string{"Premium"}, MerchantRoles: []string{"Owner"}}
	require.True(t, uc.HasRole("admin"))
	require.False(t, uc.HasAnyMerchantRole("owner"), "merchant roles need a merchant")
	uc.Merchant = "acme"
	require.True(t, uc.HasAnyMerchantRole("viewer", "OWNER"))

	ok, err := uc.HasEntitlementFresh(context.Background(), nil, "premium")
	require.NoError(t, err)
	require.True(t, ok, "nil resolver trusts the verified token snapshot")
	revoked := EntitlementFreshnessResolverFunc(func(context.Context, UserContext, string) (bool, error) { return false, nil })
	ok, err = uc.HasEntitlementFresh(context.Background(), revoked, "premium")
	require.NoError(t, err)
	require.False(t, ok, "resolver reflects revocation before token expiry")
}
