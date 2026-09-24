package redact

import (
	"net/url"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSecretsScrubsCredentialQueryValuesInText(t *testing.T) {
	for in, want := range map[string]string{
		`rpc on https://mainnet.helius-rpc.com/?api-key=abc123 status: 500`:             `rpc on https://mainnet.helius-rpc.com/?api-key=REDACTED status: 500`,
		`Get "https://x/y?apiKey=s3cret&z=1": timeout`:                                  `Get "https://x/y?apiKey=REDACTED&z=1": timeout`,
		`https://x/y?a=1&client_secret=zz&password=pw&X-Amz-Signature=f&access_token=t`: `https://x/y?a=1&client_secret=REDACTED&password=REDACTED&X-Amz-Signature=REDACTED&access_token=REDACTED`,
		`https://x/y?page=2&limit=10`:                                                   `https://x/y?page=2&limit=10`,
	} {
		require.Equal(t, want, Secrets(in))
	}
	for _, name := range []string{"API_KEY", "apikey", "token", "Authorization", "passwd", "sig", " key "} {
		require.True(t, IsSecretParam(name), name)
	}
	for _, name := range []string{"page", "keyword", "signal"} {
		require.False(t, IsSecretParam(name), name)
	}
}

func TestURLAndStripSecretQuery(t *testing.T) {
	require.Equal(t, "https://x/y?a=1&sig=REDACTED", URL("https://x/y?sig=abc&a=1"))
	require.Equal(t, "https://x/y?a=1", URL("https://x/y?a=1"))
	require.Equal(t, "%zz?token=REDACTED", URL("%zz?token=abc"), "unparseable input falls back to text scrubbing")

	const raw = "https://rpc.example/?api-key=k1&cluster=mainnet&token=t1&token=t2"
	safe, secret := StripSecretQuery(raw)
	require.Equal(t, "https://rpc.example/?cluster=mainnet", safe)
	// Re-attaching the stripped parameters restores the original query.
	u, _ := url.Parse(safe)
	q := u.Query()
	for k, v := range secret {
		q[k] = v
	}
	orig, _ := url.Parse(raw)
	require.Equal(t, orig.Query(), q)
	safe, secret = StripSecretQuery("https://rpc.example/path")
	require.Equal(t, "https://rpc.example/path", safe)
	require.Nil(t, secret)
}
