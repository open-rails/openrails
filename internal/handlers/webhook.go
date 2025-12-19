package handlers

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	log "github.com/sirupsen/logrus"

	"github.com/doujins-org/doujins-billing/internal/app"
	"github.com/doujins-org/doujins-billing/internal/processors"
	"github.com/doujins-org/doujins-billing/internal/services"
	ipverify "github.com/doujins-org/doujins-billing/internal/utils"
)

func Webhook(r *Request) {
	// Single path segment: /v1/webhooks/:provider
	// Provider can be: mobius, ccbill, solana
	// NMI is the gateway used by mobius, not a provider itself
	provider := strings.Trim(strings.ToLower(r.Param("provider")), " /")

	// Normalize legacy "nmi" provider to "mobius"
	if provider == "nmi" {
		provider = "mobius"
	}

	// NOTE: Webhook authentication can be bypassed for testing by setting test_mode: true
	// in the respective processor config (nmi.test_mode or ccbill.test_mode)
	// This is useful for integration tests and development environments

	// Create dead letter service
	deadLetterService := &services.DeadLetterService{
		DB:                  r.State.DB,
		NotificationService: r.State.NotificationService,
	}

	// Capture request headers for dead letter logging
	headers := make(map[string]string)
	for key, values := range r.Request.Header {
		if len(values) > 0 {
			headers[key] = values[0]
		}
	}
	clientIP := r.GetClientIP()

	log.WithFields(log.Fields{
		"provider":  provider,
		"client_ip": clientIP,
	}).Debug("Received webhook")

	ccbillTestMode := false
	if r.State != nil && r.State.CCBillRESTClient != nil {
		if cfg := r.State.CCBillRESTClient.Config(); cfg != nil {
			ccbillTestMode = cfg.TestMode
		}
	}

	// Route based on provider - NMI-backed processors go to handleNMIWebhook
	if processors.IsNMIBacked(provider) {
		handleNMIWebhook(r, provider, headers, clientIP)
		return
	}

	switch provider {
	case services.ProcessorCCBill:
		// Check if CCBill is in test mode - bypass authentication for testing
		if !ccbillTestMode {
			// Verify CCBill webhook comes from authorized IP ranges
			if !ipverify.IsValidCCBillIP(clientIP) {
				log.WithFields(log.Fields{
					"client_ip":  clientIP,
					"processor":  "ccbill",
					"event_type": r.Query("eventType"),
				}).Warn("CCBill webhook rejected - unauthorized IP address")

				// Log the security violation for monitoring
				deadLetterService.LogInvalidPayload(
					context.Background(),
					"ccbill",
					[]byte(fmt.Sprintf("Unauthorized IP: %s", clientIP)),
					fmt.Errorf("webhook from unauthorized IP address: %s", clientIP),
					headers,
					clientIP,
				)

				r.ErrorJSON(http.StatusForbidden, "Unauthorized webhook source")
				return
			}

			log.WithField("client_ip", clientIP).Debug("CCBill webhook authenticated - valid IP range")
		} else {
			log.WithField("client_ip", clientIP).Debug("CCBill webhook authentication bypassed - test mode enabled")
		}

		body, err := readRequestBody(r.Request.Body)
		if err != nil {
			deadLetterService.LogInvalidPayload(context.Background(), "ccbill", nil, err, headers, clientIP)
			r.ErrorJSON(http.StatusInternalServerError, "Failed to read request body")
			return
		}

		body, err = normalizeCCBillPayload(body)
		if err != nil {
			deadLetterService.LogInvalidPayload(context.Background(), "ccbill", body, err, headers, clientIP)
			r.ErrorJSON(http.StatusBadRequest, "Invalid webhook payload")
			return
		}

		eventType := strings.TrimSpace(r.Query("eventType"))
		if eventType == "" {
			r.ErrorJSON(http.StatusBadRequest, "Missing eventType parameter")
			return
		}

		if r.State == nil || r.State.WebhookEventService == nil || r.State.WebhookProcessor == nil {
			log.Error("Webhook components not configured; unable to persist event")
			r.ErrorJSON(http.StatusInternalServerError, "Webhook processing unavailable")
			return
		}

		eventHeaders := copyHeaders(headers)
		if provider != "" {
			eventHeaders["x-internal-provider"] = provider
		}

		ctx := r.Request.Context()
		event, err := r.State.WebhookEventService.Create(ctx, services.CreateWebhookEventParams{
			Processor: services.ProcessorCCBill,
			EventType: eventType,
			Payload:   body,
			Headers:   eventHeaders,
			IPAddress: clientIP,
		})
		if err != nil {
			log.WithError(err).Error("failed to persist CCBill webhook event")
			r.ErrorJSON(http.StatusInternalServerError, "Failed to persist webhook event")
			return
		}

		if err := processWebhookSync(ctx, r.State, event.ID); err != nil {
			log.WithError(err).Error("CCBill webhook processing failed")
		}

		r.SuccessJSON(map[string]string{"status": "accepted"})
		return
	case services.ProcessorStripe:
		body, err := readRequestBody(r.Request.Body)
		if err != nil {
			deadLetterService.LogInvalidPayload(context.Background(), "stripe", nil, err, headers, clientIP)
			r.ErrorJSON(http.StatusInternalServerError, "Failed to read request body")
			return
		}

		if r.State == nil || r.State.Config == nil || r.State.WebhookEventService == nil || r.State.WebhookProcessor == nil {
			log.Error("Webhook components not configured; unable to persist event")
			r.ErrorJSON(http.StatusInternalServerError, "Webhook processing unavailable")
			return
		}

		isDev := true
		if r.State.Config != nil {
			env := strings.TrimSpace(strings.ToLower(r.State.Config.Env))
			isDev = env == "" || env == "dev" || env == "development"
		}

		secret := ""
		if r.State.Config.Stripe != nil {
			secret = r.State.Config.Stripe.WebhookSecret
		}
		sig := r.Request.Header.Get("Stripe-Signature")
		var signatureValidPtr *bool
		if secret != "" {
			if sig == "" {
				deadLetterService.LogAuthenticationFailure(context.Background(), "stripe", body, fmt.Errorf("missing stripe signature"), headers, clientIP)
				r.ErrorJSON(http.StatusUnauthorized, "Missing webhook signature")
				return
			}
			if err := verifyStripeSignature(secret, sig, body, 5*time.Minute); err != nil {
				deadLetterService.LogAuthenticationFailure(context.Background(), "stripe", body, err, headers, clientIP)
				r.ErrorJSON(http.StatusUnauthorized, "Invalid webhook signature")
				return
			}
			truth := true
			signatureValidPtr = &truth
		} else {
			if !isDev {
				deadLetterService.LogAuthenticationFailure(context.Background(), "stripe", body, fmt.Errorf("missing stripe webhook secret"), headers, clientIP)
				r.ErrorJSON(http.StatusUnauthorized, "Webhook signature required")
				return
			}
			log.Warn("Stripe webhook secret not configured; signature verification disabled")
		}

		eventID, eventType, err := parseStripeEventMeta(body)
		if err != nil {
			deadLetterService.LogInvalidPayload(context.Background(), "stripe", body, err, headers, clientIP)
			r.ErrorJSON(http.StatusBadRequest, "Invalid webhook payload")
			return
		}

		ctx := r.Request.Context()
		event, err := r.State.WebhookEventService.Create(ctx, services.CreateWebhookEventParams{
			Processor:      services.ProcessorStripe,
			EventID:        eventID,
			EventType:      eventType,
			Payload:        body,
			Headers:        headers,
			IPAddress:      clientIP,
			Signature:      sig,
			SignatureValid: signatureValidPtr,
		})
		if err != nil {
			log.WithError(err).Error("failed to persist Stripe webhook event")
			r.ErrorJSON(http.StatusInternalServerError, "Failed to persist webhook event")
			return
		}
		if err := processWebhookSync(ctx, r.State, event.ID); err != nil {
			log.WithError(err).Error("Stripe webhook processing failed")
		}

		r.SuccessJSON(map[string]string{"status": "accepted"})
		return
	default:
		webhookBody, readErr := readRequestBody(r.Request.Body)
		if readErr != nil {
			log.WithError(readErr).WithField("provider", provider).Warn("Failed to read body for unknown webhook provider")
		}
		// Log unknown provider to dead letter queue
		deadLetterService.LogUnknownEvent(context.Background(), provider, "unknown", json.RawMessage(webhookBody), headers, clientIP)
		r.ErrorJSON(http.StatusBadRequest, "Invalid provider")
		return
	}
}

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

func handleNMIWebhook(r *Request, provider string, headers map[string]string, clientIP string) {
	// Read the request body for signature verification
	body, err := readRequestBody(r.Request.Body)
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, "Failed to read request body")
		return
	}

	providerKey := strings.TrimSpace(strings.ToLower(provider))
	if providerKey == "" {
		providerKey = "mobius"
	}

	client, ok := r.State.NMIClients[providerKey]
	if !ok || client == nil {
		r.ErrorJSON(http.StatusNotFound, fmt.Sprintf("unknown nmi provider '%s'", providerKey))
		return
	}

	isDev := true
	if r.State != nil && r.State.Config != nil {
		env := strings.TrimSpace(strings.ToLower(r.State.Config.Env))
		isDev = env == "" || env == "dev" || env == "development"
	}

	signature := ""
	signatureValidated := false
	if !client.Config().TestMode {
		if client.GetWebhookSecret() != "" {
			signature = r.Request.Header.Get("X-Signature")
			if signature == "" {
				signature = r.Request.Header.Get("X-NMI-Signature")
			}
			if signature == "" {
				signature = r.Request.Header.Get("X-Mobius-Signature")
			}
			if signature == "" {
				log.Error("Missing webhook signature for NMI webhook")
				r.ErrorJSON(http.StatusUnauthorized, "Missing webhook signature")
				return
			}
			if err := client.VerifyWebhookSignature(body, signature); err != nil {
				log.WithError(err).Error("NMI webhook signature verification failed")
				r.ErrorJSON(http.StatusUnauthorized, "Invalid webhook signature")
				return
			}
			signatureValidated = true
		} else {
			if !isDev {
				log.Error("NMI webhook secret not configured")
				r.ErrorJSON(http.StatusUnauthorized, "Missing webhook signature")
				return
			}
			log.Warn("NMI webhook secret not configured - skipping signature verification")
		}
	} else {
		log.Debug("NMI webhook authentication bypassed - test mode enabled")
	}

	var data services.NMIWebhookEvent
	if err := json.Unmarshal(body, &data); err != nil {
		log.WithError(err).Error("failed to parse NMI webhook JSON")
		r.ErrorJSON(http.StatusBadRequest, "Invalid JSON data")
		return
	}
	if data.EventID == "" {
		r.ErrorJSON(http.StatusBadRequest, "Missing event_id in payload")
		return
	}

	if r.State == nil || r.State.WebhookEventService == nil || r.State.WebhookProcessor == nil {
		log.Error("Webhook components not configured; unable to persist NMI event")
		r.ErrorJSON(http.StatusInternalServerError, "Webhook processing unavailable")
		return
	}

	ctx := r.Request.Context()
	eventHeaders := copyHeaders(headers)
	eventHeaders["x-internal-provider"] = providerKey

	var signatureValidPtr *bool
	if signatureValidated {
		truth := true
		signatureValidPtr = &truth
	}

	params := services.CreateWebhookEventParams{
		Processor:      providerKey, // Use the actual processor name (e.g., "mobius")
		EventID:        data.EventID,
		EventType:      string(data.EventType),
		Payload:        body,
		Headers:        eventHeaders,
		IPAddress:      clientIP,
		Signature:      signature,
		SignatureValid: signatureValidPtr,
	}
	if event, err := r.State.WebhookEventService.Create(ctx, params); err != nil {
		log.WithError(err).Error("failed to persist NMI webhook event")
		r.ErrorJSON(http.StatusInternalServerError, "Failed to persist webhook event")
		return
	} else {
		if err := processWebhookSync(ctx, r.State, event.ID); err != nil {
			log.WithError(err).Error("NMI webhook processing failed")
		}
	}

	r.SuccessJSON(map[string]string{"status": "accepted"})
}

func readRequestBody(body io.ReadCloser) ([]byte, error) {
	if body == nil {
		return []byte{}, nil
	}
	defer body.Close()
	return io.ReadAll(body)
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

func copyHeaders(src map[string]string) map[string]string {
	if src == nil {
		return map[string]string{}
	}
	dst := make(map[string]string, len(src))
	for k, v := range src {
		dst[k] = v
	}
	return dst
}

// processWebhookSync processes a webhook event synchronously.
// Webhook processing is now synchronous-only - no async retry mechanism.
// Payment processors (CCBill, NMI) will retry failed webhooks from their end.
func processWebhookSync(ctx context.Context, runtime *app.Runtime, eventID uuid.UUID) error {
	if runtime == nil || runtime.WebhookProcessor == nil {
		return fmt.Errorf("webhook processor unavailable")
	}
	return runtime.WebhookProcessor.Process(ctx, eventID)
}
