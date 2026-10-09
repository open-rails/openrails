package openrails

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/custodians"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/modules/payments/rails"
)

type typedPSP interface{ PSPConfig() PSPConfig }

var typedPSPs = map[models.Rail]reflect.Type{
	models.RailNMI:    reflect.TypeFor[NMIPSP](),
	models.RailStripe: reflect.TypeFor[StripePSP](),
	models.RailCCBill: reflect.TypeFor[CCBillPSP](),
	models.RailSolana: reflect.TypeFor[SolanaPSP](),
}

// Each typed declaration covers exactly its rail's registry slots, required
// where the registry says so, and every field lands on the key its tag names.
// A slot added to the registry fails here until its struct gains the field.
func TestTypedPSPsCoverTheRailRegistry(t *testing.T) {
	for _, d := range rails.All() {
		typ, ok := typedPSPs[d.Rail]
		require.Equal(t, d.HasPSPs, ok, "rail %s: a typed declaration iff it takes PSPs", d.Rail)
		if !ok {
			continue
		}
		wantSecrets := map[string]bool{}
		for _, k := range d.CredentialKeys {
			wantSecrets[k.Name] = k.Required
		}
		custodial := false
		for _, kind := range custodians.Kinds() {
			c, _ := custodians.Get(kind)
			custodial = custodial || c.SupportsRail(d.Rail)
		}

		gotSecrets, gotSettings := map[string]bool{}, []string{}
		hasCustodian := false
		value := reflect.New(typ).Elem()
		want := PSPConfig{Rail: billing.Rail(d.Rail)}
		for i := range typ.NumField() {
			field := typ.Field(i)
			tag, ok := field.Tag.Lookup("psp")
			require.True(t, ok, "%s.%s has no psp tag", typ.Name(), field.Name)
			key, flag, _ := strings.Cut(tag, ",")
			require.Contains(t, []string{"", "required"}, flag, "%s.%s", typ.Name(), field.Name)
			sample := sampleFor(t, value.Field(i), field.Name)
			section, name, _ := strings.Cut(key, ".")
			switch section {
			case "secrets":
				require.NotContains(t, gotSecrets, name, "%s.%s", typ.Name(), field.Name)
				gotSecrets[name] = flag == "required"
				want.Secrets = put(want.Secrets, name, sample.(string))
			case "settings":
				require.Empty(t, flag, "settings are never required: %s.%s", typ.Name(), field.Name)
				gotSettings = append(gotSettings, name)
				want.Settings = put(want.Settings, name, sample)
			case "account_id":
				want.AccountID = sample.(string)
			case "archived":
				want.Archived = true
			case "custodian":
				hasCustodian = true
				want.Custodian = sample.(string)
			case "signer":
				require.Equal(t, "key", name)
				want.Signer = &PSPSignerConfig{Mode: "vault_transit", Key: sample.(string)}
			default:
				t.Fatalf("%s.%s: unknown psp tag %q", typ.Name(), field.Name, tag)
			}
		}
		require.Equal(t, wantSecrets, gotSecrets, "%s secrets (name: required)", typ.Name())
		require.ElementsMatch(t, d.SettingKeys, gotSettings, "%s settings", typ.Name())
		require.Equal(t, custodial, hasCustodian, "%s: a Custodian field iff a custodian charges through %s", typ.Name(), d.Rail)

		require.Equal(t, want, value.Interface().(typedPSP).PSPConfig(), "%s fills the keys its tags name", typ.Name())
		require.Equal(t, PSPConfig{Rail: billing.Rail(d.Rail)}, reflect.Zero(typ).Interface().(typedPSP).PSPConfig(), "%s: an empty field is an omitted key", typ.Name())
	}
}

// sampleFor sets a distinct value on field and returns it as the declaration
// holds it.
func sampleFor(t *testing.T, field reflect.Value, name string) any {
	switch v := field.Addr().Interface().(type) {
	case *string:
		*v = "sample-" + name
		return *v
	case *bool:
		*v = true
		return true
	case *time.Time:
		*v = time.Date(2026, 10, 8, 12, 0, 0, 0, time.FixedZone("x", 3600))
		return "2026-10-08T11:00:00Z"
	case *map[string]SolanaToken:
		*v = map[string]SolanaToken{"SOL": {}, "XYZ": {Mint: "mint-" + name, Name: "Xyz"}}
		return map[string]any{"SOL": map[string]any{}, "XYZ": map[string]any{"mint": "mint-" + name, "name": "Xyz"}}
	}
	t.Fatalf("%s: no sample for %s", name, field.Type())
	return nil
}

func put[V any](m map[string]V, key string, value V) map[string]V {
	if m == nil {
		m = map[string]V{}
	}
	m[key] = value
	return m
}

// The typed form and the YAML form are one declaration.
func TestTypedPSPsMatchTheirYAML(t *testing.T) {
	m, err := ParseMerchantDeclaration([]byte(`
slug: onlydemo
psps:
  mobius:
    rail: nmi
    account_id: "000000"
    settings: {tokenization_key: tk, webhook_overlap_expires_at: "2026-10-08T00:00:00Z"}
    secrets: {security_key: sk, webhook_signing_secret: whs, webhook_signing_secret_previous: old}
  stripe: {rail: stripe, account_id: acct_1, archived: true, secrets: {secret_key: sk_test_1, webhook_signing_secret: whsec_1}}
  ccbill: {rail: ccbill, account_id: 999999-0000, secrets: {salt: s}}
  wallet:
    rail: solana
    signer: {mode: vault_transit, key: transit-key}
    settings: {rpc_provider: public, tokens: {SOL: {}, XYZ: {mint: m}}}
`))
	require.NoError(t, err)
	require.Equal(t, map[string]PSPConfig{
		"mobius": NMIPSP{
			AccountID: "000000", TokenizationKey: "tk",
			SecurityKey: "sk", WebhookSigningSecret: "whs",
			WebhookSigningSecretPrevious: "old", WebhookOverlapExpiresAt: time.Date(2026, 10, 8, 0, 0, 0, 0, time.UTC),
		}.PSPConfig(),
		"stripe": StripePSP{AccountID: "acct_1", Archived: true, SecretKey: "sk_test_1", WebhookSigningSecret: "whsec_1"}.PSPConfig(),
		"ccbill": CCBillPSP{AccountID: "999999-0000", Salt: "s"}.PSPConfig(),
		"wallet": SolanaPSP{TransitKey: "transit-key", RPCProvider: "public", Tokens: map[string]SolanaToken{"SOL": {}, "XYZ": {Mint: "m"}}}.PSPConfig(),
	}, m.PSPs)
}
