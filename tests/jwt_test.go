package tests

import (
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// JWTClaims represents JWT claims for testing
type JWTClaims struct {
	jwt.RegisteredClaims
	Email   string   `json:"email"`
	Sub     string   `json:"sub"`
	Roles   []string `json:"roles,omitempty"`
	IsAdmin bool     `json:"is_admin,omitempty"`
}

// GenerateTestJWT generates a JWT token for testing
func GenerateTestJWT(t *testing.T, userID, email string, isAdmin bool) string {
	t.Helper()

	claims := &JWTClaims{
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   userID,
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
			IssuedAt:  jwt.NewNumericDate(time.Now()),
			Issuer:    "openrails-test",
			Audience:  []string{"openrails-test-app"},
		},
		Email:   email,
		Sub:     userID,
		IsAdmin: isAdmin,
	}

	if isAdmin {
		claims.Roles = []string{"admin"}
	}

	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	tokenString, err := token.SignedString([]byte("test-secret-key-for-testing-only"))
	require.NoError(t, err)

	return tokenString
}

// TestJWTGeneration tests JWT token generation
func TestJWTGeneration(t *testing.T) {
	t.Run("Generate Regular User Token", func(t *testing.T) {
		userID := "a1111111-1111-4111-8111-111111111111" // UUID-only subjects (#364)
		email := "test@example.com"
		token := GenerateTestJWT(t, userID, email, false)

		assert.NotEmpty(t, token, "Should generate a JWT token")
		assert.Contains(t, token, ".", "JWT should contain dots")

		// JWT should have 3 parts separated by dots
		parts := len(strings.Split(token, "."))
		assert.Equal(t, 3, parts, "JWT should have 3 parts")
	})

	t.Run("Generate Admin User Token", func(t *testing.T) {
		userID := "a2222222-2222-4222-8222-222222222222" // UUID-only subjects (#364)
		email := "admin@example.com"
		token := GenerateTestJWT(t, userID, email, true)

		assert.NotEmpty(t, token, "Should generate an admin JWT token")
		assert.Contains(t, token, ".", "JWT should contain dots")

		// Verify we can parse it back
		parsedToken, err := jwt.ParseWithClaims(token, &JWTClaims{}, func(token *jwt.Token) (interface{}, error) {
			return []byte("test-secret-key-for-testing-only"), nil
		})

		require.NoError(t, err, "Should be able to parse token")
		require.True(t, parsedToken.Valid, "Token should be valid")

		claims, ok := parsedToken.Claims.(*JWTClaims)
		require.True(t, ok, "Should be able to extract claims")
		assert.Equal(t, userID, claims.Sub, "Should have correct user ID")
		assert.Equal(t, email, claims.Email, "Should have correct email")
		assert.True(t, claims.IsAdmin, "Should be marked as admin")
		assert.Contains(t, claims.Roles, "admin", "Should have admin role")
	})
}
