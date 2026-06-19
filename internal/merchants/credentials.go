package merchants

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails/pkg/merchant"
)

// DefaultNMICollectJSURL is the standard NMI Collect.js script URL.
const DefaultNMICollectJSURL = "https://secure.networkmerchants.com/token/Collect.js"

// StripeCredentials are a tenant's processor credentials, loaded by tenant id at
// request time (NOT injected process-wide). Empty fields mean "not configured".
type StripeCredentials struct {
	SecretKey            string
	WebhookSigningSecret string
	WebhookSigningThin   string
}

// NMITokenizationConfig is browser-facing NMI tokenization configuration for a
// merchant/provider. The key is public, but it is still loaded from the
// merchant-scoped provider config so tenant A's browser config does not bleed
// into tenant B.
type NMITokenizationConfig struct {
	TokenizationKey string
	CollectJSURL    string
}

// LoadStripeCredentials loads a tenant's Stripe credentials from the secret store
// (issue #225). A missing individual secret is not an error — it is returned as an
// empty field — so a tenant that has only a webhook secret (or only an API key)
// still loads. A nil secret store yields empty credentials.
func (s *Service) LoadStripeCredentials(ctx context.Context, id merchant.ID) (StripeCredentials, error) {
	var creds StripeCredentials
	if s.secrets == nil {
		return creds, nil
	}
	if s.pool == nil {
		return s.loadStripeCredentialsByName(ctx, id, SecretStripeSecretKey, SecretStripeWebhookSigning, SecretStripeWebhookSigningThin)
	}
	scope, ok, err := s.primaryProviderAccountSecretScope(ctx, id, "stripe", "live")
	if err != nil {
		return creds, err
	}
	if !ok {
		return creds, nil
	}
	secretKeyName, err := scope.secretName("secret_key")
	if err != nil {
		return creds, err
	}
	webhookName, err := scope.secretName("webhook_signing_secret")
	if err != nil {
		return creds, err
	}
	thinName, err := scope.secretName("webhook_signing_secret_thin")
	if err != nil {
		return creds, err
	}
	return s.loadStripeCredentialsByName(ctx, id, secretKeyName, webhookName, thinName)
}

func (s *Service) LoadNMIWebhookSigningSecret(ctx context.Context, id merchant.ID, provider string) (string, error) {
	if s.secrets == nil || id.IsZero() {
		return "", nil
	}
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case "mobius", "nmi":
	default:
		return "", nil
	}
	if s.pool == nil {
		return s.secretValue(ctx, id, SecretNMIMobiusWebhookSigning)
	}
	scope, ok, err := s.primaryProviderAccountSecretScope(ctx, id, "nmi", "live")
	if err != nil {
		return "", err
	}
	if !ok {
		return "", nil
	}
	secretName, err := scope.secretName("webhook_signing_secret")
	if err != nil {
		return "", err
	}
	return s.secretValue(ctx, id, secretName)
}

// LoadNMITokenizationConfig loads merchant-scoped browser tokenization config
// for an NMI provider. Missing values return empty fields, except CollectJSURL
// defaults to NMI's standard URL when a supported provider is selected.
func (s *Service) LoadNMITokenizationConfig(ctx context.Context, id merchant.ID, provider string) (NMITokenizationConfig, error) {
	var cfg NMITokenizationConfig
	if s.secrets == nil || id.IsZero() {
		return cfg, nil
	}

	var keyName, urlName string
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case "mobius":
		if s.pool == nil {
			keyName = SecretNMIMobiusTokenizationKey
			urlName = SecretNMIMobiusTokenizationURL
			break
		}
		scope, ok, err := s.primaryProviderAccountSecretScope(ctx, id, "nmi", "live")
		if err != nil {
			return cfg, err
		}
		if !ok {
			cfg.CollectJSURL = DefaultNMICollectJSURL
			return cfg, nil
		}
		keyName, err = scope.secretName("tokenization_key")
		if err != nil {
			return cfg, err
		}
		urlName, err = scope.secretName("tokenization_url")
		if err != nil {
			return cfg, err
		}
	default:
		return cfg, nil
	}

	get := func(name string) (string, error) {
		sec, err := s.secrets.Get(ctx, id, name)
		if err != nil {
			if errors.Is(err, ErrSecretNotFound) {
				return "", nil
			}
			return "", err
		}
		return sec.Value, nil
	}

	var err error
	if cfg.TokenizationKey, err = get(keyName); err != nil {
		return cfg, err
	}
	if cfg.CollectJSURL, err = get(urlName); err != nil {
		return cfg, err
	}
	if cfg.CollectJSURL == "" {
		cfg.CollectJSURL = DefaultNMICollectJSURL
	}
	return cfg, nil
}

func (s *Service) loadStripeCredentialsByName(ctx context.Context, id merchant.ID, secretKeyName, webhookName, thinName string) (StripeCredentials, error) {
	var creds StripeCredentials
	var err error
	if creds.SecretKey, err = s.secretValue(ctx, id, secretKeyName); err != nil {
		return creds, err
	}
	if creds.WebhookSigningSecret, err = s.secretValue(ctx, id, webhookName); err != nil {
		return creds, err
	}
	if creds.WebhookSigningThin, err = s.secretValue(ctx, id, thinName); err != nil {
		return creds, err
	}
	return creds, nil
}

func (s *Service) secretValue(ctx context.Context, id merchant.ID, name string) (string, error) {
	sec, err := s.secrets.Get(ctx, id, name)
	if err != nil {
		if errors.Is(err, ErrSecretNotFound) {
			return "", nil
		}
		return "", err
	}
	return sec.Value, nil
}

type providerAccountSecretScope struct {
	providerType string
	environment  string
	accountID    string
}

func (s providerAccountSecretScope) secretName(key string) (string, error) {
	return ProviderAccountSecretName(s.providerType, s.environment, s.accountID, key)
}

// PrimaryProviderAccountSecretName resolves the enabled primary provider account
// for a merchant/provider/environment and returns that account's scoped secret
// name for key.
func (s *Service) PrimaryProviderAccountSecretName(ctx context.Context, id merchant.ID, providerType, environment, key string) (string, bool, error) {
	scope, ok, err := s.primaryProviderAccountSecretScope(ctx, id, providerType, environment)
	if err != nil || !ok {
		return "", ok, err
	}
	name, err := scope.secretName(key)
	if err != nil {
		return "", false, err
	}
	return name, true, nil
}

func (s *Service) primaryProviderAccountSecretScope(ctx context.Context, id merchant.ID, providerType, environment string) (providerAccountSecretScope, bool, error) {
	if s == nil || s.pool == nil || id.IsZero() {
		return providerAccountSecretScope{}, false, nil
	}
	environment = normalizeProviderSecretEnvironment(environment)
	if environment == "" {
		return providerAccountSecretScope{}, false, fmt.Errorf("provider account environment must be live or test")
	}
	providerType = normalizeProviderSecretType(providerType)
	var scope providerAccountSecretScope
	err := s.pool.MerchantTx(ctx, id, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			SELECT provider_type, environment, account_id
			  FROM openrails.provider_accounts
			 WHERE merchant_id = $1::uuid
			   AND provider_type = lower($2)
			   AND environment = $3
			   AND role = 'primary'
			   AND status = 'enabled'
			 LIMIT 1
		`, id.String(), providerType, environment).Scan(&scope.providerType, &scope.environment, &scope.accountID)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return providerAccountSecretScope{}, false, nil
	}
	if err != nil {
		return providerAccountSecretScope{}, false, fmt.Errorf("load primary provider account %s/%s: %w", providerType, environment, err)
	}
	return scope, true, nil
}

// PutCredential stores/rotates a single per-tenant credential.
func (s *Service) PutCredential(ctx context.Context, id merchant.ID, name, value string) (Secret, error) {
	if s.secrets == nil {
		return Secret{}, errors.New("tenancy: no secret store configured")
	}
	name = cleanSecretName(name)
	if !SecretWritable(name) {
		return Secret{}, fmt.Errorf("tenancy: unknown tenant secret %q", name)
	}
	if err := validateSecretValueLocal(name, value); err != nil {
		return Secret{}, fmt.Errorf("tenancy: validate tenant secret %q: %w", name, err)
	}
	sec, err := s.secrets.Put(ctx, id, name, value)
	if err != nil {
		return Secret{}, err
	}
	return sec, nil
}

// RotateCredential is PutCredential with action="rotate".
func (s *Service) RotateCredential(ctx context.Context, id merchant.ID, name, value string) (Secret, error) {
	return s.PutCredential(ctx, id, name, value)
}

// TestStripeCredential verifies a tenant's stored Stripe secret key works WITHOUT
// charging, by listing the account's balance via the Stripe API. tester is the
// verification function (so this stays testable without a live Stripe); when nil,
// a default real Stripe balance check is used. It records a "test" audit row.
func (s *Service) TestStripeCredential(ctx context.Context, id merchant.ID, tester func(ctx context.Context, secretKey string) error) error {
	return s.ValidateCredential(ctx, id, SecretStripeSecretKey, "", tester)
}
