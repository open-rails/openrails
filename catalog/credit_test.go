package catalog

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCreditCatalogExactYAMLAmounts(t *testing.T) {
	raw := []byte(`schema_version: 1
products:
  credits:
    display_name: Credits
    credit_grant: {currency: USD, amount: 9007199254740993}
    prices:
      deposit:
        currency: USD
        unit_amount: 0
        customer_amount: {min_amount: 1000000, max_amount: 1000000000}
`)
	app, err := ParseApplicationYAML(raw)
	require.NoError(t, err)
	require.Equal(t, int64(9007199254740993), *app.Products["credits"].CreditGrant.Value.Amount)
	require.Equal(t, int64(1000000000), app.Products["credits"].Prices["deposit"].CustomerAmount.Value.MaxAmount)
	wire, err := json.Marshal(app)
	require.NoError(t, err)
	require.Contains(t, string(wire), `"amount":"9007199254740993"`)
	decoded, err := ParseApplicationJSON(wire)
	require.NoError(t, err)
	require.Equal(t, app, decoded)
	for _, bad := range []string{
		`{"schema_version":1,"products":{"x":{"credit_grant":{"currency":"USD","amount":1.5}}}}`,
		`{"schema_version":1,"products":{"x":{"credit_grant":{"currency":"USD","amount":9223372036854775808}}}}`,
		`{"schema_version":1,"products":{"x":{"credit_grant":{"currency":"USD","amount":"1","unknown":true}}}}`,
	} {
		_, err := ParseApplicationJSON([]byte(bad))
		require.Error(t, err)
	}
}
