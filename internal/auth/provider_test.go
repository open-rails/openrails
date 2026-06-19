package auth

import (
	"testing"

	authhttp "github.com/open-rails/authkit/http"
	"github.com/stretchr/testify/require"
)

func TestUserContextFromClaimsMapsFields(t *testing.T) {
	t.Parallel()

	uc := userContextFromClaims(authhttp.Claims{
		UserID:    "user-1",
		Email:     "test@example.com",
		SessionID: "session-1",
	})

	require.Equal(t, "user-1", uc.UserID)
	require.Equal(t, "test@example.com", uc.Email)
	require.Equal(t, "session-1", uc.SessionID)
}

func TestNewVerifierRejectsOnlyBlankIssuers(t *testing.T) {
	t.Parallel()

	_, err := NewIssuerVerifier([]string{" ", ""}, "openrails")
	require.ErrorContains(t, err, "at least one non-empty auth issuer is required")
}
