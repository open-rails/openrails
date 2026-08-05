package redact

import (
	"net/url"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSecretsScrubsCredentialQueryParams(t *testing.T) {
	cases := []struct{ in, want string }{
		{
			`rpc call getBalance() on https://mainnet.helius-rpc.com/?api-key=abc123 status code: 500`,
			`rpc call getBalance() on https://mainnet.helius-rpc.com/?api-key=REDACTED status code: 500`,
		},
		{`Get "https://x/y?apiKey=s3cret&z=1": timeout`, `Get "https://x/y?apiKey=REDACTED&z=1": timeout`},
		{`https://x/y?access_token=abc`, `https://x/y?access_token=REDACTED`},
		{`https://x/y?page=2&limit=10`, `https://x/y?page=2&limit=10`},
	}
	for _, c := range cases {
		require.Equal(t, c.want, Secrets(c.in))
	}
}

func TestStripSecretQuerySplitsCredentials(t *testing.T) {
	safe, secret := StripSecretQuery("https://rpc.example/?api-key=k1&cluster=mainnet")
	require.Equal(t, "https://rpc.example/?cluster=mainnet", safe)
	require.Equal(t, url.Values{"api-key": {"k1"}}, secret)

	safe, secret = StripSecretQuery("https://rpc.example/path")
	require.Equal(t, "https://rpc.example/path", safe)
	require.Nil(t, secret)
}

func TestURLMasksInPlace(t *testing.T) {
	require.Equal(t, "https://x/y?api-key=REDACTED", URL("https://x/y?api-key=abc"))
	require.Equal(t, "https://x/y?a=1", URL("https://x/y?a=1"))
}
