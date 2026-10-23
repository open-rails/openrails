package contract

import (
	"bytes"
	"testing"

	"github.com/open-rails/openrails/internal/archivewire"
	"github.com/stretchr/testify/require"
)

func TestOlderUsageArchivePreservesExplicitZeroCredit(t *testing.T) {
	profile, values := row(t, "money_settings", map[string]string{"merchant_id": testMerchant, "customer_id": testMerchant, "currency": "USD", "billing_mode": "prepaid", "credit_limit_amount": "0"})
	legacy := values[:len(profile.Columns)-5]
	var data bytes.Buffer
	writer, err := archivewire.NewWriter(&data, testMerchant)
	require.NoError(t, err)
	for _, p := range Profiles {
		require.NoError(t, writer.Table(p.Name))
		if p.Name == "money_settings" {
			require.NoError(t, writer.Row(legacy))
		}
	}
	require.NoError(t, writer.Close())
	seen := false
	_, err = Read(bytes.NewReader(data.Bytes()), nil, func(p Profile, values []*string) error {
		if p.Name == "money_settings" {
			seen = true
			require.Equal(t, "true", *value(p, values, "credit_limit_override"))
			require.Equal(t, "0", *value(p, values, "credit_limit_amount"))
			require.Nil(t, value(p, values, "usage_billing_anchor"))
		}
		return nil
	})
	require.NoError(t, err)
	require.True(t, seen)
}
