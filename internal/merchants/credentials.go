package merchants

import (
	"context"
	"errors"
	"fmt"
	"strings"

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
	if creds.SecretKey, err = get(SecretStripeSecretKey); err != nil {
		return creds, err
	}
	if creds.WebhookSigningSecret, err = get(SecretStripeWebhookSigning); err != nil {
		return creds, err
	}
	if creds.WebhookSigningThin, err = get(SecretStripeWebhookSigningThin); err != nil {
		return creds, err
	}
	return creds, nil
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
		keyName = SecretNMIMobiusTokenizationKey
		urlName = SecretNMIMobiusTokenizationURL
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

// PutCredential stores/rotates a single per-tenant credential.
func (s *Service) PutCredential(ctx context.Context, id merchant.ID, name, value string) (Secret, error) {
	if s.secrets == nil {
		return Secret{}, errors.New("tenancy: no secret store configured")
	}
	name = cleanSecretName(name)
	if _, ok := SecretDefinitionFor(name); !ok {
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
