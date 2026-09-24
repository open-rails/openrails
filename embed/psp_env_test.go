package embed

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func envLookup(values map[string]string) func(string) (string, bool) {
	return func(name string) (string, bool) { v, ok := values[name]; return v, ok }
}

func TestPSPFromEnv(t *testing.T) {
	got, err := PSPFromEnv(" Stripe ", envLookup(map[string]string{
		"STRIPE_ACCOUNT_ID": "acct_1", "STRIPE_SECRET_KEY": " sk_test_1 ", "STRIPE_WEBHOOK_SIGNING_SECRET": "whsec_1",
		"STRIPE_PUBLISHABLE_KEY": "pk_test_1", "STRIPE_WEBHOOK_SIGNING_SECRET_THIN": "", "STRIPE_UNRELATED": "x",
	}))
	require.NoError(t, err)
	require.Equal(t, PSPConfig{"stripe": {AccountID: "acct_1",
		Secrets:  map[string]string{"secret_key": "sk_test_1", "webhook_signing_secret": "whsec_1"},
		Settings: map[string]any{"publishable_key": "pk_test_1"}}}, got, "unset optional slots and unknown variables are omitted")

	got, err = PSPFromEnv("nmi-eu", envLookup(map[string]string{
		"NMI_EU_RAIL": "NMI", "NMI_EU_ACCOUNT_ID": "123", "NMI_EU_SECURITY_KEY": "s", "NMI_EU_WEBHOOK_SIGNING_SECRET": "w",
		"NMI_EU_TOKENIZATION_KEY": "t", "NMI_EU_ENDPOINT_DEPLOYMENT": "gateway",
	}))
	require.NoError(t, err)
	require.Equal(t, PSPConfig{"nmi": {AccountID: "123",
		Secrets:  map[string]string{"security_key": "s", "webhook_signing_secret": "w"},
		Settings: map[string]any{"tokenization_key": "t", "endpoint_deployment": "gateway"}}}, got, "the key names the variables; RAIL selects the rail")

	for name, tc := range map[string]struct {
		key  string
		env  map[string]string
		want string
	}{
		"blank key":           {" ", nil, "key is required"},
		"unknown rail":        {"paypal", map[string]string{"PAYPAL_ACCOUNT_ID": "1"}, "unknown rail"},
		"solana":              {"solana", map[string]string{"SOLANA_ACCOUNT_ID": "1"}, "programmatically"},
		"missing account":     {"stripe", map[string]string{"STRIPE_SECRET_KEY": "sk_test_1", "STRIPE_WEBHOOK_SIGNING_SECRET": "w"}, "STRIPE_ACCOUNT_ID"},
		"missing webhook":     {"nmi", map[string]string{"NMI_ACCOUNT_ID": "1", "NMI_SECURITY_KEY": "s"}, "NMI_WEBHOOK_SIGNING_SECRET"},
		"blank required slot": {"nmi", map[string]string{"NMI_ACCOUNT_ID": "1", "NMI_SECURITY_KEY": " ", "NMI_WEBHOOK_SIGNING_SECRET": "w"}, "NMI_SECURITY_KEY"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := PSPFromEnv(tc.key, envLookup(tc.env))
			require.ErrorContains(t, err, tc.want)
		})
	}
}
