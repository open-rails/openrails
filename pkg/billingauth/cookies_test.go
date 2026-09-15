package billingauth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCookieAuthenticationBoundary(t *testing.T) {
	const origin = "https://merchant.example"
	wrap, err := CookieAuthentication(origin)
	require.NoError(t, err)
	mutations := 0
	auth := AuthenticatorFunc(func(_ context.Context, r *http.Request) (UserContext, error) {
		if r.Header.Get("Authorization") == "Bearer approved" {
			return UserContext{UserID: "abdd9f2c-04df-48ce-8a6f-f05843175cc6"}, nil
		}
		if cookie, err := r.Cookie("session"); err == nil && cookie.Value == "approved" {
			return UserContext{UserID: "abdd9f2c-04df-48ce-8a6f-f05843175cc6"}, nil
		}
		return UserContext{}, ErrUnauthenticated
	})
	endpoint := ExplicitCredentials(Required(auth)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { mutations++; w.WriteHeader(http.StatusNoContent) })))
	for _, tc := range []struct {
		name, origin, credential, body string
		optin                          bool
		want                           int
	}{
		{name: "default ignores cookie", origin: origin, want: 401},
		{name: "same origin empty body", origin: origin, optin: true, want: 204},
		{name: "same origin JSON", origin: origin, body: `{}`, optin: true, want: 204},
		{name: "cross origin form", origin: "https://attacker.example", body: `{}`, optin: true, want: 403},
		{name: "same site sibling", origin: "https://other.merchant.example", optin: true, want: 403},
		{name: "missing origin", optin: true, want: 403},
		{name: "opaque origin", origin: "null", optin: true, want: 403},
		{name: "explicit bearer", origin: "https://attacker.example", credential: "Bearer approved", optin: true, want: 204},
		{name: "invalid bearer cannot use cookie", origin: origin, credential: "Bearer wrong", optin: true, want: 401},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, origin+"/billing/v1/me/action", strings.NewReader(tc.body))
			r.AddCookie(&http.Cookie{Name: "session", Value: "approved"})
			r.Header.Set("Origin", tc.origin)
			r.Header.Set("Authorization", tc.credential)
			r.Header.Set("Content-Type", "text/plain")
			h := endpoint
			if tc.optin {
				h = wrap(h)
			}
			before := mutations
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			require.Equal(t, tc.want, w.Code, w.Body.String())
			if tc.want != 204 {
				require.Equal(t, before, mutations)
			}
		})
	}
	for _, bad := range []string{"*", "https://merchant.example/", "https://user@merchant.example", "https://merchant.example?q=1", "https://merchant.example#part", "//merchant.example", "ftp://merchant.example", "https://*", "https://*.example", "https://:443", "https://EXAMPLE.com", "https://example.com:443", "https://example.com:0443", "http://example.com", "https://127.1", "https://2130706433", "https://0x7f000001", "https://[0:0::1]", "https://example.com?", "https://example.com#"} {
		_, err := CookieAuthentication(bad)
		require.Error(t, err, bad)
	}
}

func TestCookieAuthenticationCanonicalOrigins(t *testing.T) {
	for _, origin := range []string{"https://merchant.example", "https://merchant.example:8443", "https://[2001:db8::1]", "http://localhost:8080", "http://tenant.localhost:8080", "http://127.0.0.1:8080", "http://[::1]:8080"} {
		_, err := CookieAuthentication(origin)
		require.NoError(t, err, origin)
	}
}
