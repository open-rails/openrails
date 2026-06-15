package auth

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestFormatVerifierError(t *testing.T) {
	t.Parallel()

	t.Run("nil error yields empty string", func(t *testing.T) {
		require.Equal(t, "", FormatVerifierError(nil))
	})

	t.Run("maps known verifier codes", func(t *testing.T) {
		cases := map[string]string{
			"missing_token":             "missing_token",
			"bad_issuer for token":      "invalid_issuer",
			"bad_audience":              "invalid_audience",
			"invalid_token: expired":    "invalid_or_expired_token",
		}
		for in, want := range cases {
			require.Equal(t, want, FormatVerifierError(errors.New(in)), "input %q", in)
		}
	})

	// Security: an unrecognised verifier error MUST NOT be echoed verbatim to
	// the client. Anything with internal detail collapses to a generic code.
	t.Run("unknown error does not leak internal detail", func(t *testing.T) {
		leaky := errors.New("fetch https://idp.internal.example/.well-known/jwks.json: dial tcp 10.0.0.5:443: connection refused")
		got := FormatVerifierError(leaky)
		require.Equal(t, "invalid_or_expired_token", got)
		require.NotContains(t, got, "10.0.0.5")
		require.NotContains(t, got, "jwks.json")
		require.NotContains(t, got, "internal")
	})
}
