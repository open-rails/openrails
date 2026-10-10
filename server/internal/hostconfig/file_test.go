package hostconfig

import (
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/open-rails/authkit"
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
		case reflect.Map:
			field.Set(reflect.ValueOf(map[string]any{"token": map[string]any{"issuer": "https://auth.example"}}))
		case reflect.Struct:
			for j := range field.NumField() {
				switch sub := field.Field(j); sub.Kind() {
				case reflect.Bool:
					sub.SetBool(true)
				case reflect.String:
					sub.SetString("sandbox")
				case reflect.Pointer:
					sub.Set(reflect.New(sub.Type().Elem()))
				case reflect.Struct:
					sub.Field(0).SetString("finite")
				}
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
	require.Equal(t, "http://localhost:3053", cfg.Auth.Token.Issuer)
	require.True(t, cfg.Auth.Keys.AllowEphemeralDevKeys)
	require.True(t, cfg.Auth.HTTP.DirectPeerIP)
	require.Equal(t, 2160*time.Hour, *cfg.Naming.FormerNames.Duration)
	require.Equal(t, 1200, (*cfg.RateLimits)["webhook"].RequestsPerMinute)
	require.Contains(t, cfg.DB.URL, "@localhost:5434/openrails_db")
}

// auth is AuthKit's configuration, decoded by AuthKit's own keys: the
// resource, the issuance DPoP mode and trusted issuers reach it whole, and an
// AUTH_* variable sets the key it names.
func TestAuthSectionLoads(t *testing.T) {
	bootEnv(t)
	t.Setenv("AUTH_SIGN_IN_DPOP", "required")
	t.Setenv("AUTH_RESOURCE_PUBLIC_URL", "https://api.openrails.example.com")
	path := writeFile(t, filepath.Join(t.TempDir(), "config.yaml"), `
auth:
  token:
    issuer: https://openrails.example.com/auth
    access_token_duration: 10m
  resource:
    id: https://openrails.example.com
  remote_applications:
    - issuer: https://example.com/auth
      role: root:owner
      role_map: {billing-admins: root:owner}
`)
	loaded, err := Load(path)
	require.NoError(t, err)
	a := loaded.Auth
	require.Equal(t, "https://openrails.example.com", a.Resource.ID)
	require.Equal(t, "https://api.openrails.example.com", a.Resource.PublicURL, "env merges into the section")
	require.Equal(t, authkit.DPoPRequired, a.SignIn.DPoP)
	require.Equal(t, 10*time.Minute, a.Token.AccessTokenDuration)
	require.Equal(t, "https://openrails.example.com/auth", a.Token.Issuer)
	require.Len(t, a.RemoteApplications, 1)
	require.Equal(t, "root:owner", a.RemoteApplications[0].Role.String())
	require.Equal(t, "root:owner", a.RemoteApplications[0].RoleMap["billing-admins"].String())

	t.Setenv("AUTH_NOT_A_KEY", "x")
	_, err = Load(path)
	require.ErrorContains(t, err, "not_a_key", "an AUTH_ variable AuthKit does not know refuses boot")
}
