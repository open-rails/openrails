package tenancy

import (
	"context"
	"errors"
	"fmt"
	"strings"

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

// PutCredential stores/rotates a single per-tenant credential and writes a
// credential audit row (issue #225 rotation API + audit log). action is recorded
// as the audit action (e.g. "put" or "rotate").
func (s *Service) PutCredential(ctx context.Context, id merchant.ID, name, value, action, actor string) (Secret, error) {
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
	action = strings.TrimSpace(action)
	if action == "" {
		action = "put"
	}
	s.audit(ctx, id, name, action, actor, fmt.Sprintf("version=%d", sec.Version))
	return sec, nil
}

// RotateCredential is PutCredential with action="rotate".
func (s *Service) RotateCredential(ctx context.Context, id merchant.ID, name, value, actor string) (Secret, error) {
	return s.PutCredential(ctx, id, name, value, "rotate", actor)
}

// TestStripeCredential verifies a tenant's stored Stripe secret key works WITHOUT
// charging, by listing the account's balance via the Stripe API. tester is the
// verification function (so this stays testable without a live Stripe); when nil,
// a default real Stripe balance check is used. It records a "test" audit row.
func (s *Service) TestStripeCredential(ctx context.Context, id merchant.ID, tester func(ctx context.Context, secretKey string) error) error {
	return s.ValidateCredential(ctx, id, SecretStripeSecretKey, "", "", tester)
}

// audit best-effort appends a credential audit row. A failure to audit must not
// fail the underlying operation, so errors are swallowed (the operation already
// succeeded); a real deployment routes these to its log pipeline.
func (s *Service) audit(ctx context.Context, id merchant.ID, name, action, actor, detail string) {
	if s.pool == nil {
		return
	}
	_, _ = s.pool.Exec(ctx, `
		INSERT INTO openrails.tenant_credential_audit (tenant_id, name, action, actor, detail)
		VALUES ($1::uuid, $2, $3, NULLIF($4,''), NULLIF($5,''))
	`, id.String(), name, action, strings.TrimSpace(actor), detail)
}
