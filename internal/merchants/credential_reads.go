package merchants

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/merchantdocs"
)

// ErrSecretNotFound means the merchant's configuration holds no such
// credential. It is terminal for that credential: never retry it, and never
// read it as "verification disabled".
var ErrSecretNotFound = errors.New("merchants: merchant secret not found")

// ErrConfigUnavailable is an operational failure reading the
// merchant's configuration (Vault unreachable, sealed or refusing): retry;
// never cancel, delete or skip a signature check over it.
var ErrConfigUnavailable = merchantdocs.ErrUnavailable

// Secret is one credential value and the revision of the document holding it.
type Secret struct {
	Name    string
	Value   string
	Version int
}

// MerchantSecretReader reads one credential of a merchant by its name
// (PSPSecretName, CustodianSecretName).
type MerchantSecretReader interface {
	Get(ctx context.Context, merchantID billing.MerchantID, name string) (Secret, error)
}

// SecretMap reads fixed credentials by name, the same for every merchant:
// for tools and tests that have no merchant configuration.
type SecretMap map[string]string

func (m SecretMap) Get(_ context.Context, _ billing.MerchantID, name string) (Secret, error) {
	name = cleanSecretName(name)
	if v := strings.TrimSpace(m[name]); v != "" {
		return Secret{Name: name, Value: v, Version: 1}, nil
	}
	return Secret{}, ErrSecretNotFound
}

// SecretRef names a credential slot. Retired reads as absent: nothing arms a
// duplicate declaration.
type SecretRef struct {
	Name    string
	Retired bool
}

// ReadSecretRef reads ref through reader.
func ReadSecretRef(ctx context.Context, reader MerchantSecretReader, id billing.MerchantID, ref SecretRef) (Secret, error) {
	if ref.Retired {
		return Secret{}, ErrSecretNotFound
	}
	if reader == nil {
		return Secret{}, errors.New("merchants: no merchant configuration")
	}
	return reader.Get(ctx, id, ref.Name)
}

// PSPScope is one PSP: its identity joined with its document.
type PSPScope struct {
	ID          uuid.UUID
	Rail        string
	Environment string
	AccountID   string
	// Key is the merchant's name for the PSP: the payment-provider vocabulary
	// catalog links and checkout use.
	Key      string
	Settings map[string]any
	// Archived PSPs take no new work and drain: an archived document, an
	// identity its key no longer names, or one no document names.
	Archived bool
	// Custodian is the key of the custodian holding the instruments charged
	// through this PSP, CustodianID its identity (or#880); "" and nil mean the
	// PSP holds its own.
	Custodian   string
	CustodianID *uuid.UUID
	Signer      *merchantdocs.Signer
	// SignerChange is a pending, unapproved Transit signer public key.
	SignerChange string
	// DuplicateAccount: another live PSP already declares this gateway
	// account; no credential of this one is read.
	DuplicateAccount  bool
	Revision          int64
	CreatedAt         time.Time
	UpdatedAt         time.Time
	ValidatedAt       *time.Time
	WebhookEndpointID string
	// WebhookOverlapUntil bounds webhook_signing_secret_previous (SEC-29).
	WebhookOverlapUntil time.Time
	secrets             map[string]string
}

// SecretRef names the PSP's credential slot key.
func (s PSPScope) SecretRef(key string) (SecretRef, error) {
	if s.DuplicateAccount {
		return SecretRef{Retired: true}, nil
	}
	name, err := PSPSecretName(s.Rail, s.Environment, s.AccountID, key)
	if err != nil {
		return SecretRef{}, err
	}
	return SecretRef{Name: name}, nil
}

// Secret is the value of credential key, "" when the document holds none or
// the PSP duplicates another's account.
func (s PSPScope) Secret(key string) string {
	if s.DuplicateAccount {
		return ""
	}
	return strings.TrimSpace(s.secrets[NormalizeCredentialVersionKey(key)])
}

// HasSecret reports whether the document holds credential key.
func (s PSPScope) HasSecret(key string) bool {
	return strings.TrimSpace(s.secrets[NormalizeCredentialVersionKey(key)]) != ""
}

// NormalizeCredentialVersionKey is the canonical form of a credential key:
// lowercase, trimmed.
func NormalizeCredentialVersionKey(key string) string {
	return strings.ToLower(strings.TrimSpace(key))
}

// PSPSecretResolver resolves the canonical secret name for the active PSP a
// merchant should use.
type PSPSecretResolver interface {
	ActivePSPSecretName(ctx context.Context, merchantID billing.MerchantID, rail, environment, key string) (string, bool, error)
}

// PSPSecretRefResolver is PSPSecretResolver as a SecretRef.
type PSPSecretRefResolver interface {
	ActivePSPSecretRef(ctx context.Context, merchantID billing.MerchantID, rail, environment, key string) (SecretRef, bool, error)
}

// PSPScopeResolver resolves the selected PSP without requiring a particular
// secret key.
type PSPScopeResolver interface {
	ActivePSPScope(ctx context.Context, merchantID billing.MerchantID, rail, environment string) (PSPScope, bool, error)
}

// PSPKeyResolver resolves a live PSP by its key: the payment-provider name
// checkout requests and catalog provider_links use.
type PSPKeyResolver interface {
	PSPScopeByKey(ctx context.Context, merchantID billing.MerchantID, key, environment string) (PSPScope, bool, error)
}

// PSPRailScopesResolver lists every live PSP on a rail: checkout's
// unambiguous rail-kind fallback (#848).
type PSPRailScopesResolver interface {
	ActivePSPScopesForRail(ctx context.Context, merchantID billing.MerchantID, rail, environment string) ([]PSPScope, error)
}

// ArchivedPSPKeyResolver reports whether a key names an ARCHIVED PSP (or#288),
// so routing can tell "you retired this PSP" from "no such PSP".
type ArchivedPSPKeyResolver interface {
	PSPKeyArchived(ctx context.Context, merchantID billing.MerchantID, key, environment string) (bool, error)
}

// PSPIdentityScopeResolver resolves a PSP by identity for existing
// obligations; archived PSPs remain available.
type PSPIdentityScopeResolver interface {
	PSPScopeByID(ctx context.Context, merchantID billing.MerchantID, pspID uuid.UUID) (PSPScope, bool, error)
}

// cleanSecretName normalises a secret name, refusing ("") one with a
// path-traversal segment.
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

// configSecrets reads credentials from the merchant's documents.
type configSecrets struct{ service *Service }

// Secrets is the merchant credential reader over the configuration.
func (s *Service) Secrets() MerchantSecretReader {
	if s == nil || s.config == nil {
		return nil
	}
	return configSecrets{service: s}
}

func (r configSecrets) Get(ctx context.Context, id billing.MerchantID, name string) (Secret, error) {
	name = cleanSecretName(name)
	if id.IsZero() || name == "" {
		return Secret{}, errors.New("merchants: a credential read needs a merchant and a name")
	}
	set, err := r.service.config.Get(ctx, id)
	if err != nil {
		return Secret{}, err
	}
	if kind, environment, accountID, key, ok, err := ParseCustodianSecretName(name); ok {
		if err != nil {
			return Secret{}, err
		}
		for _, doc := range set.Custodians {
			c := doc.Value
			if c.Kind == kind && c.Environment == environment && c.AccountID == accountID {
				return found(name, c.Secrets[key], doc.Revision)
			}
		}
		return Secret{}, ErrSecretNotFound
	}
	rail, environment, accountID, key, ok, err := ParsePSPSecretName(name)
	if err != nil {
		return Secret{}, err
	}
	if !ok {
		return Secret{}, ErrSecretNotFound
	}
	for _, doc := range set.PSPs {
		p := doc.Value
		if p.Rail == rail && p.Environment == environment && p.AccountID == accountID {
			return found(name, p.Secrets[key], doc.Revision)
		}
	}
	return Secret{}, ErrSecretNotFound
}

func found(name, value string, revision int64) (Secret, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return Secret{}, ErrSecretNotFound
	}
	return Secret{Name: name, Value: value, Version: int(revision)}, nil
}
