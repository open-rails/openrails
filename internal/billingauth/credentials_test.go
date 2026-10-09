package billingauth

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// An ambient cookie never reaches an Auth, with or without an explicit
// credential beside it.
func TestExplicitCredentialsStripCookies(t *testing.T) {
	var seen []string
	h := ExplicitCredentials(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Header.Get("Cookie")+"|"+r.Header.Get("Authorization"))
	}))
	for _, bearer := range []string{"", "Bearer token"} {
		r := httptest.NewRequest(http.MethodPost, "https://merchant.example/billing/v1/me/action", strings.NewReader(`{}`))
		r.AddCookie(&http.Cookie{Name: "session", Value: "approved"})
		if bearer != "" {
			r.Header.Set("Authorization", bearer)
		}
		h.ServeHTTP(httptest.NewRecorder(), r)
	}
	require.Equal(t, []string{"|", "|Bearer token"}, seen)
}
