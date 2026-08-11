package authkit

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestNewVerifierDelegatedAuthenticator_Validation(t *testing.T) {
	t.Parallel()

	// The merchant pin must be a real merchant id, checked at boot — a bad pin
	// failing per-request would reproduce the th#1765 silent-403 shape.
	_, err := NewVerifierDelegatedAuthenticator([]string{"https://auth.host.example"}, "openrails", "not-a-uuid")
	require.ErrorContains(t, err, "merchant id")
	require.ErrorContains(t, err, "bound merchant")

	// Issuer rules are shared with NewVerifierAuthenticator.
	_, err = NewVerifierDelegatedAuthenticator(nil, "openrails", "6a68e70a-4dd9-4b39-a3ba-4657303c6f70")
	require.ErrorContains(t, err, "at least one auth issuer is required")

	authn, err := NewVerifierDelegatedAuthenticator([]string{"https://auth.host.example"}, "openrails", "6a68e70a-4dd9-4b39-a3ba-4657303c6f70")
	require.NoError(t, err)
	require.NotNil(t, authn)
}
