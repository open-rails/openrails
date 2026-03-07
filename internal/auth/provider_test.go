package auth

import (
	"testing"

	jwt "github.com/golang-jwt/jwt/v5"
	"github.com/stretchr/testify/require"
)

func TestUserContextFromMapUsesSIDOnly(t *testing.T) {
	t.Parallel()

	uc := userContextFromMap(jwt.MapClaims{
		"sid":        "session-from-sid",
		"session_id": "legacy-session",
	})

	require.Equal(t, "session-from-sid", uc.SessionID)
}

func TestUserContextFromMapIgnoresLegacySessionID(t *testing.T) {
	t.Parallel()

	uc := userContextFromMap(jwt.MapClaims{
		"session_id": "legacy-session",
	})

	require.Empty(t, uc.SessionID)
}
