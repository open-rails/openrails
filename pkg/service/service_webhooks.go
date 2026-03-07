// Package service provides the in-process billing API for embedded hosts.
package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/internal/processors"
	riverjobs "github.com/open-rails/openrails/internal/river"
	"github.com/open-rails/openrails/internal/services"
	"github.com/open-rails/openrails/internal/shared/webhookutil"
	ipverify "github.com/open-rails/openrails/internal/utils"
	"github.com/riverqueue/river"
)

// HandleWebhook processes an incoming webhook from a payment processor.
// It validates the signature, parses the payload, and enqueues a job for async processing.
func (s *Service) HandleWebhook(ctx context.Context, req HandleWebhookRequest) (*WebhookResult, error) {
	provider := webhookutil.CanonicalProvider(req.Provider)

	if s.rt == nil || s.rt.RiverProducer == nil {
		return nil, fmt.Errorf("job queue unavailable")
	}

	log.WithFields(log.Fields{
		"provider":  provider,
		"client_ip": req.ClientIP,
	}).Debug("Received webhook via Service API")

	// Route based on provider
	if processors.IsNMIBacked(provider) {
		return s.handleNMIWebhook(ctx, provider, req)
	}

	switch provider {
	case services.ProcessorCCBill:
		return s.handleCCBillWebhook(ctx, req)
	case services.ProcessorStripe:
		return s.handleStripeWebhook(ctx, req)
	default:
		return &WebhookResult{
			Accepted: false,
			Error:    fmt.Sprintf("invalid provider: %s", provider),
		}, nil
	}
}

func (s *Service) handleNMIWebhook(ctx context.Context, provider string, req HandleWebhookRequest) (*WebhookResult, error) {
	providerKey := strings.TrimSpace(strings.ToLower(provider))
	if providerKey == "" {
		providerKey = "mobius"
	}

	client, ok := s.rt.NMIClients[providerKey]
	if !ok || client == nil {
		return &WebhookResult{
			Accepted: false,
			Error:    fmt.Sprintf("unknown nmi provider '%s'", providerKey),
		}, nil
	}

	signature, err := webhookutil.ValidateNMISignature(client.GetWebhookSecret(), req.Body, getHeaderValue(req.Headers, "Webhook-Signature"), []string{
		getHeaderValue(req.Headers, "X-Signature"),
		getHeaderValue(req.Headers, "X-NMI-Signature"),
		getHeaderValue(req.Headers, "X-Mobius-Signature"),
	}, func(signature string) error {
		return client.VerifyWebhookSignature(req.Body, signature)
	})
	if err != nil {
		if errors.Is(err, webhookutil.ErrNMIWebhookSecretMissing) || errors.Is(err, webhookutil.ErrNMIWebhookSignatureMissing) {
			log.WithError(err).Error("Missing webhook signature for NMI webhook")
			return &WebhookResult{
				Accepted: false,
				Error:    "missing webhook signature",
			}, nil
		}

		log.WithError(err).Error("NMI webhook signature verification failed")
		return &WebhookResult{
			Accepted: false,
			Error:    "invalid webhook signature",
		}, nil
	}

	var data services.NMIWebhookEvent
	if err := json.Unmarshal(req.Body, &data); err != nil {
		log.WithError(err).Error("failed to parse NMI webhook JSON")
		return &WebhookResult{
			Accepted: false,
			Error:    "invalid JSON data",
		}, nil
	}
	if data.EventID == "" {
		return &WebhookResult{
			Accepted: false,
			Error:    "missing event_id in payload",
		}, nil
	}

	truth := true
	signatureValidPtr := &truth

	uniqueKey := webhookutil.ComputeUniqueKey(providerKey, data.EventID, string(data.EventType), req.Body)

	args := riverjobs.WebhookProcessArgs{
		Provider:       providerKey,
		EventID:        data.EventID,
		EventType:      string(data.EventType),
		Body:           req.Body,
		ClientIP:       req.ClientIP,
		Signature:      signature,
		SignatureValid: signatureValidPtr,
		UniqueKey:      uniqueKey,
	}

	if err := s.enqueueWebhookJob(ctx, args); err != nil {
		log.WithError(err).Error("failed to enqueue NMI webhook")
		return nil, fmt.Errorf("failed to enqueue webhook: %w", err)
	}

	return &WebhookResult{
		Accepted:  true,
		EventID:   data.EventID,
		EventType: string(data.EventType),
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
	// Use global test_mode for CCBill IP allowlist bypass.
	isTestMode := s.rt.Config.IsTestMode()

	if !isTestMode {
		// Verify CCBill webhook comes from authorized IP ranges
		if !ipverify.IsValidCCBillIP(req.ClientIP) {
			log.WithFields(log.Fields{
				"client_ip":  req.ClientIP,
				"processor":  "ccbill",
				"event_type": req.EventType,
			}).Warn("CCBill webhook rejected - unauthorized IP address")

			return &WebhookResult{
				Accepted: false,
				Error:    "unauthorized webhook source",
			}, nil
		}
		log.WithField("client_ip", req.ClientIP).Debug("CCBill webhook authenticated - valid IP range")
	} else {
		log.WithField("client_ip", req.ClientIP).Debug("CCBill webhook authentication bypassed - test mode enabled")
	}

	body, err := webhookutil.NormalizeCCBillPayload(req.Body)
	if err != nil {
		return &WebhookResult{
			Accepted: false,
			Error:    "invalid webhook payload",
		}, nil
	}

	eventType := strings.TrimSpace(req.EventType)
	if eventType == "" {
		return &WebhookResult{
			Accepted: false,
			Error:    "missing eventType parameter",
		}, nil
	}

	uniqueKey := webhookutil.ComputeUniqueKey(services.ProcessorCCBill, "", eventType, body)

	args := riverjobs.WebhookProcessArgs{
		Provider:  services.ProcessorCCBill,
		EventType: eventType,
		Body:      body,
		ClientIP:  req.ClientIP,
		UniqueKey: uniqueKey,
	}

	if err := s.enqueueWebhookJob(ctx, args); err != nil {
		log.WithError(err).Error("failed to enqueue CCBill webhook")
		return nil, fmt.Errorf("failed to enqueue webhook: %w", err)
	}

	return &WebhookResult{
		Accepted:  true,
		EventType: eventType,
	}, nil
}

func (s *Service) handleStripeWebhook(ctx context.Context, req HandleWebhookRequest) (*WebhookResult, error) {
	secret := ""
	if stripeProc := s.rt.Config.GetStripeProcessor(); stripeProc != nil {
		secret = stripeProc.WebhookSecret
	}

	sig := req.Headers["Stripe-Signature"]
	var signatureValidPtr *bool
	if secret == "" {
		return &WebhookResult{
			Accepted: false,
			Error:    "webhook signature required",
		}, nil
	}
	if sig == "" {
		return &WebhookResult{
			Accepted: false,
			Error:    "missing webhook signature",
		}, nil
	}
	if err := webhookutil.VerifyStripeSignature(secret, sig, req.Body, 5*time.Minute); err != nil {
		return &WebhookResult{
			Accepted: false,
			Error:    "invalid webhook signature",
		}, nil
	}
	truth := true
	signatureValidPtr = &truth

	eventID, eventType, err := webhookutil.ParseStripeEventMeta(req.Body)
	if err != nil {
		return &WebhookResult{
			Accepted: false,
			Error:    "invalid webhook payload",
		}, nil
	}

	uniqueKey := webhookutil.ComputeUniqueKey(services.ProcessorStripe, eventID, eventType, req.Body)

	args := riverjobs.WebhookProcessArgs{
		Provider:       services.ProcessorStripe,
		EventID:        eventID,
		EventType:      eventType,
		Body:           req.Body,
		ClientIP:       req.ClientIP,
		Signature:      sig,
		SignatureValid: signatureValidPtr,
		UniqueKey:      uniqueKey,
	}

	if err := s.enqueueWebhookJob(ctx, args); err != nil {
		log.WithError(err).Error("failed to enqueue Stripe webhook")
		return nil, fmt.Errorf("failed to enqueue webhook: %w", err)
	}

	return &WebhookResult{
		Accepted:  true,
		EventID:   eventID,
		EventType: eventType,
	}, nil
}

func (s *Service) enqueueWebhookJob(ctx context.Context, args riverjobs.WebhookProcessArgs) error {
	if s.rt == nil || s.rt.RiverProducer == nil {
		return fmt.Errorf("river producer unavailable")
	}

	opts := &river.InsertOpts{
		Queue: riverjobs.QueueWebhooks,
		UniqueOpts: river.UniqueOpts{
			ByArgs:  true,
			ByQueue: true,
		},
	}

	_, err := s.rt.RiverProducer.Insert(ctx, args, opts)
	return err
}
