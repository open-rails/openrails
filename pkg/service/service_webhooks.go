// Package service provides the in-process billing API for embedded hosts.
package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/internal/modules/webhooks"
	"github.com/open-rails/openrails/internal/shared/webhookutil"
	"github.com/open-rails/openrails/internal/webhookauth"
)

// HandleWebhook processes an incoming webhook from a payment rail.
// It validates the signature, parses the payload, and dispatches SYNCHRONOUSLY
// through the same registry-driven dispatcher as the HTTP surfaces (#788):
// credentials resolve from the ctx merchant's armed rail state at dispatch
// time. A retryable processing failure returns an error (the host answers
// non-2xx and the provider redelivers); a non-retryable (poison) event is
// accepted so the provider stops retrying.
func (s *Service) HandleWebhook(ctx context.Context, req HandleWebhookRequest) (*WebhookResult, error) {
	_, err := s.requireWebhookRuntime()
	if err != nil {
		return nil, err
	}

	provider := webhookutil.CanonicalRail(req.Provider)

	log.WithFields(log.Fields{
		"provider":  provider,
		"client_ip": req.ClientIP,
	}).Debug("Received webhook via Service API")

	ingress, ok := serviceWebhookIngress[models.Rail(provider)]
	if !ok {
		return &WebhookResult{
			Accepted: false,
			Error:    fmt.Sprintf("invalid provider: %s", provider),
		}, nil
	}
	return ingress(s, ctx, provider, req)
}

// serviceWebhookIngress is the embedded-surface routing TABLE (#669): which
// handler serves each rail. Verification/posture logic stays in the handlers.
var serviceWebhookIngress = map[models.Rail]func(s *Service, ctx context.Context, provider string, req HandleWebhookRequest) (*WebhookResult, error){
	models.RailNMI: func(s *Service, ctx context.Context, provider string, req HandleWebhookRequest) (*WebhookResult, error) {
		return s.handleNMIWebhook(ctx, provider, req)
	},
	models.RailCCBill: func(s *Service, ctx context.Context, _ string, req HandleWebhookRequest) (*WebhookResult, error) {
		return s.handleCCBillWebhook(ctx, req)
	},
	models.RailStripe: func(s *Service, ctx context.Context, _ string, req HandleWebhookRequest) (*WebhookResult, error) {
		return s.handleStripeWebhook(ctx, req)
	},
}

func (s *Service) handleNMIWebhook(ctx context.Context, provider string, req HandleWebhookRequest) (*WebhookResult, error) {
	providerKey := webhookutil.CanonicalRail(provider)

	// #788: the signing secret comes from the ctx merchant's armed rail
	// state — an alias other than the bare rail name pins that declared
	// account; an unarmed rail rejects the webhook (fail closed).
	accountPin := ""
	if providerKey != string(models.RailNMI) {
		accountPin = providerKey
	}
	var signingSecret string
	if s.rt != nil && s.rt.RailConfigs != nil {
		proc, rerr := s.rt.RailConfigs.RailConfig(ctx, string(models.RailNMI), accountPin)
		if rerr != nil {
			return &WebhookResult{
				Accepted: false,
				Error:    fmt.Sprintf("nmi rail is not armed for provider '%s'", providerKey),
			}, nil
		}
		if proc.NMI != nil {
			signingSecret = proc.NMI.WebhookSigningSecret
		}
	}

	prepared, err := webhookutil.PrepareNMI(providerKey, req.Body, signingSecret, getHeaderValue(req.Headers, "Webhook-Signature"))
	if err != nil {
		if errors.Is(err, webhookutil.ErrNMIWebhookSecretMissing) || errors.Is(err, webhookutil.ErrNMIWebhookSignatureMissing) {
			log.WithError(err).Error("Missing webhook signature for NMI webhook")
			return &WebhookResult{
				Accepted: false,
				Error:    "missing webhook signature",
			}, nil
		}
		if errors.Is(err, webhookutil.ErrNMIWebhookSignatureInvalid) {
			log.WithError(err).Error("NMI webhook signature verification failed")
			return &WebhookResult{
				Accepted: false,
				Error:    "invalid webhook signature",
			}, nil
		}
		if errors.Is(err, webhookutil.ErrWebhookPayloadInvalid) {
			log.WithError(err).Error("failed to parse NMI webhook JSON")
			return &WebhookResult{
				Accepted: false,
				Error:    "invalid JSON data",
			}, nil
		}
		if errors.Is(err, webhookutil.ErrWebhookEventIDMissing) {
			return &WebhookResult{
				Accepted: false,
				Error:    "missing event_id in payload",
			}, nil
		}

		log.WithError(err).Error("failed to prepare NMI webhook")
		return &WebhookResult{
			Accepted: false,
			Error:    "invalid webhook payload",
		}, nil
	}

	if err := s.dispatchWebhook(ctx, prepared, req.ClientIP); err != nil {
		log.WithError(err).Error("nmi webhook processing failed")
		return nil, fmt.Errorf("webhook processing failed: %w", err)
	}

	return &WebhookResult{
		Accepted:  true,
		EventID:   prepared.EventID,
		EventType: prepared.EventType,
	}, nil
}

func getHeaderValue(headers map[string]string, keys ...string) string {
	for _, key := range keys {
		for headerName, value := range headers {
			if !strings.EqualFold(strings.TrimSpace(headerName), key) {
				continue
			}
			trimmed := strings.TrimSpace(value)
			if trimmed != "" {
				return trimmed
			}
		}
	}

	return ""
}

func (s *Service) handleCCBillWebhook(ctx context.Context, req HandleWebhookRequest) (*WebhookResult, error) {
	cfg, err := s.requireConfig()
	if err != nil {
		return nil, err
	}
	// SEC-19: the SAME gate the HTTP surface uses. This surface previously
	// skipped the IP check on bare test_mode with no live-PSP guard at all.
	if !webhookauth.CCBillIPAllowed(ctx, cfg, s.ccbillLivePSPProbe(), req.ClientIP) {
		log.WithFields(log.Fields{
			"client_ip":  req.ClientIP,
			"rail":       "ccbill",
			"event_type": req.EventType,
		}).Warn("CCBill webhook rejected - unauthorized IP address")

		return &WebhookResult{
			Accepted: false,
			Error:    "unauthorized webhook source",
		}, nil
	}

	prepared, err := webhookutil.PrepareCCBill(req.Body, req.EventType)
	if err != nil {
		if errors.Is(err, webhookutil.ErrWebhookEventTypeMissing) {
			return &WebhookResult{
				Accepted: false,
				Error:    "missing eventType parameter",
			}, nil
		}
		return &WebhookResult{
			Accepted: false,
			Error:    "invalid webhook payload",
		}, nil
	}
	if err := s.dispatchWebhook(ctx, prepared, req.ClientIP); err != nil {
		log.WithError(err).Error("ccbill webhook processing failed")
		return nil, fmt.Errorf("webhook processing failed: %w", err)
	}

	return &WebhookResult{
		Accepted:  true,
		EventType: prepared.EventType,
	}, nil
}

// ccbillLivePSPProbe binds the cross-merchant live-PSP catalog probe for the
// CCBill IP gate. nil (no merchants service on this runtime) fails it closed.
func (s *Service) ccbillLivePSPProbe() webhookauth.LiveRailProbe {
	if s == nil || s.rt == nil || s.rt.Merchants == nil {
		return nil
	}
	svc := s.rt.Merchants
	return func(ctx context.Context) (merchants.LiveRailPresence, error) {
		return svc.ProbeLiveRailPSPs(ctx, string(models.RailCCBill))
	}
}

func (s *Service) handleStripeWebhook(ctx context.Context, req HandleWebhookRequest) (*WebhookResult, error) {
	if _, err := s.requireConfig(); err != nil {
		return nil, err
	}
	secret := ""
	if s.rt != nil && s.rt.RailConfigs != nil {
		if proc, rerr := s.rt.RailConfigs.RailConfig(ctx, string(models.RailStripe), ""); rerr == nil && proc.Stripe != nil {
			secret = proc.Stripe.WebhookSigningSecret
		}
	}

	prepared, err := webhookutil.PrepareStripe(req.Body, secret, getHeaderValue(req.Headers, "Stripe-Signature"), 5*time.Minute)
	if err != nil {
		if errors.Is(err, webhookutil.ErrWebhookSignatureRequired) {
			return &WebhookResult{
				Accepted: false,
				Error:    "webhook signature required",
			}, nil
		}
		if errors.Is(err, webhookutil.ErrWebhookSignatureMissing) {
			return &WebhookResult{
				Accepted: false,
				Error:    "missing webhook signature",
			}, nil
		}
		if errors.Is(err, webhookutil.ErrWebhookSignatureInvalid) {
			return &WebhookResult{
				Accepted: false,
				Error:    "invalid webhook signature",
			}, nil
		}
		return &WebhookResult{
			Accepted: false,
			Error:    "invalid webhook payload",
		}, nil
	}
	if err := s.dispatchWebhook(ctx, prepared, req.ClientIP); err != nil {
		log.WithError(err).Error("stripe webhook processing failed")
		return nil, fmt.Errorf("webhook processing failed: %w", err)
	}

	return &WebhookResult{
		Accepted:  true,
		EventID:   prepared.EventID,
		EventType: prepared.EventType,
	}, nil
}

// dispatchWebhook runs the prepared event through the runtime dispatcher
// synchronously. Non-retryable (poison) errors are swallowed — accepted so
// the provider stops retrying; retryable errors surface to the caller.
func (s *Service) dispatchWebhook(ctx context.Context, prepared webhookutil.Prepared, clientIP string) error {
	rt, err := s.requireWebhookRuntime()
	if err != nil {
		return err
	}
	if rt.WebhookDispatcher == nil {
		return fmt.Errorf("webhook dispatcher unavailable")
	}
	msg := &webhooks.WebhookMessage{
		Rail:       prepared.Rail,
		EventID:    prepared.EventID,
		EventType:  prepared.EventType,
		Payload:    prepared.Body,
		IPAddress:  clientIP,
		Signature:  prepared.Signature,
		ReceivedAt: time.Now(),
	}
	if prepared.SignatureVerified {
		verified := true
		msg.SignatureValid = &verified
	}
	if err := rt.WebhookDispatcher.Process(ctx, msg); err != nil {
		if webhooks.IsWebhookErrorNonRetryable(err) {
			log.WithContext(ctx).WithError(err).WithFields(log.Fields{
				"rail": prepared.Rail, "event_id": prepared.EventID,
			}).Warn("webhook non-retryable error; acking to stop retries")
			return nil
		}
		return err
	}
	return nil
}
