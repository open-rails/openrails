package webhooks

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestStripeClientSecretRedactionPreservesExactJSONNumbers(t *testing.T) {
	raw := []byte(`{"client_secret":"top","id":"evt_fixture","data":{"object":{"amount":9007199254740993,"decimal":12345678901234567890.012300,"exponent":1.234567890123456789e+40,"client_secret_hint":"visible","description":"client_secret is a field","nested":[{"client_secret":"nested","amount":-9007199254740993},[{"client_secret":"deep"}]]},"previous_attributes":{"client_secret":"previous","amount":9007199254740995}}}`)
	scrubbed, err := redactStripeClientSecrets(raw)
	require.NoError(t, err)
	require.NotContains(t, string(scrubbed), `"client_secret":`)
	for _, exact := range []string{`"amount":9007199254740993`, `"amount":-9007199254740993`, `"amount":9007199254740995`, `"decimal":12345678901234567890.012300`, `"exponent":1.234567890123456789e+40`, `"client_secret_hint":"visible"`, `"description":"client_secret is a field"`, `"id":"evt_fixture"`} {
		require.Contains(t, string(scrubbed), exact)
	}
	require.True(t, json.Valid(scrubbed))
	again, err := redactStripeClientSecrets(scrubbed)
	require.NoError(t, err)
	require.Equal(t, scrubbed, again)
	for _, invalid := range []string{`{"client_secret":`, `{} {}`} {
		_, err := redactStripeClientSecrets([]byte(invalid))
		require.Error(t, err)
	}
}
