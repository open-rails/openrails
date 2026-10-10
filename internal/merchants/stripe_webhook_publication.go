package merchants

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/merchant"
)

// StripeWebhookCredentialState is private operator state, never a public DTO.
type StripeWebhookCredentialState struct {
	SecretKey, CurrentSecret, PreviousSecret, EndpointID string
	Writable                                             bool
}

// StripeWebhookPublication writes a provider-created webhook signing secret
// into one Stripe PSP's document, at the revision it loaded. Each reconcile
// pass owns one instance.
type StripeWebhookPublication struct {
	service  *Service
	merchant billing.MerchantID
	account  string
	scope    PSPScope
	loaded   bool
}

func (s *Service) StripeWebhookPublication(id billing.MerchantID, account string) *StripeWebhookPublication {
	return &StripeWebhookPublication{service: s, merchant: id, account: account}
}

func (p *StripeWebhookPublication) Load(ctx context.Context) (StripeWebhookCredentialState, error) {
	scope, ok, err := p.service.PSPScopeByAccountID(ctx, p.merchant, "stripe", p.account)
	if err != nil {
		return StripeWebhookCredentialState{}, err
	}
	if !ok || scope.Archived {
		return StripeWebhookCredentialState{}, ErrPSPNotFound
	}
	p.scope, p.loaded = scope, true
	creds := p.service.stripeCredentials(scope)
	return StripeWebhookCredentialState{
		SecretKey: creds.SecretKey, CurrentSecret: creds.WebhookSigningSecret, PreviousSecret: creds.WebhookSigningPrevious,
		EndpointID: scope.WebhookEndpointID, Writable: p.service.config.Writable() && scope.Revision > 0,
	}, nil
}

// Publish writes the signing secret of the endpoint the provider created; the
// outgoing secret keeps verifying for MaxWebhookSecretOverlap.
func (p *StripeWebhookPublication) Publish(ctx context.Context, endpoint, secret string) error {
	endpoint, secret = strings.TrimSpace(endpoint), strings.TrimSpace(secret)
	if !p.loaded || endpoint == "" || secret == "" {
		return fmt.Errorf("managed webhook publication requires qualified endpoint and signing secret")
	}
	if err := validateCredentialValue("stripe", "webhook_signing_secret", secret); err != nil {
		return fmt.Errorf("managed webhook signing secret: %w", err)
	}
	return p.write(ctx, endpoint, func(next map[string]string, settings map[string]any) {
		if current := strings.TrimSpace(next["webhook_signing_secret"]); current != "" && current != secret {
			next["webhook_signing_secret_previous"] = current
			settings[WebhookOverlapExpiresKey] = p.service.now().Add(MaxWebhookSecretOverlap).Format(time.RFC3339)
		}
		next["webhook_signing_secret"] = secret
	})
}

// RetireOverlap is called only after qualified provider endpoint retirement:
// the outgoing secret stops verifying.
func (p *StripeWebhookPublication) RetireOverlap(ctx context.Context) error {
	if !p.loaded || p.scope.WebhookEndpointID == "" {
		return fmt.Errorf("managed webhook retirement requires published endpoint identity")
	}
	return p.write(ctx, p.scope.WebhookEndpointID, func(next map[string]string, settings map[string]any) {
		delete(next, "webhook_signing_secret_previous")
		delete(settings, WebhookOverlapExpiresKey)
	})
}

func (p *StripeWebhookPublication) write(ctx context.Context, endpoint string, change func(map[string]string, map[string]any)) error {
	s := p.service
	if !s.config.Writable() {
		return ErrConfigReadOnly
	}
	set, err := s.config.Get(ctx, p.merchant)
	if err != nil {
		return err
	}
	held, ok := set.PSPs[strings.ToLower(p.scope.Key)]
	if !ok || held.Revision != p.scope.Revision {
		return ErrRevisionMismatch
	}
	next := clonePSP(held.Value)
	if next.Secrets == nil {
		next.Secrets = map[string]string{}
	}
	if next.Settings == nil {
		next.Settings = map[string]any{}
	}
	change(next.Secrets, next.Settings)
	if len(next.Settings) == 0 {
		next.Settings = nil
	}
	if _, err := s.config.PutPSP(ctx, p.merchant, p.scope.Key, next, held.Revision); err != nil {
		return err
	}
	if err := s.database.RunInMerchantConn(merchant.WithID(ctx, p.merchant), func(ctx context.Context) error {
		return s.database.Gen(ctx).SetPSPWebhookEndpoint(ctx, gen.SetPSPWebhookEndpointParams{MerchantID: p.merchant.UUID(), ID: p.scope.ID, EndpointID: &endpoint})
	}); err != nil {
		return err
	}
	scope, ok, err := s.PSPScopeByID(ctx, p.merchant, p.scope.ID)
	if err == nil && ok {
		p.scope = scope
	}
	return err
}
