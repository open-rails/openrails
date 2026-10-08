package contract

import (
	"bytes"
	"testing"

	"github.com/open-rails/openrails/internal/archivewire"
	"github.com/stretchr/testify/require"
)

// Older archives carry no native-credit promise. Neither adding those nullable
// columns nor deriving an old revision may replace the original price UUID.
func TestReadPreCreditCatalogRows(t *testing.T) {
	for _, table := range []string{"products", "prices", "payments"} {
		for _, withRevision := range []bool{true, false} {
			if table == "payments" && !withRevision {
				continue
			}
			p, values := row(t, table, map[string]string{"merchant_id": testMerchant, "id": "10000000-0000-0000-0000-000000000099"})
			fields := values[:len(values)-1]
			if table != "payments" {
				fields[1] = new("3")
				if !withRevision {
					fields = append(append([]*string{}, fields[:1]...), fields[2:]...)
				}
			}
			var artifact bytes.Buffer
			writer, err := archivewire.NewWriter(&artifact, testMerchant)
			require.NoError(t, err)
			for _, profile := range Profiles {
				require.NoError(t, writer.Table(profile.Name))
				if profile.Name == table {
					require.NoError(t, writer.Row(fields))
				}
			}
			require.NoError(t, writer.Close())
			count := 0
			_, err = Read(bytes.NewReader(artifact.Bytes()), nil, func(got Profile, restored []*string) error {
				count++
				require.Equal(t, p.Name, got.Name)
				require.Equal(t, "10000000-0000-0000-0000-000000000099", *value(got, restored, "id"))
				require.Nil(t, restored[len(restored)-1], "historical rows do not adopt current product benefits")
				if table != "payments" {
					require.Equal(t, withRevision, value(got, restored, "revision") != nil)
				}
				return nil
			})
			require.NoError(t, err, table)
			require.Equal(t, 1, count)
		}
	}
}
