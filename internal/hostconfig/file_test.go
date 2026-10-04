package hostconfig

import (
	"reflect"
	"testing"

	billing "github.com/open-rails/openrails/internal/config"
	"github.com/stretchr/testify/require"
)

// hostOnly are the engine settings a host builds in code; no file key sets them.
var hostOnly = map[string]bool{"River": true, "RiverSchema": true, "Merchant": true, "Catalog": true, "HTTP": true, "ControlPlane": true}

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
		case reflect.Int:
			field.SetInt(1)
		case reflect.Slice:
			field.Set(reflect.ValueOf([]string{"x"}))
		case reflect.Pointer:
			field.Set(reflect.New(field.Type().Elem()))
		default:
			t.Fatalf("%s: unhandled kind %s", v.Type().Field(i).Name, field.Kind())
		}
	}
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
		f.DB.URL = billing.DBConnectionString(f.DB)
		cfg, err := f.config()
		require.NoError(t, err)
		require.Equal(t, ok, Validate(cfg) == nil, value)
	}
}
