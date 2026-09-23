package embed

import (
	"reflect"
	"strings"
	"testing"
)

func envLookup(values map[string]string) func(string) (string, bool) {
	return func(name string) (string, bool) { v, ok := values[name]; return v, ok }
}

func TestPSPFromEnv(t *testing.T) {
	got, err := PSPFromEnv("stripe", envLookup(map[string]string{
		"STRIPE_ACCOUNT_ID": "acct_1", "STRIPE_SECRET_KEY": "sk_test_1", "STRIPE_WEBHOOK_SIGNING_SECRET": "whsec_1", "STRIPE_PUBLISHABLE_KEY": "pk_test_1", "STRIPE_UNRELATED": "x",
	}))
	want := PSPConfig{"stripe": {AccountID: "acct_1", Secrets: map[string]string{"secret_key": "sk_test_1", "webhook_signing_secret": "whsec_1"}, Settings: map[string]any{"publishable_key": "pk_test_1"}}}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("stripe = %+v, %v", got, err)
	}

	got, err = PSPFromEnv("nmi-eu", envLookup(map[string]string{
		"NMI_EU_RAIL": "nmi", "NMI_EU_ACCOUNT_ID": "123", "NMI_EU_SECURITY_KEY": "s", "NMI_EU_WEBHOOK_SIGNING_SECRET": "w", "NMI_EU_TOKENIZATION_KEY": "t", "NMI_EU_ENDPOINT_DEPLOYMENT": "gateway",
	}))
	want = PSPConfig{"nmi": {AccountID: "123", Secrets: map[string]string{"security_key": "s", "webhook_signing_secret": "w"}, Settings: map[string]any{"tokenization_key": "t", "endpoint_deployment": "gateway"}}}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("nmi = %+v, %v", got, err)
	}

	for key, env := range map[string]map[string]string{
		"nmi":    {"NMI_ACCOUNT_ID": "1", "NMI_SECURITY_KEY": "s"},
		"stripe": {"STRIPE_SECRET_KEY": "sk_test_1", "STRIPE_WEBHOOK_SIGNING_SECRET": "w"},
		"paypal": {"PAYPAL_ACCOUNT_ID": "1"},
		"solana": {"SOLANA_ACCOUNT_ID": "1"},
	} {
		if _, err := PSPFromEnv(key, envLookup(env)); err == nil || !strings.Contains(err.Error(), strings.ToUpper(key)[:3]) && !strings.Contains(err.Error(), key) {
			t.Errorf("%s: err = %v", key, err)
		}
	}
}
