package contract

import (
	"bytes"
	"github.com/open-rails/openrails/internal/archivewire"
	"github.com/stretchr/testify/require"
	"testing"
)

func TestReadSubscriptionHistoryBeforeAttribution(t *testing.T) {
	p, transition := row(t, "subscription_status_transitions", map[string]string{
		"merchant_id": testMerchant, "id": "10000000-0000-0000-0000-000000000021", "subscription_id": "10000000-0000-0000-0000-000000000022",
		"to_status": "active", "decision": "created", "occurred_at": "2026-09-01 00:00:00+00",
	})
	var archive bytes.Buffer
	writer, err := archivewire.NewWriter(&archive, testMerchant)
	require.NoError(t, err)
	for _, profile := range Profiles {
		if profile.Name == "subscription_billing_changes" {
			continue
		}
		require.NoError(t, writer.Table(profile.Name))
		if profile.Name == p.Name {
			require.NoError(t, writer.Row(transition[:len(transition)-4]))
		}
	}
	require.NoError(t, writer.Close())
	seen := 0
	_, err = Read(bytes.NewReader(archive.Bytes()), nil, func(profile Profile, values []*string) error {
		require.Equal(t, p.Name, profile.Name)
		require.Equal(t, transition, values)
		seen++
		return nil
	})
	require.NoError(t, err)
	require.Equal(t, 1, seen)
}
