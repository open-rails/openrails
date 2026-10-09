package payments

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

// A sale admits its product, not keys: a new payload carries none.
func TestNewSaleAdmitsNoKeys(t *testing.T) {
	raw, err := json.Marshal(NMISalePayload{AccessDurationHours: new(72)})
	require.NoError(t, err)
	require.NotContains(t, string(raw), `"entitlements"`)
	require.NotContains(t, string(raw), "legacy_entitlements")
}

// A sale accepted before product access keeps its admitted keys verbatim, so
// its accepted fingerprint still reproduces.
func TestAcceptedSaleKeepsItsAdmittedKeys(t *testing.T) {
	for _, admitted := range []string{`["post:101","premium"]`, `{"post:101":12,"premium":null}`, `{}`, `[]`} {
		var original NMISalePayload
		require.NoError(t, json.Unmarshal([]byte(`{"entitlements":`+admitted+`}`), &original))
		require.JSONEq(t, admitted, string(original.Entitlements))
		raw, err := json.Marshal(original)
		require.NoError(t, err)
		var resumed NMISalePayload
		require.NoError(t, json.Unmarshal(raw, &resumed))
		require.Equal(t, original, resumed)
	}
	var invalid NMISalePayload
	require.Error(t, json.Unmarshal([]byte(`{"entitlements":"premium"}`), &invalid))
}
