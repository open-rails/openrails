package hostconfig

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"

	"github.com/go-viper/mapstructure/v2"
	"github.com/open-rails/authkit"
	"github.com/open-rails/authkit/keys"
)

// SigningKey is AuthKit's inline signing key, read from the environment
// (AUTHKIT_ACTIVE_KEY_ID, AUTHKIT_ACTIVE_PRIVATE_KEY_PEM and
// AUTHKIT_PUBLIC_KEYS, a JSON object {kid: PEM} of further keys trusted for
// verification, such as the previous one during a rotation).
type SigningKey struct {
	ActiveKeyID         string `koanf:"active_key_id"`
	ActivePrivateKeyPEM string `koanf:"active_private_key_pem"`
	PublicKeysJSON      string `koanf:"public_keys"`
}

// Source is the key as AuthKit's Deps.KeySource; nil without one, when
// AuthKit reads auth.keys.path. An inline key is frozen for the process:
// keys.path rotates without a restart.
func (k SigningKey) Source() (keys.Source, error) {
	id, pem := strings.TrimSpace(k.ActiveKeyID), strings.TrimSpace(k.ActivePrivateKeyPEM)
	if id == "" && pem == "" {
		return nil, nil
	}
	var public map[string]string
	if raw := strings.TrimSpace(k.PublicKeysJSON); raw != "" {
		if err := json.Unmarshal([]byte(raw), &public); err != nil {
			return nil, fmt.Errorf("AUTHKIT_PUBLIC_KEYS is not a JSON object of PEM keys: %w", err)
		}
	}
	src, err := keys.StaticFromPEM(id, pem, public)
	if err != nil {
		return nil, fmt.Errorf("AUTHKIT_ACTIVE_KEY_ID / AUTHKIT_ACTIVE_PRIVATE_KEY_PEM: %w", err)
	}
	return src, nil
}

// authConfig decodes the auth section by AuthKit's own keys: AuthKit
// documents them, and a key it does not know refuses boot.
func authConfig(section map[string]any) (authkit.Config, error) {
	var out authkit.Config
	dec, err := mapstructure.NewDecoder(&mapstructure.DecoderConfig{
		DecodeHook: mapstructure.ComposeDecodeHookFunc(
			mapstructure.StringToTimeDurationHookFunc(),
			mapstructure.StringToSliceHookFunc(","),
			mapstructure.TextUnmarshallerHookFunc(),
		),
		Result: &out, WeaklyTypedInput: true, ErrorUnused: true, TagName: "yaml",
	})
	if err != nil {
		return authkit.Config{}, err
	}
	if err := dec.Decode(section); err != nil {
		return authkit.Config{}, fmt.Errorf("auth (AuthKit's configuration): %w", err)
	}
	return out, nil
}

// authPath is the auth section key an AUTH_* environment variable names:
// its rest after AUTH_, matched against AuthKit's keys (sign_in_dpop is
// sign_in.dpop). A rest naming no key stays whole, so boot refuses it.
func authPath(rest string) string {
	if p := yamlPath(reflect.TypeOf(authkit.Config{}), rest); p != "" {
		return p
	}
	return rest
}

func yamlPath(t reflect.Type, rest string) string {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t.Kind() != reflect.Struct {
		return ""
	}
	for i := 0; i < t.NumField(); i++ {
		name, _, _ := strings.Cut(t.Field(i).Tag.Get("yaml"), ",")
		if name == "" || name == "-" {
			continue
		}
		if rest == name {
			return name
		}
		if sub, ok := strings.CutPrefix(rest, name+"_"); ok {
			if p := yamlPath(t.Field(i).Type, sub); p != "" {
				return name + "." + p
			}
		}
	}
	return ""
}
