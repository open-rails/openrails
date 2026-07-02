// Package merchants implements merchant provisioning, lifecycle, per-merchant
// rail credentials, and webhook routing for OpenRails' merchant platform
// (issue #225). It builds on the #223 merchant primitive (pkg/merchant +
// openrails.merchants)
// and the #224 in-process AuthKit control plane (internal/controlplane): the
// lifecycle service records merchant permission-group ids through control-plane
// core calls and records merchant directory state directly in openrails.*
// (OpenRails-owned control-plane state).
package merchants

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"path"
	"strings"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/modules/payments/rails"
	"github.com/open-rails/openrails/pkg/merchant"
)

// Canonical per-merchant secret names. The store namespaces values by
// (merchant id, name); these are the well-known names OpenRails reads at request
// time. A managed (Vault-backed) deployment keeps the SAME names but resolves
// them to a merchant-scoped Vault path.
const (
	// SecretStripeSecretKey is the merchant's Stripe API secret key (BYO or Connect).
	SecretStripeSecretKey = "stripe/secret_key"
	// SecretStripeWebhookSigning is the merchant's Stripe webhook signing secret,
	// used to verify inbound webhooks AFTER merchant resolution.
	SecretStripeWebhookSigning = "stripe/webhook_signing_secret"
	// SecretStripeWebhookSigningThin is the merchant's Stripe "thin" Event
	// Destination signing secret (a single endpoint may receive both).
	SecretStripeWebhookSigningThin = "stripe/webhook_signing_secret_thin"
	// SecretNMISecurityKey is the legacy broad NMI security-key secret.
	SecretNMISecurityKey = "nmi/mobius/security_key"
	// SecretNMITokenizationURL overrides the Collect.js script URL for a
	// merchant/provider. Most NMI accounts use DefaultNMICollectJSURL.
	SecretNMITokenizationURL = "nmi/mobius/tokenization_url"
	// SecretNMIWebhookSigning is the merchant's NMI webhook signing
	// secret, used to verify inbound webhooks after merchant/provider resolution.
	SecretNMIWebhookSigning = "nmi/mobius/webhook_signing_secret"
)

// SecretDefinition describes one OpenRails-owned merchant secret. It is the
// canonical registry used by status APIs, docs, validation, and runbooks.
type SecretDefinition struct {
	Name              string `json:"name"`
	Rail              string `json:"rail"`
	Purpose           string `json:"purpose"`
	DisplayLabel      string `json:"display_label"`
	ManualVault       bool   `json:"manual_vault"`
	MerchantWritable  bool   `json:"merchant_writable"`
	Validation        string `json:"validation"`
	PlaintextReadable bool   `json:"plaintext_readable"`
}

var merchantSecretRegistry = []SecretDefinition{
	{Name: SecretStripeSecretKey, Rail: "stripe", Purpose: "api_key", DisplayLabel: "Stripe secret key", ManualVault: true, MerchantWritable: true, Validation: "stripe_balance_check"},
	{Name: SecretStripeWebhookSigning, Rail: "stripe", Purpose: "webhook_signing", DisplayLabel: "Stripe webhook signing secret", ManualVault: true, MerchantWritable: true, Validation: "format"},
	{Name: SecretStripeWebhookSigningThin, Rail: "stripe", Purpose: "webhook_signing", DisplayLabel: "Stripe thin event signing secret", ManualVault: true, MerchantWritable: true, Validation: "format"},
	{Name: SecretNMISecurityKey, Rail: "nmi", Purpose: "security_key", DisplayLabel: "NMI security key", ManualVault: true, MerchantWritable: true, Validation: "presence"},
	{Name: SecretNMITokenizationURL, Rail: "nmi", Purpose: "tokenization_url", DisplayLabel: "NMI Collect.js URL", ManualVault: true, MerchantWritable: true, Validation: "url", PlaintextReadable: true},
	{Name: SecretNMIWebhookSigning, Rail: "nmi", Purpose: "webhook_signing", DisplayLabel: "NMI webhook signing secret", ManualVault: true, MerchantWritable: true, Validation: "presence"},
}

// RailMerchantAccountSecretName returns the canonical secret-store name for a
// provider-account-owned credential. The merchant id still namespaces the store;
// this path adds provider identity so one merchant can rotate or run multiple
// accounts of the same provider without credential collisions.
func RailMerchantAccountSecretName(rail, environment, accountID, key string) (string, error) {
	rail = normalizeProviderSecretType(rail)
	environment = normalizeProviderSecretEnvironment(environment)
	accountID = strings.TrimSpace(accountID)
	key, err := NormalizeRailMerchantAccountSecretKey(rail, key)
	if err != nil {
		return "", err
	}
	if rail == "" {
		return "", fmt.Errorf("provider account secret requires rail")
	}
	if environment == "" {
		return "", fmt.Errorf("provider account secret environment must be live or test")
	}
	if accountID == "" {
		return "", fmt.Errorf("provider account secret requires account id")
	}
	// #683: the prefix is part of the DURABLE canonical secret-name shape
	// (persisted in the merchant-secret store, including HashiCorp Vault KV
	// paths). Renaming it again means migrating stored names in EVERY
	// deployment (SQL for DB-backed, a KV move for Vault-backed) — treat it
	// with stripeapi.APIVersion discipline: never float it casually.
	return path.Join("rail_merchant_accounts", rail, environment, url.PathEscape(accountID), key), nil
}

// ParseRailMerchantAccountSecretName parses a provider-account-scoped secret name.
func ParseRailMerchantAccountSecretName(name string) (rail, environment, accountID, key string, ok bool, err error) {
	name = cleanSecretName(name)
	parts := strings.Split(name, "/")
	if len(parts) != 5 || parts[0] != "rail_merchant_accounts" {
		return "", "", "", "", false, nil
	}
	rail = normalizeProviderSecretType(parts[1])
	environment = normalizeProviderSecretEnvironment(parts[2])
	accountID, err = url.PathUnescape(parts[3])
	if err != nil {
		return "", "", "", "", true, fmt.Errorf("invalid provider account id escape: %w", err)
	}
	key, err = NormalizeRailMerchantAccountSecretKey(rail, parts[4])
	if err != nil {
		return "", "", "", "", true, err
	}
	if rail == "" || environment == "" || strings.TrimSpace(accountID) == "" {
		return "", "", "", "", true, fmt.Errorf("invalid provider account secret name %q", name)
	}
	return rail, environment, accountID, key, true, nil
}

// SecretWritable reports whether a merchant operator may write the secret name.
func SecretWritable(name string) bool {
	name = cleanSecretName(name)
	if def, ok := SecretDefinitionFor(name); ok {
		return def.MerchantWritable
	}
	rail, _, _, key, ok, err := ParseRailMerchantAccountSecretName(name)
	if !ok || err != nil {
		return false
	}
	// Registry-backed (#669): operator-only slots (solana private_key) are not
	// merchant-writable.
	k, known := rails.CredentialKeyFor(models.Rail(rail), key)
	return known && k.MerchantWritable
}

// NormalizeRailMerchantAccountSecretKey canonicalizes manifest/admin secret keys for
// a provider account against the rail's registry-declared slots (#669). It
// deliberately returns key fragments, not legacy broad merchant secret names.
func NormalizeRailMerchantAccountSecretKey(rail, key string) (string, error) {
	rail = normalizeProviderSecretType(rail)
	key = strings.ToLower(strings.TrimSpace(key))
	if k, ok := rails.CredentialKeyFor(models.Rail(rail), key); ok {
		return k.Name, nil
	}
	return "", fmt.Errorf("unknown provider account secret %s.%s", rail, key)
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

// SecretDefinitionFor returns a registry entry by stable secret name.
func SecretDefinitionFor(name string) (SecretDefinition, bool) {
	name = cleanSecretName(name)
	for _, d := range merchantSecretRegistry {
		if d.Name == name {
			return d, true
		}
	}
	return SecretDefinition{}, false
}

// MerchantSecretStatus is the read-only dashboard/admin view of a merchant secret.
// It never contains plaintext.
type MerchantSecretStatus struct {
	SecretDefinition
	Configured bool `json:"configured"`
	Version    int  `json:"version,omitempty"`
}

// ErrSecretNotFound is returned by a MerchantSecretStore when no secret exists for
// the (merchant, name) pair. This is a TERMINAL condition for that (merchant, name):
// the secret is genuinely absent (merchant never configured it, or was
// deprovisioned). Callers distinguish "not configured" from a backend error with
// errors.Is.
var ErrSecretNotFound = errors.New("merchants: merchant secret not found")

// ErrSecretBackendUnavailable wraps any OPERATIONAL failure of the underlying
// secret backend — Vault unreachable / sealed / permission-denied, a DB query
// error, or a not-yet-wired Vault client. It is distinct from ErrSecretNotFound
// (terminal absence) precisely so money-path callers can fail closed correctly:
//
//   - secret ABSENT (ErrSecretNotFound)      -> terminal for that merchant; never
//     retry, and (critically) NEVER treat as "verification disabled".
//   - backend UNAVAILABLE (this error)       -> RETRY; do NOT cancel a
//     subscription, delete a merchant, or skip a webhook signature check.
//
// The recurring Solana pull worker and the webhook signature verifier both branch
// on errors.Is(err, ErrSecretBackendUnavailable) to retry rather than treat a
// transient Vault outage as "no secret".
var ErrSecretBackendUnavailable = errors.New("merchants: merchant secret backend unavailable")

// Secret is a stored per-merchant secret value plus its version (bumped on every
// rotation), so callers can audit/rotate without re-reading the plaintext.
type Secret struct {
	Name    string
	Value   string
	Version int
}

// MerchantSecretReader is the read-only half of the per-merchant secret store.
// Checkout, vaulting, and workers use this so fixed-credential runtimes do not
// need write access merely to resolve credentials.
type MerchantSecretReader interface {
	Get(ctx context.Context, merchantID merchant.ID, name string) (Secret, error)
}

// RailMerchantAccountSecretResolver resolves the canonical secret name for the
// active provider account a merchant should use.
type RailMerchantAccountSecretResolver interface {
	ActiveRailMerchantAccountSecretName(ctx context.Context, merchantID merchant.ID, rail, environment, key string) (string, bool, error)
}

// RailMerchantAccountScope is the configured provider account selected for a
// merchant rail/environment.
type RailMerchantAccountScope struct {
	ID          uuid.UUID
	Rail        string
	Environment string
	AccountID   string
	Settings    map[string]any
}

// RailMerchantAccountScopeResolver resolves the selected provider account without
// requiring a particular secret key.
type RailMerchantAccountScopeResolver interface {
	ActiveRailMerchantAccountScope(ctx context.Context, merchantID merchant.ID, rail, environment string) (RailMerchantAccountScope, bool, error)
}

// MerchantSecretStore is the per-merchant secrets abstraction (issue #225). Every
// operation is namespaced by merchant id so one merchant can never read or
// overwrite another merchant's Stripe credentials or webhook signing secrets.
//
// Two implementations ship:
//
//   - dbSecretStore / memSecretStore: build and run WITHOUT a live Vault (the
//     dev / self-hosted default). DB-backed persists to openrails.merchant_secrets.
//   - vaultSecretStore: a documented adapter that resolves the SAME (merchant,
//     name) addressing to a merchant-scoped Vault KV path. It is a stub today and
//     is wired in managed deployments without any schema or caller change.
type MerchantSecretStore interface {
	MerchantSecretReader
	// Get returns the secret for (merchant, name), or ErrSecretNotFound.
	// Put creates or rotates the secret for (merchant, name). It is idempotent on
	// value: putting the same value twice is a no-op rotation. Returns the stored
	// secret (with its new version).
	Put(ctx context.Context, merchantID merchant.ID, name, value string) (Secret, error)
	// Delete removes the secret for (merchant, name). Deleting a missing secret is
	// a no-op (idempotent), so it is safe in merchant-delete purge.
	Delete(ctx context.Context, merchantID merchant.ID, name string) error
	// List enumerates the secret NAMES (never values) held for a merchant. Used by
	// the export path for Vault-side secret enumeration (GDPR / portability).
	List(ctx context.Context, merchantID merchant.ID) ([]string, error)
}

// validateSecretRef guards the (merchant, name) addressing shared by every store
// so a blank/zero merchant or empty name can never read or clobber a secret.
func validateSecretRef(merchantID merchant.ID, name string) error {
	if merchantID.IsZero() {
		return fmt.Errorf("merchants: secret access requires a merchant id")
	}
	if cleanSecretName(name) == "" {
		return fmt.Errorf("merchants: secret access requires a name")
	}
	return nil
}

func cleanSecretName(name string) string {
	return strings.Trim(strings.TrimSpace(name), "/")
}
