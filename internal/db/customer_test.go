package db_test

import (
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/db"
)

// ResolveCustomerID is pure derivation (#364): a UUID subject IS its own
// payable id, empty matches nothing, anything else is rejected.
func TestResolveCustomerID(t *testing.T) {
	uid := uuid.New()

	got, err := db.ResolveCustomerID(uid.String())
	require.NoError(t, err)
	require.Equal(t, uid, got)

	// Case-insensitive: uppercase form parses to the same id.
	got, err = db.ResolveCustomerID(" " + strings.ToUpper(uid.String()) + " ")
	require.NoError(t, err)
	require.Equal(t, uid, got)

	got, err = db.ResolveCustomerID("")
	require.NoError(t, err)
	require.Equal(t, uuid.Nil, got)

	_, err = db.ResolveCustomerID("legacy-user-123")
	require.ErrorContains(t, err, "UUID-only")
}
