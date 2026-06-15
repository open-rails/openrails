package tenancy

import (
	"context"
	"errors"
	"fmt"

	"github.com/open-rails/openrails/pkg/merchant"
)

// StripeCredentials are a tenant's processor credentials, loaded by tenant id at
// request time (NOT injected process-wide). Empty fields mean "not configured".
type StripeCredentials struct {
	SecretKey            string
	WebhookSigningSecret string
	WebhookSigningThin   string
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
