package merchants

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/pkg/merchant"
)

// StripeWebhookCredentialState is private operator state, never a public DTO.
type StripeWebhookCredentialState struct {
	SecretKey, CurrentSecret, PreviousSecret, EndpointID string
	Writable                                             bool
}

// StripeWebhookPublication binds provider-created credentials to one account and
// its loaded revision. Each reconcile pass owns one instance.
type StripeWebhookPublication struct {
	service  *Service
	merchant merchant.ID
	account  string
	row      gen.OpenrailsPsp
	loaded   bool
}

func (s *Service) StripeWebhookPublication(id merchant.ID, account string) *StripeWebhookPublication {
	return &StripeWebhookPublication{service: s, merchant: id, account: account}
}
func (p *StripeWebhookPublication) Load(ctx context.Context) (StripeWebhookCredentialState, error) {
	s := p.service
	err := s.pool.MerchantTx(ctx, p.merchant, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		p.row, err = gen.New(tx).GetPSPByRailIdentity(ctx, gen.GetPSPByRailIdentityParams{MerchantID: p.merchant.UUID(), Rail: "stripe", Environment: &s.providerEnvironment, AccountID: p.account})
		return err
	})
	if err != nil {
		return StripeWebhookCredentialState{}, err
	}
	if p.row.Archived {
		return StripeWebhookCredentialState{}, ErrPaymentProviderAccountNotFound
	}
	creds, ok, err := s.LoadStripeCredentialsForAccount(ctx, p.merchant, p.account)
	if err != nil {
		return StripeWebhookCredentialState{}, err
	}
	if !ok {
		return StripeWebhookCredentialState{}, ErrPaymentProviderAccountNotFound
	}
	p.loaded = true
	return StripeWebhookCredentialState{SecretKey: creds.SecretKey, CurrentSecret: creds.WebhookSigningSecret, PreviousSecret: creds.WebhookSigningPrevious, EndpointID: unmarshalProviderEvidence(p.row.Evidence).WebhookEndpointID, Writable: CanStageCredentials(s.secrets)}, nil
}
func (p *StripeWebhookPublication) Publish(ctx context.Context, endpoint, secret string) error {
	if !p.loaded || strings.TrimSpace(endpoint) == "" || strings.TrimSpace(secret) == "" {
		return fmt.Errorf("managed webhook publication requires qualified endpoint and signing secret")
	}
	name, err := PSPSecretName("stripe", p.service.providerEnvironment, p.account, "webhook_signing_secret")
	if err != nil {
		return err
	}
	if err = validateSecretValueLocal(name, secret); err != nil {
		return err
	}
	names, keys := map[string]string{name: secret}, map[string]string{name: "webhook_signing_secret"}
	if _, published := CredentialRefs(p.row.Evidence)["webhook_signing_secret"]; !published {
		ref, err := PSPSecretRef("stripe", p.service.providerEnvironment, p.account, p.row.Evidence, "webhook_signing_secret")
		if err != nil {
			return err
		}
		previous, err := ReadSecretRef(ctx, p.service.secrets, p.merchant, ref)
		if err == nil {
			previousName, err := PSPSecretName("stripe", p.service.providerEnvironment, p.account, "webhook_signing_secret_previous")
			if err != nil {
				return err
			}
			names[previousName] = previous.Value
			keys[previousName] = "webhook_signing_secret_previous"
		} else if !errors.Is(err, ErrSecretNotFound) {
			return err
		}
	}
	return p.publish(ctx, endpoint, names, keys, false)
}

// RetireOverlap is called only after qualified provider endpoint retirement.
// Historical secret material remains; the published verifier reference is retired.
func (p *StripeWebhookPublication) RetireOverlap(ctx context.Context) error {
	if !p.loaded || unmarshalProviderEvidence(p.row.Evidence).WebhookEndpointID == "" {
		return fmt.Errorf("managed webhook retirement requires published endpoint identity")
	}
	return p.publish(ctx, unmarshalProviderEvidence(p.row.Evidence).WebhookEndpointID, nil, nil, true)
}
func (p *StripeWebhookPublication) publish(ctx context.Context, endpoint string, names, keys map[string]string, retire bool) error {
	if !CanStageCredentials(p.service.secrets) {
		return credentialWriteRefusal(p.service.secrets)
	}
	evidence := unmarshalProviderEvidence(p.row.Evidence)
	operation := uuid.NewSHA1(p.merchant.UUID(), []byte("stripe-webhook/"+p.service.providerEnvironment+"/"+p.account+"/"+endpoint))
	if retire {
		operation = uuid.NewSHA1(operation, []byte(fmt.Sprintf("retire/%d", evidence.Revision)))
	}
	row, err := p.service.publishProviderCredentials(ctx, p.merchant, "stripe", p.service.providerEnvironment, p.account, !p.row.Archived, UpsertPaymentProviderConfigRequest{OperationID: operation, ExpectedRevision: &evidence.Revision, AccountID: p.account}, names, keys, evidence.CredentialsValidated, p.row.LastVerifiedAt, credentialTransitionPublication{WebhookEndpointID: endpoint, RetireWebhookOverlap: retire})
	if err == nil {
		p.row = row
	}
	return err
}
