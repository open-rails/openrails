package hostconfig

import (
	"fmt"
	"slices"
	"strings"
)

// retiredAuth refuses every auth key but auth.keys.path, from the file, a
// mounted secret file or the environment (AUTH_*): OpenRails derives the
// rest of the server's AuthKit configuration.
func retiredAuth(keys []string) error {
	var found []string
	for _, key := range keys {
		rest, ok := strings.CutPrefix(key, "auth.")
		if !ok || key == "auth.keys.path" {
			continue
		}
		line := fmt.Sprintf("%s (AUTH_%s)", key, strings.ToUpper(strings.ReplaceAll(rest, ".", "_")))
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
		return "set public_url"
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
	case key == "active_key_id" || key == "active_private_key_pem" || key == "public_keys" || key == "keys_path" || section == "keys":
		return "AuthKit's keys are keys.json and totp.key under auth.keys.path (AUTH_KEYS_PATH)"
	}
	return ""
}
