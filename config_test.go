package openrails

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/goccy/go-yaml"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/billing"
)

func TestReadMerchantFile(t *testing.T) {
	_, err := ReadMerchantFile(filepath.Join(t.TempDir(), "missing.yaml"))
	require.ErrorIs(t, err, os.ErrNotExist)

	typo := filepath.Join(t.TempDir(), "merchant.yaml")
	require.NoError(t, os.WriteFile(typo, []byte("psp:\n  mobius: {rail: nmi}\n"), 0o600))
	_, err = ReadMerchantFile(typo)
	require.ErrorContains(t, err, typo, "a refused file names itself")
}

// A declaration built in Go, written as a merchant file, reads back unchanged.
func TestMerchantFileRoundTrip(t *testing.T) {
	want := MerchantDeclaration{Sandbox: true,
		Slug: "onlydemo", DisplayName: "OnlyDemo",
		PSPs: PSPs{
			"mobius": NMIPSP{Environment: EnvironmentSandbox, GatewayID: "000000", TokenizationKey: "tk", SecurityKey: "sk", WebhookSigningSecret: "whs", CardEntry: "server"}.PSPConfig(),
			"wallet": SolanaPSP{Environment: EnvironmentDevnet, Treasury: "CXFYwCTBtzsF3qS5zZyWqMKYwBVFHUHCrtTiJkAa5c1w", RPCURL: "https://devnet.helius-rpc.com/?api-key=k"}.PSPConfig(),
		},
		MerchantSettings: billing.MerchantSettings{BillingPeriodBoundary: "calendar_month"},
	}
	raw, err := yaml.Marshal(struct {
		Slug                string `yaml:"slug"`
		MerchantDeclaration `yaml:",inline"`
	}{want.Slug, want})
	require.NoError(t, err)
	path := filepath.Join(t.TempDir(), "merchant.yaml")
	require.NoError(t, os.WriteFile(path, raw, 0o600))
	got, err := ReadMerchantFile(path)
	require.NoError(t, err)
	require.Equal(t, want, got)
}

// A host's own YAML file decodes into Config by its yaml tags.
func TestConfigLoadsFromYAML(t *testing.T) {
	path := filepath.Join(t.TempDir(), "billing.yaml")
	require.NoError(t, os.WriteFile(path, []byte(`
provider_write_mode: full
database: {schema: host_billing}
db: {url: "postgres://app@db/app", sslmode: verify-full}
vault:
  address: https://vault.example:8200
  namespace: team
  scope_prefix: openrails-dev
  auth_method: approle
  role_id: role
  secret_id: secret
  auth_mount: approle-billing
  kv_mount: kv
  transit_mount: transit-billing
read_cache:
  catalog: {refresh_after: 2m, max_stale: 30m}
  meters: {}
`), 0o600))
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	var cfg Config
	require.NoError(t, yaml.UnmarshalWithOptions(raw, &cfg, yaml.DisallowUnknownField()))
	require.Equal(t, "full", cfg.ProviderWriteMode)
	require.Equal(t, DatabaseConfig{Schema: "host_billing"}, cfg.Database)
	require.Equal(t, &DBConfig{URL: "postgres://app@db/app", SSLMode: "verify-full"}, cfg.DB)
	require.Equal(t, &VaultConfig{
		Address: "https://vault.example:8200", Namespace: "team", ScopePrefix: "openrails-dev",
		AuthMethod: "approle", RoleID: "role", SecretID: "secret", AuthMount: "approle-billing",
		KVMount: "kv", TransitMount: "transit-billing",
	}, cfg.Vault)
	require.Equal(t, ReadCache{ReadCatalog: {RefreshAfter: 2 * time.Minute, MaxStale: 30 * time.Minute}, ReadMeters: {}}, cfg.ReadCache)
}

// Every field Config holds has a yaml and a json key, so none decodes by a
// library's default spelling. Merchants are the merchant file's shape.
func TestConfigFieldsAreTagged(t *testing.T) {
	seen := map[reflect.Type]bool{}
	var walk func(path string, typ reflect.Type)
	walk = func(path string, typ reflect.Type) {
		switch typ.Kind() {
		case reflect.Pointer, reflect.Slice, reflect.Map:
			walk(path, typ.Elem())
		case reflect.Struct:
			if seen[typ] {
				return
			}
			seen[typ] = true
			for i := range typ.NumField() {
				field := typ.Field(i)
				if field.Name == "Merchants" && typ == reflect.TypeFor[Config]() {
					continue
				}
				if field.Tag.Get("yaml") == "" || field.Tag.Get("json") == "" {
					t.Errorf("%s.%s lacks a yaml or json key", path, field.Name)
				}
				walk(path+"."+field.Name, field.Type)
			}
		}
	}
	walk("Config", reflect.TypeFor[Config]())
}
