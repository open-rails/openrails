// Package merchants implements merchant provisioning, lifecycle, per-merchant
// processor credentials, and webhook routing for OpenRails' merchant platform
// (issue #225). It builds on the #223 merchant primitive (pkg/merchant +
// openrails.merchants)
// and the #224 in-process AuthKit control plane (internal/controlplane): the
// lifecycle service mints/links owner orgs and service tokens through control-plane core
// calls and records merchant directory state directly in openrails.* (OpenRails-owned
// control-plane state).
package merchants

import (
	"context"
	"errors"
	"fmt"
	"strings"

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
	// SecretNMIMobiusProductionKey is the merchant's Mobius/NMI production key.
	SecretNMIMobiusProductionKey = "nmi/mobius/production_key"
	// SecretNMIMobiusTokenizationKey is the merchant's public Collect.js key.
	// It is client-side configuration, but it is still merchant-scoped provider
	// configuration and belongs with the merchant's NMI account setup.
	SecretNMIMobiusTokenizationKey = "nmi/mobius/tokenization_key"
	// SecretNMIMobiusTokenizationURL overrides the Collect.js script URL for a
	// merchant/provider. Most NMI accounts use DefaultNMICollectJSURL.
	SecretNMIMobiusTokenizationURL = "nmi/mobius/tokenization_url"
	// SecretCCBillAccountConfig is the merchant's CCBill account/config payload.
	// Store as an OpenRails-owned JSON string until the CCBill adapter grows a
	// typed multi-field secret.
	SecretCCBillAccountConfig = "ccbill/account_config"
	// SecretSolanaPrivateKey is the merchant's Solana signing keypair. This is
	// OpenRails-owned and intentionally not merchant-writable through self-service
	// APIs.
	SecretSolanaPrivateKey = "solana/private_key"
)

// SecretDefinition describes one OpenRails-owned merchant secret. It is the
// canonical registry used by status APIs, docs, validation, and runbooks.
type SecretDefinition struct {
	Name              string `json:"name"`
	Provider          string `json:"provider"`
	Purpose           string `json:"purpose"`
	DisplayLabel      string `json:"display_label"`
	ManualVault       bool   `json:"manual_vault"`
	MerchantWritable  bool   `json:"merchant_writable"`
	Validation        string `json:"validation"`
	PlaintextReadable bool   `json:"plaintext_readable"`
}

var merchantSecretRegistry = []SecretDefinition{
	{Name: SecretStripeSecretKey, Provider: "stripe", Purpose: "api_key", DisplayLabel: "Stripe secret key", ManualVault: true, MerchantWritable: true, Validation: "stripe_balance_check"},
	{Name: SecretStripeWebhookSigning, Provider: "stripe", Purpose: "webhook_signing", DisplayLabel: "Stripe webhook signing secret", ManualVault: true, MerchantWritable: true, Validation: "format"},
	{Name: SecretStripeWebhookSigningThin, Provider: "stripe", Purpose: "webhook_signing", DisplayLabel: "Stripe thin event signing secret", ManualVault: true, MerchantWritable: true, Validation: "format"},
	{Name: SecretNMIMobiusProductionKey, Provider: "nmi", Purpose: "mobius_production_key", DisplayLabel: "Mobius/NMI production key", ManualVault: true, MerchantWritable: true, Validation: "presence"},
	{Name: SecretNMIMobiusTokenizationKey, Provider: "nmi", Purpose: "tokenization_key", DisplayLabel: "Mobius/NMI tokenization key", ManualVault: true, MerchantWritable: true, Validation: "presence", PlaintextReadable: true},
	{Name: SecretNMIMobiusTokenizationURL, Provider: "nmi", Purpose: "tokenization_url", DisplayLabel: "Mobius/NMI Collect.js URL", ManualVault: true, MerchantWritable: true, Validation: "url", PlaintextReadable: true},
	{Name: SecretCCBillAccountConfig, Provider: "ccbill", Purpose: "account_config", DisplayLabel: "CCBill account configuration", ManualVault: true, MerchantWritable: true, Validation: "presence"},
	{Name: SecretSolanaPrivateKey, Provider: "solana", Purpose: "signing_keypair", DisplayLabel: "Solana signing keypair", ManualVault: true, MerchantWritable: false, Validation: "presence"},
}

// MerchantSecretRegistry returns the canonical OpenRails merchant-secret registry.
func MerchantSecretRegistry() []SecretDefinition {
	out := make([]SecretDefinition, len(merchantSecretRegistry))
	copy(out, merchantSecretRegistry)
	return out
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
//     subscription, suspend a merchant, or skip a webhook signature check.
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
	// Get returns the secret for (merchant, name), or ErrSecretNotFound.
	Get(ctx context.Context, merchantID merchant.ID, name string) (Secret, error)
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
