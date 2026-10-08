package billing

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCreateProductEntitlementListDecoding(t *testing.T) {
	for _, raw := range []string{`{"key":"p"}`, `{"key":"p","entitlements":[]}`, `{"key":"p","entitlements":["post:101","premium"]}`} {
		var params CreateProductParams
		require.NoError(t, json.Unmarshal([]byte(raw), &params), raw)
		require.Equal(t, "p", params.Key)
	}
	for _, raw := range []string{
		`{"entitlements":null}`,
		`{"entitlements":{"premium":null}}`,
		`{"entitlements_spec":{"premium":null}}`,
		`{"entitlements":[1]}`,
	} {
		var params CreateProductParams
		require.Error(t, json.Unmarshal([]byte(raw), &params), raw)
	}
}
