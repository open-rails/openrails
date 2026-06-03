// Package tenancy implements tenant provisioning, lifecycle, per-tenant processor
// credentials, and webhook routing for OpenRails' multi-tenant platform (issue
// #225). It builds on the #223 tenant primitive (pkg/tenant + billing.tenants)
// and the #224 in-process AuthKit control plane (internal/controlplane): the
// lifecycle service mints/links operator orgs and OATs through control-plane core
// calls and records tenant directory state directly in billing.* (OpenRails-owned
// control-plane state).
package tenancy

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/open-rails/openrails/pkg/tenant"
)

// Canonical per-tenant secret names. The store namespaces values by
// (tenant id, name); these are the well-known names OpenRails reads at request
// time. A managed (Vault-backed) deployment keeps the SAME names but resolves
// them to a tenant-scoped Vault path.
const (
	// SecretStripeSecretKey is the tenant's Stripe API secret key (BYO or Connect).
	SecretStripeSecretKey = "stripe/secret_key"
	// SecretStripeWebhookSigning is the tenant's Stripe webhook signing secret,
	// used to verify inbound webhooks AFTER tenant resolution.
	SecretStripeWebhookSigning = "stripe/webhook_signing_secret"
	// SecretStripeWebhookSigningThin is the tenant's Stripe "thin" Event
	// Destination signing secret (a single endpoint may receive both).
	SecretStripeWebhookSigningThin = "stripe/webhook_signing_secret_thin"
)

// ErrSecretNotFound is returned by a TenantSecretStore when no secret exists for
// the (tenant, name) pair. This is a TERMINAL condition for that (tenant, name):
// the secret is genuinely absent (tenant never configured it, or was
// deprovisioned). Callers distinguish "not configured" from a backend error with
// errors.Is.
var ErrSecretNotFound = errors.New("tenancy: tenant secret not found")

// ErrSecretBackendUnavailable wraps any OPERATIONAL failure of the underlying
// secret backend — Vault unreachable / sealed / permission-denied, a DB query
// error, or a not-yet-wired Vault client. It is distinct from ErrSecretNotFound
// (terminal absence) precisely so money-path callers can fail closed correctly:
//
//   - secret ABSENT (ErrSecretNotFound)      -> terminal for that tenant; never
//     retry, and (critically) NEVER treat as "verification disabled".
//   - backend UNAVAILABLE (this error)       -> RETRY; do NOT cancel a
//     subscription, suspend a tenant, or skip a webhook signature check.
//
// The recurring Solana pull worker and the webhook signature verifier both branch
// on errors.Is(err, ErrSecretBackendUnavailable) to retry rather than treat a
// transient Vault outage as "no secret".
var ErrSecretBackendUnavailable = errors.New("tenancy: tenant secret backend unavailable")

// Secret is a stored per-tenant secret value plus its version (bumped on every
// rotation), so callers can audit/rotate without re-reading the plaintext.
type Secret struct {
	Name    string
	Value   string
	Version int
}

// TenantSecretStore is the per-tenant secrets abstraction (issue #225). Every
// operation is namespaced by tenant id so one tenant can never read or overwrite
// another tenant's Stripe credentials or webhook signing secrets.
//
// Two implementations ship:
//
//   - dbSecretStore / memSecretStore: build and run WITHOUT a live Vault (the
//     dev / self-hosted default). DB-backed persists to billing.tenant_secrets.
//   - vaultSecretStore: a documented adapter that resolves the SAME (tenant,
//     name) addressing to a tenant-scoped Vault KV path. It is a stub today and
//     is wired in managed deployments without any schema or caller change.
type TenantSecretStore interface {
	// Get returns the secret for (tenant, name), or ErrSecretNotFound.
	Get(ctx context.Context, tenantID tenant.ID, name string) (Secret, error)
	// Put creates or rotates the secret for (tenant, name). It is idempotent on
	// value: putting the same value twice is a no-op rotation. Returns the stored
	// secret (with its new version).
	Put(ctx context.Context, tenantID tenant.ID, name, value string) (Secret, error)
	// Delete removes the secret for (tenant, name). Deleting a missing secret is a
	// no-op (idempotent), so it is safe in tenant-delete purge.
	Delete(ctx context.Context, tenantID tenant.ID, name string) error
	// List enumerates the secret NAMES (never values) held for a tenant. Used by
	// the export path for Vault-side secret enumeration (GDPR / portability).
	List(ctx context.Context, tenantID tenant.ID) ([]string, error)
}

// validateSecretRef guards the (tenant, name) addressing shared by every store
// so a blank/zero tenant or empty name can never read or clobber a secret.
func validateSecretRef(tenantID tenant.ID, name string) error {
	if tenantID.IsZero() {
		return fmt.Errorf("tenancy: secret access requires a tenant id")
	}
	if strings.TrimSpace(name) == "" {
		return fmt.Errorf("tenancy: secret access requires a name")
	}
	return nil
}
