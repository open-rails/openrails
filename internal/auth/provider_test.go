package auth

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestNewVerifierRejectsOnlyBlankIssuers(t *testing.T) {
	t.Parallel()

	_, err := NewIssuerVerifier([]string{" ", ""}, "openrails")
	require.ErrorContains(t, err, "at least one non-empty auth issuer is required")
}
