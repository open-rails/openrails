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
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"path"
	"strings"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/custodians"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/modules/payments/rails"
	"github.com/open-rails/openrails/pkg/merchant"
)

// A merchant secret has exactly ONE spelling: the PSP-scoped name
// `psps/<rail>/<environment>/<account_id>/<key>` built by PSPSecretName. The
// flat `<rail>/<purpose>` names (stripe/secret_key, nmi/mobius/security_key, …)
// were retired in #884 — they were write-only surface, read only on a
// `pool == nil` branch that no production construction can reach, and they baked
// a PSP key ("mobius") into names presented as rail-generic.

// PSPSecretName returns the canonical secret-store name for a
// provider-account-owned credential. The merchant id still namespaces the store;
// this path adds provider identity so one merchant can rotate or run multiple
// accounts of the same provider without credential collisions.
func PSPSecretName(rail, environment, accountID, key string) (string, error) {
	rail = normalizeProviderSecretType(rail)
	environment = normalizeProviderSecretEnvironment(environment)
	accountID = strings.TrimSpace(accountID)
	key, err := NormalizePSPSecretKey(rail, key)
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
	// The prefix is part of the DURABLE canonical secret-name shape (persisted
	// in the merchant-secret store, including HashiCorp Vault KV paths).
	// Renaming it means migrating stored names in EVERY deployment (SQL for
	// DB-backed, a KV move for Vault-backed) — treat it with
	// stripeapi.APIVersion discipline: never float it casually.
	return path.Join("psps", rail, environment, url.PathEscape(accountID), key), nil
}

// CustodianSecretName is the custody sibling of PSPSecretName (or#880):
// `custodians/<kind>/<environment>/<account_id>/<key>`. It scopes by the
// custodian's IDENTITY — not by the merchant's nickname for it — for the same
// reason PSPSecretName does (#884): a stored name must survive a re-key of the
// manifest, and one identity must address one credential set.
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
	// Same durability contract as the psps/ prefix: this string is persisted
	// in every secret backend, including Vault KV paths. Never float it.
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

// ParsePSPSecretName parses a provider-account-scoped secret name.
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
		return "", "", "", "", true, fmt.Errorf("invalid provider account id escape: %w", err)
	}
	key, err = NormalizePSPSecretKey(rail, parts[4])
	if err != nil {
		return "", "", "", "", true, err
	}
	if rail == "" || environment == "" || strings.TrimSpace(accountID) == "" {
		return "", "", "", "", true, fmt.Errorf("invalid provider account secret name %q", name)
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
// a provider account against the rail's registry-declared slots (#669). It
// deliberately returns key fragments, not legacy broad merchant secret names.
func NormalizePSPSecretKey(rail, key string) (string, error) {
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

// MerchantSecretStatus is the read-only dashboard/admin view of one merchant
// secret slot. It never contains plaintext. The advertised slots come from the
// rail credential registry (#884) — the same source the money path reads with.
type MerchantSecretStatus struct {
	Name string `json:"name"`
	// Rail is the gateway kind for a PSP-scoped credential; "" for a
	// custodian-scoped one (or#880 — custody is not a rail).
	Rail string `json:"rail"`
	// Custodian is the vendor kind for a custodian-scoped credential; "" for a
	// PSP-scoped one. Exactly one of Rail/Custodian is set.
	Custodian        string `json:"custodian,omitempty"`
	Key              string `json:"key"`
	DisplayLabel     string `json:"display_label"`
	MerchantWritable bool   `json:"merchant_writable"`
	Configured       bool   `json:"configured"`
	Version          int    `json:"version,omitempty"`
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

// VersionedSecretReader is the cross-node ROTATION CUTOVER contract (or#812).
//
// A read-through secret cache is per-process, so a credential rotated on node A
// stays cached on node B until B's entry expires — up to DefaultSecretCacheTTL
// of a retired credential still being presented to a gateway. The fix is a
// versioned read: the PSP row (shared DB, re-read live on every credential
// resolution) records the Secret.Version each credential reached at its last
// rotation, and a reader that holds an OLDER version must go back to the
// backend instead of answering from cache.
//
// Only the caching wrapper needs to implement this; ReadSecretRef degrades to
// a plain Get for uncached stores, which are already never stale.
type VersionedSecretReader interface {
	GetAtLeastVersion(ctx context.Context, merchantID merchant.ID, name string, minVersion int) (Secret, error)
}

// SecretRef names a credential AND the version floor a reader must satisfy for
// it. MinVersion 0 means "no floor recorded" — the pre-rotation state, and the
// state of every secret written outside the provider-config API.
type SecretRef struct {
	Name       string
	MinVersion int
}

// ReadSecretRef reads ref through reader, honouring the version floor when the
// reader can. Every provider-credential read goes through here so the cutover
// rule lives in exactly one place.
func ReadSecretRef(ctx context.Context, reader MerchantSecretReader, id merchant.ID, ref SecretRef) (Secret, error) {
	if reader == nil {
		return Secret{}, errors.New("merchants: no secret store configured")
	}
	if versioned, ok := reader.(VersionedSecretReader); ok && ref.MinVersion > 0 {
		return versioned.GetAtLeastVersion(ctx, id, ref.Name, ref.MinVersion)
	}
	return reader.Get(ctx, id, ref.Name)
}

// PSPSecretResolver resolves the canonical secret name for the
// active provider account a merchant should use.
type PSPSecretResolver interface {
	ActivePSPSecretName(ctx context.Context, merchantID merchant.ID, rail, environment, key string) (string, bool, error)
}

// PSPSecretRefResolver is PSPSecretResolver plus the rotation version floor —
// the form every credential read should use (or#812).
type PSPSecretRefResolver interface {
	ActivePSPSecretRef(ctx context.Context, merchantID merchant.ID, rail, environment, key string) (SecretRef, bool, error)
}

// PSPScope is the configured provider account selected for a
// merchant rail/environment.
type PSPScope struct {
	ID          uuid.UUID
	Rail        string
	Environment string
	AccountID   string
	// Key is the manifest account key ("mobius") — the
	// payment-provider vocabulary catalog links and checkout use.
	Key      string
	Settings map[string]any
	// CustodianID references the custodian holding the instruments charged
	// through this PSP (or#880). nil = the PSP holds its own.
	CustodianID *uuid.UUID
	// CredentialVersions is the rotation watermark per credential key
	// (or#812): the Secret.Version each credential reached the last time it
	// was rotated through the provider-config API. Absent/zero = no floor.
	CredentialVersions map[string]int
}

// SecretRef returns the scoped secret name for key together with the rotation
// version floor recorded on this PSP row.
func (s PSPScope) SecretRef(key string) (SecretRef, error) {
	name, err := PSPSecretName(s.Rail, s.Environment, s.AccountID, key)
	if err != nil {
		return SecretRef{}, err
	}
	return SecretRef{Name: name, MinVersion: s.CredentialVersions[NormalizeCredentialVersionKey(key)]}, nil
}

// PSPSecretRef builds a scoped SecretRef straight from a PSP row's identity and
// its evidence document — for the paths that hold the raw row rather than a
// resolved PSPScope.
func PSPSecretRef(rail, environment, accountID string, evidence []byte, key string) (SecretRef, error) {
	name, err := PSPSecretName(rail, environment, accountID, key)
	if err != nil {
		return SecretRef{}, err
	}
	return SecretRef{Name: name, MinVersion: CredentialVersions(evidence)[NormalizeCredentialVersionKey(key)]}, nil
}

// CredentialVersions reads the or#812 rotation watermarks out of a PSP row's
// evidence document.
func CredentialVersions(evidence []byte) map[string]int {
	if len(evidence) == 0 {
		return nil
	}
	var doc struct {
		CredentialVersions map[string]int `json:"credential_versions"`
	}
	if json.Unmarshal(evidence, &doc) != nil {
		return nil
	}
	return doc.CredentialVersions
}

// NormalizeCredentialVersionKey is the canonical form credential-version keys
// are recorded under: lowercase, trimmed. It deliberately does NOT go through
// NormalizePSPSecretKey (which rejects unknown keys) — a version floor for a
// key this build does not recognise is still worth honouring.
func NormalizeCredentialVersionKey(key string) string {
	return strings.ToLower(strings.TrimSpace(key))
}

// PSPScopeResolver resolves the selected provider account without
// requiring a particular secret key.
type PSPScopeResolver interface {
	ActivePSPScope(ctx context.Context, merchantID merchant.ID, rail, environment string) (PSPScope, bool, error)
}

// PSPKeyResolver resolves a declared account by its manifest
// account key — the payment-provider name checkout requests
// and catalog provider_links use.
type PSPKeyResolver interface {
	PSPScopeByKey(ctx context.Context, merchantID merchant.ID, key, environment string) (PSPScope, bool, error)
}

// PSPRailScopesResolver lists every non-archived account on a rail kind —
// checkout's unambiguous rail-kind fallback (#848).
type PSPRailScopesResolver interface {
	ActivePSPScopesForRail(ctx context.Context, merchantID merchant.ID, rail, environment string) ([]PSPScope, error)
}

// ArchivedPSPKeyResolver reports whether a key names an ARCHIVED account
// (or#288). Routing needs it to tell "you retired this PSP" (not_armed) from
// "no such PSP was ever declared" (unknown_selector) — two very different
// answers to "why didn't my checkout go there".
type ArchivedPSPKeyResolver interface {
	PSPKeyArchived(ctx context.Context, merchantID merchant.ID, key, environment string) (bool, error)
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

// cleanSecretName normalises a secret name. It REJECTS (by returning "") a
// name containing a path-traversal segment: every caller allowlists the name
// and accountID is PathEscaped, so this is not reachable today — but the
// function's output is joined into a Vault path, and a guard that only trims
// slashes is one refactor away from being the hole (SEC-24 item 6).
func cleanSecretName(name string) string {
	cleaned := strings.Trim(strings.TrimSpace(name), "/")
	if cleaned == "." || cleaned == ".." {
		return ""
	}
	for _, seg := range strings.Split(cleaned, "/") {
		if seg == ".." || seg == "." {
			return ""
		}
	}
	return cleaned
}
