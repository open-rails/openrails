// Package merchants implements merchant provisioning, lifecycle, PSPs and
// custodians, and webhook routing. A merchant's identity is a Postgres row; its
// configuration, credentials included, is its documents in a file or Vault
// (internal/merchantdocs), which this package joins with the identities history
// points at.
package merchants

import (
	"fmt"
	"net/url"
	"path"
	"strings"

	"github.com/open-rails/openrails/internal/custodians"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/modules/payments/rails"
	"github.com/open-rails/openrails/internal/shared/apperr"
)

// PSPSecretName names one credential slot of a PSP:
// psps/<rail>/<environment>/<account_id>/<key>. It is an address resolved
// against the PSP document whose identity it names, never a storage path.
func PSPSecretName(rail, environment, accountID, key string) (string, error) {
	rail = normalizeProviderSecretType(rail)
	environment = normalizeProviderSecretEnvironment(environment)
	accountID = strings.TrimSpace(accountID)
	key, err := NormalizePSPSecretKey(rail, key)
	if err != nil {
		return "", err
	}
	if rail == "" {
		return "", apperr.Invalidf("PSP secret requires rail")
	}
	if environment == "" {
		return "", apperr.Invalidf("PSP secret environment must be live or test")
	}
	if accountID == "" {
		return "", apperr.Invalidf("PSP secret requires account id")
	}
	return path.Join("psps", rail, environment, url.PathEscape(accountID), key), nil
}

// CustodianSecretName is the custody sibling of PSPSecretName (or#880):
// custodians/<kind>/<environment>/<account_id>/<key>.
func CustodianSecretName(kind, environment, accountID, key string) (string, error) {
	d, err := custodians.Require(kind)
	if err != nil {
		return "", err
	}
	environment = normalizeProviderSecretEnvironment(environment)
	accountID = strings.TrimSpace(accountID)
	slotName := strings.ToLower(strings.TrimSpace(key))
	if _, ok := d.Secret(slotName); !ok {
		return "", fmt.Errorf("unknown custodian secret %s.%s", d.Kind, key)
	}
	if environment == "" {
		return "", fmt.Errorf("custodian secret environment must be live or test")
	}
	if accountID == "" {
		return "", fmt.Errorf("custodian secret requires account id")
	}
	return path.Join("custodians", d.Kind, environment, url.PathEscape(accountID), slotName), nil
}

// ParseCustodianSecretName parses a custodian-scoped secret name.
func ParseCustodianSecretName(name string) (kind, environment, accountID, key string, ok bool, err error) {
	name = cleanSecretName(name)
	parts := strings.Split(name, "/")
	if len(parts) != 5 || parts[0] != "custodians" {
		return "", "", "", "", false, nil
	}
	d, derr := custodians.Require(parts[1])
	if derr != nil {
		return "", "", "", "", true, derr
	}
	environment = normalizeProviderSecretEnvironment(parts[2])
	accountID, err = url.PathUnescape(parts[3])
	if err != nil {
		return "", "", "", "", true, fmt.Errorf("invalid custodian account id escape: %w", err)
	}
	key = strings.ToLower(strings.TrimSpace(parts[4]))
	if _, known := d.Secret(key); !known {
		return "", "", "", "", true, fmt.Errorf("unknown custodian secret %s.%s", d.Kind, parts[4])
	}
	if environment == "" || strings.TrimSpace(accountID) == "" {
		return "", "", "", "", true, fmt.Errorf("invalid custodian secret name %q", name)
	}
	return d.Kind, environment, accountID, key, true, nil
}

// ParsePSPSecretName parses a PSP-scoped secret name.
func ParsePSPSecretName(name string) (rail, environment, accountID, key string, ok bool, err error) {
	name = cleanSecretName(name)
	parts := strings.Split(name, "/")
	if len(parts) != 5 || parts[0] != "psps" {
		return "", "", "", "", false, nil
	}
	rail = normalizeProviderSecretType(parts[1])
	environment = normalizeProviderSecretEnvironment(parts[2])
	accountID, err = url.PathUnescape(parts[3])
	if err != nil {
		return "", "", "", "", true, fmt.Errorf("invalid PSP id escape: %w", err)
	}
	key, err = NormalizePSPSecretKey(rail, parts[4])
	if err != nil {
		return "", "", "", "", true, err
	}
	if rail == "" || environment == "" || strings.TrimSpace(accountID) == "" {
		return "", "", "", "", true, fmt.Errorf("invalid PSP secret name %q", name)
	}
	return rail, environment, accountID, key, true, nil
}

// SecretWritable reports whether a merchant operator may write the secret name.
// Only PSP-scoped and custodian-scoped names qualify (#884/or#880): a retired
// flat name parses as unscoped and is refused.
func SecretWritable(name string) bool {
	if kind, _, _, key, ok, err := ParseCustodianSecretName(name); ok {
		if err != nil {
			return false
		}
		d, derr := custodians.Require(kind)
		if derr != nil {
			return false
		}
		slot, known := d.Secret(key)
		return known && slot.MerchantWritable
	}
	rail, _, _, key, ok, err := ParsePSPSecretName(name)
	if !ok || err != nil {
		return false
	}
	// Registry-backed (#669): operator-only slots (solana private_key) are not
	// merchant-writable.
	k, known := rails.CredentialKeyFor(models.Rail(rail), key)
	return known && k.MerchantWritable
}

// NormalizePSPSecretKey canonicalizes manifest/admin secret keys for
// a PSP against the rail's registry-declared slots (#669). It
// deliberately returns key fragments, not legacy broad merchant secret names.
func NormalizePSPSecretKey(rail, key string) (string, error) {
	rail = normalizeProviderSecretType(rail)
	key = strings.ToLower(strings.TrimSpace(key))
	if k, ok := rails.CredentialKeyFor(models.Rail(rail), key); ok {
		return k.Name, nil
	}
	return "", apperr.Invalidf("unknown PSP secret %s.%s", rail, key)
}

func normalizeProviderSecretType(rail string) string {
	return strings.ToLower(strings.TrimSpace(rail))
}

func normalizeProviderSecretEnvironment(environment string) string {
	// Empty is NOT defaulted (#681): the caller must know its environment
	// (deployment posture) — a silent live default hid sandbox lookups.
	switch strings.ToLower(strings.TrimSpace(environment)) {
	case "live", "prod", "production", "mainnet":
		return "live"
	case "test", "sandbox", "devnet", "testnet":
		return "test"
	default:
		return ""
	}
}
