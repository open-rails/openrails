package contract

import (
	"bytes"
	"io"
	"strconv"
	"testing"

	"github.com/open-rails/openrails/catalog"
	"github.com/open-rails/openrails/internal/archivewire"
	"github.com/stretchr/testify/require"
)

func TestArchiveDurationCannotOverflowRuntime(t *testing.T) {
	for _, field := range []struct{ table, column string }{
		{"prices", "access_duration_hours"}, {"prices", "billing_interval_hours"},
		{"prices", "trial_duration_hours"}, {"subscriptions", "access_duration_hours_snapshot"},
	} {
		for _, hours := range []int{0, -1, catalog.MaxDurationHours, catalog.MaxDurationHours + 1} {
			fields := map[string]string{field.column: strconv.Itoa(hours)}
			if field.table == "subscriptions" {
				fields["collection_policy"], fields["rail"], fields["rail_subscription_id"] = "provider", "stripe", "sub_legacy"
			}
			profile, values := row(t, field.table, fields)
			err := ValidateValues(profile, values)
			if hours == catalog.MaxDurationHours {
				require.NoError(t, err, field.column)
			} else {
				require.Error(t, err, field.column)
			}
		}
	}
}

func TestReadHistoricalDurationRowsPreservesWireIdentity(t *testing.T) {
	for _, recurring := range []bool{true, false} {
		t.Run(map[bool]string{true: "recurring", false: "one_time"}[recurring], func(t *testing.T) {
			priceID := "10000000-0000-0000-0000-000000000003"
			priceProfile, price := row(t, "prices", map[string]string{"merchant_id": testMerchant, "id": priceID, "revision": "0", "access_duration_hours": "72"})
			for i, c := range priceProfile.Columns {
				if c.Name == "billing_interval_hours" {
					price[i] = new(map[bool]string{true: "true", false: "false"}[recurring])
				}
			}
			sub := legacyRow(t, LegacySubscriptions, map[string]string{"merchant_id": testMerchant, "price_id": priceID, "collection_policy": "provider", "rail": "stripe", "rail_subscription_id": "sub_legacy"})
			for i, c := range LegacySubscriptions.Columns {
				if c.Name == "access_duration_hours_snapshot" {
					sub = append(sub[:i], sub[i+1:]...)
					break
				}
			}
			var artifact bytes.Buffer
			writer, err := archivewire.NewVersionWriter(&artifact, 1, testMerchant)
			require.NoError(t, err)
			for _, profile := range ProfilesFor(1) {
				require.NoError(t, writer.Table(profile.Name))
				switch profile.Name {
				case "prices":
					require.NoError(t, writer.Row(price))
				case "subscriptions":
					require.NoError(t, writer.Row(sub))
				}
			}
			require.NoError(t, writer.Close())
			wire, err := archivewire.CopyVerified(io.Discard, bytes.NewReader(artifact.Bytes()))
			require.NoError(t, err)
			info, err := Read(bytes.NewReader(artifact.Bytes()), nil, func(p Profile, values []*string) error {
				switch p.Name {
				case "prices":
					require.Equal(t, new("72"), value(p, values, "access_duration_hours"))
					if recurring {
						require.Equal(t, new("72"), value(p, values, "billing_interval_hours"))
					} else {
						require.Nil(t, value(p, values, "billing_interval_hours"))
					}
				case "subscriptions":
					require.Equal(t, new("72"), value(p, values, "access_duration_hours_snapshot"))
				}
				return nil
			})
			require.NoError(t, err)
			require.True(t, info.LegacyDurations)
			require.Equal(t, wire.Digest, info.Digest, "historical conversion preserves original archive identity")
			require.Equal(t, int64(2), info.Rows)
		})
	}
}
