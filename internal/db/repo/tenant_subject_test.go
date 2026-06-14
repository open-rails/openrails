package repo

import (
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// ResolveMerchantSubjectID is pure derivation (#364): a UUID subject IS its own
// payable id, empty matches nothing, anything else is rejected.
func TestResolveMerchantSubjectID(t *testing.T) {
	uid := uuid.New()

	got, err := ResolveMerchantSubjectID(uid.String())
	require.NoError(t, err)
	require.Equal(t, uid, got)

	// Case-insensitive: uppercase form parses to the same id.
	got, err = ResolveMerchantSubjectID(" " + strings.ToUpper(uid.String()) + " ")
	require.NoError(t, err)
	require.Equal(t, uid, got)

	got, err = ResolveMerchantSubjectID("")
	require.NoError(t, err)
	require.Equal(t, uuid.Nil, got)

	_, err = ResolveMerchantSubjectID("legacy-user-123")
	require.ErrorContains(t, err, "UUID-only")
}
