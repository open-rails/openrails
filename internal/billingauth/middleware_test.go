package billingauth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

const testUserID = "5d8a3f6e-9c1b-4a7d-8e2f-0b1c2d3e4f5a"

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
	endpoint := ExplicitCredentials(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := authn.Authenticate(r.Context(), r); err != nil {
			WriteJSONError(w, http.StatusUnauthorized, "authentication_required", "authentication required")
			return
		}
		mutations++
		w.WriteHeader(http.StatusNoContent)
	}))
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
