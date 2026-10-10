package hostconfig

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	billing "github.com/open-rails/openrails/internal/config"
	"github.com/stretchr/testify/require"
)

// hostOnly are the engine settings a host builds in code; no file key sets them.
var hostOnly = map[string]bool{"Merchant": true, "Catalog": true, "Checkout": true}

// Every file key reaches the server's configuration, and every engine setting
// is either a file key or deliberately host-only.
func TestFileReachesConfig(t *testing.T) {
	f := &fileConfig{}
	v := reflect.ValueOf(f).Elem()
	for i := range v.NumField() {
		field := v.Field(i)
		switch field.Kind() {
		case reflect.String:
			field.SetString("sandbox")
		case reflect.Bool:
			field.SetBool(true)
		case reflect.Int, reflect.Int64:
			field.SetInt(1)
		case reflect.Slice:
			field.Set(reflect.ValueOf([]string{"x"}))
		case reflect.Pointer:
			field.Set(reflect.New(field.Type().Elem()))
		case reflect.Struct:
			for j := range field.NumField() {
				field.Field(j).SetString("sandbox")
			}
		default:
			t.Fatalf("%s: unhandled kind %s", v.Type().Field(i).Name, field.Kind())
		}
	}
	f.AdminConsole.Enabled = true
	f.EmailSMTP.Host = "smtp.example"
	f.Redis.Addr = "redis:6379"
	f.AdminConsole.Issuer = &ConsoleIssuer{}
	cfg, err := f.config()
	require.NoError(t, err)
	for _, v := range []reflect.Value{reflect.ValueOf(cfg).Elem(), reflect.ValueOf(cfg.Config).Elem()} {
		for i := range v.NumField() {
			name := v.Type().Field(i).Name
			require.Equalf(t, hostOnly[name] && v.Type() == reflect.TypeFor[billing.Config](), v.Field(i).IsZero(), "%s.%s", v.Type(), name)
		}
	}
}

func TestPort(t *testing.T) {
	for raw, want := range map[string]port{"44553": 44553, "65535": 65535, " 3053 ": 3053, "": 0} {
		var got port
		require.NoError(t, got.UnmarshalText([]byte(raw)))
		require.Equal(t, want, got)
	}
	for _, raw := range []string{"65536", "0", "-1", "not-a-port"} {
		var got port
		require.Error(t, got.UnmarshalText([]byte(raw)), raw)
	}
	// A number in config.yaml skips UnmarshalText.
	for value, ok := range map[int]bool{44553: true, 70000: false, -20983: false} {
		f := defaults()
		f.TestMode, f.ProviderWriteMode, f.Port = "sandbox", "full", port(value)
		f.DB.URL = testDatabaseURL
		cfg, err := f.config()
		require.NoError(t, err)
		require.Equal(t, ok, Validate(cfg) == nil, value)
	}
}

// config.example.yaml is the documented file: every key in it must still load.
func TestConfigExampleLoads(t *testing.T) {
	example, err := filepath.Abs(filepath.Join("..", "..", "..", "config.example.yaml"))
	require.NoError(t, err)
	bootEnv(t)
	unsetenv(t, "DB_URL")
	unsetenv(t, "TEST_MODE")
	unsetenv(t, "PROVIDER_WRITE_MODE")
	cfg, err := Load(example)
	require.NoError(t, err)
	require.Equal(t, "0.0.0.0", cfg.Host)
	require.Equal(t, 3053, cfg.Port)
	require.Equal(t, billing.CredentialPostureSandbox, cfg.TestMode)
	require.Equal(t, billing.ProviderWriteModeFull, cfg.ProviderWriteMode)
	require.Equal(t, "http://localhost:3053", cfg.Auth.Issuer)
	require.True(t, cfg.Auth.AllowEphemeralSigningKey)
	require.Equal(t, 2160*time.Hour, *cfg.Auth.Naming.FormerNames.Duration)
	require.Equal(t, 1200, (*cfg.RateLimits)["webhook"].RequestsPerMinute)
	require.Contains(t, cfg.DB.URL, "@localhost:5434/openrails_db")
}

// resource_server reaches the control plane's trusted issuers, keys and all.
func TestResourceServerLoads(t *testing.T) {
	example, err := os.ReadFile(filepath.Join("..", "..", "..", "config.example.yaml"))
	require.NoError(t, err)
	bootEnv(t)
	path := filepath.Join(t.TempDir(), "config.yaml")
	require.NoError(t, os.WriteFile(path, append(example, []byte(`
resource_server:
  identifier: https://openrails.example.com
  dpop_nonce_key: 0123456789abcdef0123456789abcdef
  trusted_issuers:
    - name: example
      issuer: https://example.com/auth
      merchants: [example]
      permissions: ["merchant:*"]
      allowed_origins: [https://admin.example.com]
      group_roles: {billing-admins: owner}
      keys:
        - kid: k1
          jwk: {kty: EC, crv: P-256, x: abc, y: def}
`)...), 0o600))
	cfg, err := Load(path)
	require.NoError(t, err)
	rs := cfg.ResourceServer
	require.NotNil(t, rs)
	require.Equal(t, "https://openrails.example.com", rs.Identifier)
	require.Len(t, rs.TrustedIssuers, 1)
	is := rs.TrustedIssuers[0]
	require.Equal(t, "https://example.com/auth", is.Issuer)
	require.Equal(t, []string{"example"}, is.Merchants)
	require.Equal(t, []string{"merchant:*"}, is.Permissions)
	require.Equal(t, []string{"https://admin.example.com"}, is.AllowedOrigins)
	require.Equal(t, map[string]string{"billing-admins": "owner"}, is.GroupRoles)
	require.Equal(t, "k1", is.Keys[0].KID)
	require.Equal(t, "P-256", is.Keys[0].JWK.Crv)
	require.NoError(t, Validate(cfg))

	rs.DPoPNonceKey = "short"
	require.ErrorContains(t, Validate(cfg), "dpop_nonce_key")
}
