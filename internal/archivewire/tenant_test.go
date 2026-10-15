package archivewire

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestTenantHeaderRoundTripAndLegacyCompatibility(t *testing.T) {
	const merchant = "11111111-1111-4111-8111-111111111111"
	const tenant = "22222222-2222-4222-8222-222222222222"
	for _, owner := range []string{"", tenant} {
		var buf bytes.Buffer
		writer, err := NewWriterWithTenant(&buf, merchant, owner, 7)
		require.NoError(t, err)
		require.NoError(t, writer.Table("example"))
		require.NoError(t, writer.Close())
		header, _, err := ReadHeader(bytes.NewReader(buf.Bytes()))
		require.NoError(t, err)
		require.Equal(t, owner, header.TenantID)
		require.EqualValues(t, 7, header.CatalogRevision)
		info, err := CopyVerified(&bytes.Buffer{}, bytes.NewReader(buf.Bytes()))
		require.NoError(t, err)
		require.Equal(t, owner, info.TenantID)
		if owner == "" {
			require.NotContains(t, buf.String(), "tenant_id")
			var legacy bytes.Buffer
			old, err := NewWriter(&legacy, merchant, 7)
			require.NoError(t, err)
			require.NoError(t, old.Table("example"))
			require.NoError(t, old.Close())
			require.Equal(t, legacy.Bytes(), buf.Bytes())
		}
	}
	_, err := NewWriterWithTenant(&bytes.Buffer{}, merchant, "wrong", 0)
	require.Error(t, err)
}
