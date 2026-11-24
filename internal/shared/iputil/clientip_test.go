package iputil

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCountryOnlyFromCloudflare(t *testing.T) {
	tp := ParseTrustedProxies([]string{"10.0.0.0/8"}, []string{"173.245.48.0/20"})
	for name, c := range map[string]struct {
		peer, forwarded, want string
	}{
		"cloudflare connects":               {"173.245.48.5:443", "203.0.113.9", "FR"},
		"cloudflare behind the ingress":     {"10.0.0.2:80", "203.0.113.9, 173.245.48.5", "FR"},
		"another proxy received the client": {"10.0.0.2:80", "203.0.113.9", ""},
		"a client claims cloudflare":        {"10.0.0.2:80", "173.245.48.5, 203.0.113.9", ""},
		"an untrusted peer":                 {"198.51.100.7:443", "", ""},
		"every hop trusted":                 {"10.0.0.2:80", "173.245.48.5", ""},
	} {
		r, err := http.NewRequest(http.MethodGet, "http://openrails.invalid/", nil)
		require.NoError(t, err)
		r.RemoteAddr = c.peer
		if c.forwarded != "" {
			r.Header.Set("X-Forwarded-For", c.forwarded)
		}
		r.Header.Set("CF-IPCountry", "fr")
		require.Equal(t, c.want, tp.Country(r), name)
	}
	require.Equal(t, "203.0.113.9", tp.ClientIP(&http.Request{RemoteAddr: "10.0.0.2:80", Header: http.Header{"X-Forwarded-For": {"203.0.113.9, 173.245.48.5"}}}), "Cloudflare is a trusted proxy")
}
