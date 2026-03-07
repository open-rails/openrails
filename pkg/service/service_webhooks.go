// Package service provides the in-process billing API for embedded hosts.
package service

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/internal/processors"
	riverjobs "github.com/open-rails/openrails/internal/river"
	"github.com/open-rails/openrails/internal/services"
	ipverify "github.com/open-rails/openrails/internal/utils"
	"github.com/riverqueue/river"
)

var (
	errNMIWebhookSecretMissing    = errors.New("nmi webhook secret not configured")
	errNMIWebhookSignatureMissing = errors.New("missing webhook signature")
	errNMIWebhookSignatureInvalid = errors.New("invalid webhook signature")
)

// HandleWebhook processes an incoming webhook from a payment processor.
// It validates the signature, parses the payload, and enqueues a job for async processing.
func (s *Service) HandleWebhook(ctx context.Context, req HandleWebhookRequest) (*WebhookResult, error) {
	provider := strings.Trim(strings.ToLower(req.Provider), " /")

	// Normalize legacy "nmi" provider to "mobius"
	if provider == "nmi" {
		provider = "mobius"
	}

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

	signature, err := validateNMIWebhookSignature(client.GetWebhookSecret(), req.Body, req.Headers, func(signature string) error {
		return client.VerifyWebhookSignature(req.Body, signature)
	})
	if err != nil {
		if errors.Is(err, errNMIWebhookSecretMissing) || errors.Is(err, errNMIWebhookSignatureMissing) {
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

	uniqueKey := computeWebhookUniqueKey(providerKey, data.EventID, string(data.EventType), req.Body)

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

func validateNMIWebhookSignature(secret string, body []byte, headers map[string]string, verifyLegacy func(signature string) error) (string, error) {
	if strings.TrimSpace(secret) == "" {
		return "", errNMIWebhookSecretMissing
	}

	signature, phpStyle := extractNMIWebhookSignature(headers)
	if signature == "" {
		return "", errNMIWebhookSignatureMissing
	}

	if phpStyle {
		if err := verifyNMIWebhookSignature(secret, signature, body); err != nil {
			return "", fmt.Errorf("%w: %v", errNMIWebhookSignatureInvalid, err)
		}
		return signature, nil
	}

	if err := verifyLegacy(signature); err != nil {
		return "", fmt.Errorf("%w: %v", errNMIWebhookSignatureInvalid, err)
	}

	return signature, nil
}

func extractNMIWebhookSignature(headers map[string]string) (string, bool) {
	signature := getHeaderValue(headers, "Webhook-Signature")
	if signature != "" {
		return signature, true
	}

	return getHeaderValue(headers, "X-Signature", "X-NMI-Signature", "X-Mobius-Signature"), false
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

	body, err := normalizeCCBillPayload(req.Body)
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

	uniqueKey := computeWebhookUniqueKey(services.ProcessorCCBill, "", eventType, body)

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
	if err := verifyStripeSignature(secret, sig, req.Body, 5*time.Minute); err != nil {
		return &WebhookResult{
			Accepted: false,
			Error:    "invalid webhook signature",
		}, nil
	}
	truth := true
	signatureValidPtr = &truth

	eventID, eventType, err := parseStripeEventMeta(req.Body)
	if err != nil {
		return &WebhookResult{
			Accepted: false,
			Error:    "invalid webhook payload",
		}, nil
	}

	uniqueKey := computeWebhookUniqueKey(services.ProcessorStripe, eventID, eventType, req.Body)

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

// Helper functions (duplicated from handlers to avoid import cycles)

func parseStripeEventMeta(body []byte) (string, string, error) {
	var payload struct {
		ID   string `json:"id"`
		Type string `json:"type"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return "", "", err
	}
	if strings.TrimSpace(payload.ID) == "" || strings.TrimSpace(payload.Type) == "" {
		return "", "", fmt.Errorf("missing event id or type")
	}
	return payload.ID, payload.Type, nil
}

func verifyStripeSignature(secret, header string, body []byte, tolerance time.Duration) error {
	timestamp, signatures := parseStripeSignatureHeader(header)
	if timestamp == "" || len(signatures) == 0 {
		return fmt.Errorf("invalid stripe signature header")
	}
	tsInt, err := strconv.ParseInt(timestamp, 10, 64)
	if err != nil {
		return fmt.Errorf("invalid stripe signature timestamp")
	}
	if tolerance > 0 {
		now := time.Now().Unix()
		if now-tsInt > int64(tolerance.Seconds()) || tsInt-now > int64(tolerance.Seconds()) {
			return fmt.Errorf("stripe signature timestamp outside tolerance")
		}
	}
	signedPayload := fmt.Sprintf("%s.%s", timestamp, string(body))
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte(signedPayload))
	expected := hex.EncodeToString(mac.Sum(nil))
	for _, sig := range signatures {
		if hmac.Equal([]byte(expected), []byte(sig)) {
			return nil
		}
	}
	return fmt.Errorf("stripe signature mismatch")
}

func parseStripeSignatureHeader(header string) (string, []string) {
	parts := strings.Split(header, ",")
	var ts string
	sigs := make([]string, 0)
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if strings.HasPrefix(part, "t=") {
			ts = strings.TrimPrefix(part, "t=")
		} else if strings.HasPrefix(part, "v1=") {
			sigs = append(sigs, strings.TrimPrefix(part, "v1="))
		}
	}
	return ts, sigs
}

func verifyNMIWebhookSignature(secret, header string, body []byte) error {
	timestamp, signature, err := parseNMIWebhookSignatureHeader(header)
	if err != nil {
		return err
	}

	mac := hmac.New(sha256.New, []byte(secret))
	dataToSign := timestamp + "." + string(body)
	_, _ = mac.Write([]byte(dataToSign))
	expectedSig := hex.EncodeToString(mac.Sum(nil))
	if !hmac.Equal([]byte(signature), []byte(expectedSig)) {
		return fmt.Errorf("invalid webhook signature")
	}

	return nil
}

func parseNMIWebhookSignatureHeader(header string) (string, string, error) {
	var ts string
	var sig string

	parts := strings.Split(header, ",")
	for _, part := range parts {
		kv := strings.SplitN(strings.TrimSpace(part), "=", 2)
		if len(kv) != 2 {
			continue
		}

		switch strings.TrimSpace(kv[0]) {
		case "t":
			ts = strings.TrimSpace(kv[1])
		case "s":
			sig = strings.TrimSpace(kv[1])
		}
	}

	if ts == "" || sig == "" {
		return "", "", fmt.Errorf("unrecognized webhook signature format")
	}

	return ts, sig, nil
}

func computeWebhookUniqueKey(provider, eventID, eventType string, body []byte) string {
	provider = strings.TrimSpace(strings.ToLower(provider))
	eventID = strings.TrimSpace(eventID)
	if eventID != "" {
		return fmt.Sprintf("webhook:%s:%s", provider, eventID)
	}
	hash := sha256.Sum256(append([]byte(provider+"|"+eventType+"|"), body...))
	return fmt.Sprintf("webhook:%s:%s", provider, hex.EncodeToString(hash[:8]))
}

func normalizeCCBillPayload(body []byte) ([]byte, error) {
	body = bytes.TrimSpace(body)
	if len(body) == 0 {
		return body, nil
	}
	if body[0] == '{' || body[0] == '[' {
		return body, nil
	}
	values, err := url.ParseQuery(string(body))
	if err != nil {
		return nil, err
	}
	payload := make(map[string]string, len(values))
	for key, val := range values {
		if len(val) > 0 {
			payload[key] = val[0]
		}
	}
	return json.Marshal(payload)
}
