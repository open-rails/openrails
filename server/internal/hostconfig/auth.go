package hostconfig

import (
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"strings"

	"github.com/open-rails/authkit"
	"github.com/open-rails/authkit/keys"

	"github.com/open-rails/openrails/server"
)

// SigningKey is AuthKit's inline signing key, read from the environment
// (AUTHKIT_ACTIVE_KEY_ID, AUTHKIT_ACTIVE_PRIVATE_KEY_PEM and
// AUTHKIT_PUBLIC_KEYS, a JSON object {kid: PEM} of further keys trusted for
// verification, such as the previous one during a rotation).
type SigningKey struct {
	// ActiveKeyID is the signing key's kid.
	ActiveKeyID string `yaml:"active_key_id"`
	// ActivePrivateKeyPEM is the signing key, PEM.
	ActivePrivateKeyPEM string `yaml:"active_private_key_pem"`
	// PublicKeysJSON is a JSON object {kid: PEM} of further keys trusted for verification.
	PublicKeysJSON string `yaml:"public_keys"`
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

// authPath is the auth section key an AUTH_* environment variable names:
// its rest after AUTH_, matched against server.AuthConfig's keys (keys_path
// is keys.path). A rest naming no key stays whole, and retiredAuth refuses
// it.
func authPath(rest string) string {
	if p := yamlPath(reflect.TypeFor[server.AuthConfig](), rest); p != "" {
		return p
	}
	return rest
}

// retiredAuth refuses every auth key outside server.AuthConfig, from the
// file, a mounted secret file or the environment: OpenRails derives the
// rest of the server's AuthKit configuration.
func retiredAuth(keys []string) error {
	var found []string
	for _, key := range keys {
		rest, ok := strings.CutPrefix(key, "auth.")
		if !ok || yamlPath(reflect.TypeFor[server.AuthConfig](), strings.ReplaceAll(rest, ".", "_")) == rest {
			continue
		}
		// An environment variable's rest is its AuthKit key, underscored.
		if p := yamlPath(reflect.TypeFor[authkit.Config](), strings.ReplaceAll(rest, ".", "_")); p != "" {
			rest = p
		}
		line := fmt.Sprintf("auth.%s (AUTH_%s)", rest, strings.ToUpper(strings.ReplaceAll(rest, ".", "_")))
		if hint := retiredAuthHint(rest); hint != "" {
			line += ": " + hint
		}
		if !slices.Contains(found, line) {
			found = append(found, line)
		}
	}
	if len(found) == 0 {
		return nil
	}
	slices.Sort(found)
	return fmt.Errorf("removed auth settings refuse boot: %s. OpenRails derives the server's AuthKit configuration; only auth.keys.path (AUTH_KEYS_PATH) is set (docs/standalone.md, The server's AuthKit)", strings.Join(found, "; "))
}

// retiredAuthHint is what replaced a former auth key, where one setting did.
func retiredAuthHint(key string) string {
	section, _, _ := strings.Cut(key, ".")
	switch {
	case key == "token.issuer" || key == "resource.id" || key == "resource.public_url" || section == "frontend" || section == "passkeys":
		return "set public_billing_base_url"
	case section == "database":
		return "AuthKit's tables live in <database.schema>_auth"
	case section == "http":
		return "set trusted_proxies"
	case section == "remote_applications":
		return "declare root's trusted applications in bootstrap.yaml"
	case section == "two_factor":
		return "every staff account needs a passkey or an authenticator app"
	case key == "token.refresh_token_duration":
		return "staff sessions last 30 days"
	case key == "active_key_id" || key == "active_private_key_pem" || key == "public_keys":
		return "the inline signing key is AUTHKIT_ACTIVE_KEY_ID, AUTHKIT_ACTIVE_PRIVATE_KEY_PEM and AUTHKIT_PUBLIC_KEYS"
	case key == "keys_path":
		return "set auth.keys.path (AUTH_KEYS_PATH)"
	}
	return ""
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
