package repo

import (
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// ResolveTenantSubjectID is pure derivation (#364): a UUID subject IS its own
// payable id, empty matches nothing, anything else is rejected.
func TestResolveTenantSubjectID(t *testing.T) {
	uid := uuid.New()

	got, err := ResolveTenantSubjectID(uid.String())
	require.NoError(t, err)
	require.Equal(t, uid, got)

	// Case-insensitive: uppercase form parses to the same id.
	got, err = ResolveTenantSubjectID(" " + strings.ToUpper(uid.String()) + " ")
	require.NoError(t, err)
	require.Equal(t, uid, got)

	got, err = ResolveTenantSubjectID("")
	require.NoError(t, err)
	require.Equal(t, uuid.Nil, got)

	_, err = ResolveTenantSubjectID("legacy-user-123")
	require.ErrorContains(t, err, "UUID-only")
}
