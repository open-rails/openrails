package handlers

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/internal/processors"
	riverjobs "github.com/open-rails/openrails/internal/river"
	"github.com/open-rails/openrails/internal/services"
	"github.com/open-rails/openrails/internal/shared/webhookpayload"
	"github.com/open-rails/openrails/internal/shared/webhooksig"
	ipverify "github.com/open-rails/openrails/internal/utils"
	"github.com/riverqueue/river"
)

func Webhook(r *Request) {
	// Single path segment: /v1/webhooks/:provider
	// Provider can be: mobius, ccbill, solana
	// NMI is the gateway used by mobius, not a provider itself
	provider := webhookpayload.CanonicalProvider(r.Param("provider"))

	// Note: CCBill webhook IP allowlist checks are bypassed in test mode for developer ergonomics.
	// NMI/Mobius and Stripe signatures are still verified when secrets are configured.
	if r.State == nil || r.State.RiverProducer == nil {
		r.ErrorJSON(http.StatusInternalServerError, "job queue unavailable")
		return
	}

	clientIP := r.GetClientIP()

	log.WithFields(log.Fields{
		"provider":  provider,
		"client_ip": clientIP,
	}).Debug("Received webhook")

	// Use global test_mode for CCBill IP allowlist bypass.
	isTestMode := false
	if r.State != nil && r.State.Config != nil {
		isTestMode = r.State.Config.IsTestMode()
	} else if r.State == nil || r.State.Config == nil {
		log.Warn("State or Config is nil - defaulting to non-test mode for webhook processing")
	}

	// Route based on provider - NMI-backed processors go to handleNMIWebhook
	if processors.IsNMIBacked(provider) {
		if enqueueNMIWebhook(r, provider, clientIP) {
			r.SuccessJSON(map[string]string{"status": "accepted"})
		}
		return
	}

	switch provider {
	case services.ProcessorCCBill:
		// Check if in test mode - bypass authentication for testing
		if !isTestMode {
			// Verify CCBill webhook comes from authorized IP ranges
			if !ipverify.IsValidCCBillIP(clientIP) {
				log.WithFields(log.Fields{
					"client_ip":  clientIP,
					"processor":  "ccbill",
					"event_type": r.Query("eventType"),
				}).Warn("CCBill webhook rejected - unauthorized IP address")

				r.ErrorJSON(http.StatusForbidden, "Unauthorized webhook source")
				return
			}

			log.WithField("client_ip", clientIP).Debug("CCBill webhook authenticated - valid IP range")
		} else {
			log.WithField("client_ip", clientIP).Debug("CCBill webhook authentication bypassed - test mode enabled")
		}

		if enqueueCCBillWebhook(r, clientIP) {
			r.SuccessJSON(map[string]string{"status": "accepted"})
		}
		return
	case services.ProcessorStripe:
		if enqueueStripeWebhook(r, clientIP) {
			r.SuccessJSON(map[string]string{"status": "accepted"})
		}
		return
	default:
		r.ErrorJSON(http.StatusBadRequest, "Invalid provider")
		return
	}
}

func enqueueCCBillWebhook(r *Request, clientIP string) bool {
	body, err := readRequestBody(r.Request.Body)
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, "Failed to read request body")
		return false
	}

	body, err = webhookpayload.NormalizeCCBillPayload(body)
	if err != nil {
		r.ErrorJSON(http.StatusBadRequest, "Invalid webhook payload")
		return false
	}

	eventType := strings.TrimSpace(r.Query("eventType"))
	if eventType == "" {
		r.ErrorJSON(http.StatusBadRequest, "Missing eventType parameter")
		return false
	}

	uniqueKey := webhookpayload.ComputeUniqueKey(services.ProcessorCCBill, "", eventType, body)

	args := riverjobs.WebhookProcessArgs{
		Provider:  services.ProcessorCCBill,
		EventType: eventType,
		Body:      body,
		ClientIP:  clientIP,
		UniqueKey: uniqueKey,
	}

	if err := enqueueWebhookJob(r, args); err != nil {
		log.WithError(err).Error("failed to enqueue CCBill webhook")
		r.ErrorJSON(http.StatusInternalServerError, "Failed to enqueue webhook")
		return false
	}

	return true
}

func enqueueStripeWebhook(r *Request, clientIP string) bool {
	body, err := readRequestBody(r.Request.Body)
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, "Failed to read request body")
		return false
	}

	if r.State == nil || r.State.Config == nil {
		r.ErrorJSON(http.StatusInternalServerError, "Webhook processing unavailable")
		return false
	}

	secret := ""
	if stripeProc := r.State.Config.GetStripeProcessor(); stripeProc != nil {
		secret = stripeProc.WebhookSecret
	}
	sig := r.Request.Header.Get("Stripe-Signature")
	var signatureValidPtr *bool
	if secret == "" {
		r.ErrorJSON(http.StatusUnauthorized, "Webhook signature required")
		return false
	}
	if sig == "" {
		r.ErrorJSON(http.StatusUnauthorized, "Missing webhook signature")
		return false
	}
	if err = webhooksig.VerifyStripeSignature(secret, sig, body, 5*time.Minute); err != nil {
		r.ErrorJSON(http.StatusUnauthorized, "Invalid webhook signature")
		return false
	}
	truth := true
	signatureValidPtr = &truth

	eventID, eventType, err := webhookpayload.ParseStripeEventMeta(body)
	if err != nil {
		r.ErrorJSON(http.StatusBadRequest, "Invalid webhook payload")
		return false
	}

	uniqueKey := webhookpayload.ComputeUniqueKey(services.ProcessorStripe, eventID, eventType, body)

	args := riverjobs.WebhookProcessArgs{
		Provider:       services.ProcessorStripe,
		EventID:        eventID,
		EventType:      eventType,
		Body:           body,
		ClientIP:       clientIP,
		Signature:      sig,
		SignatureValid: signatureValidPtr,
		UniqueKey:      uniqueKey,
	}

	if err := enqueueWebhookJob(r, args); err != nil {
		log.WithError(err).Error("failed to enqueue Stripe webhook")
		r.ErrorJSON(http.StatusInternalServerError, "Failed to enqueue webhook")
		return false
	}

	return true
}

func enqueueWebhookJob(r *Request, args riverjobs.WebhookProcessArgs) error {
	if r.State == nil || r.State.RiverProducer == nil {
		return fmt.Errorf("river producer unavailable")
	}

	opts := &river.InsertOpts{
		Queue: riverjobs.QueueWebhooks,
		UniqueOpts: river.UniqueOpts{
			ByArgs:  true,
			ByQueue: true,
		},
	}

	_, err := r.State.RiverProducer.Insert(r.Request.Context(), args, opts)
	return err
}

func enqueueNMIWebhook(r *Request, provider string, clientIP string) bool {
	// Read the request body for signature verification
	body, err := readRequestBody(r.Request.Body)
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, "Failed to read request body")
		return false
	}

	providerKey := webhookpayload.CanonicalProvider(provider)

	client, ok := r.State.NMIClients[providerKey]
	if !ok || client == nil {
		r.ErrorJSON(http.StatusNotFound, fmt.Sprintf("unknown nmi provider '%s'", providerKey))
		return false
	}

	signatureValidated := false
	signingKey := client.GetWebhookSecret()

	if signingKey == "" {
		log.Error("NMI webhook secret not configured")
		r.ErrorJSON(http.StatusUnauthorized, "Missing webhook signature")
		return false
	}

	signature, err := webhooksig.ValidateNMISignature(
		signingKey,
		body,
		r.Request.Header.Get("Webhook-Signature"),
		[]string{
			r.Request.Header.Get("X-Signature"),
			r.Request.Header.Get("X-NMI-Signature"),
			r.Request.Header.Get("X-Mobius-Signature"),
		},
		func(signature string) error { return client.VerifyWebhookSignature(body, signature) },
	)
	if err != nil {
		if err == webhooksig.ErrNMIWebhookSecretMissing || err == webhooksig.ErrNMIWebhookSignatureMissing {
			log.WithError(err).Error("Missing webhook signature for NMI webhook")
			r.ErrorJSON(http.StatusUnauthorized, "Missing webhook signature")
			return false
		}
		log.WithError(err).Error("NMI webhook signature verification failed")
		r.ErrorJSON(http.StatusUnauthorized, "Invalid webhook signature")
		return false
	}
	signatureValidated = true

	var data services.NMIWebhookEvent
	if err := json.Unmarshal(body, &data); err != nil {
		log.WithError(err).Error("failed to parse NMI webhook JSON")
		r.ErrorJSON(http.StatusBadRequest, "Invalid JSON data")
		return false
	}
	if data.EventID == "" {
		r.ErrorJSON(http.StatusBadRequest, "Missing event_id in payload")
		return false
	}

	var signatureValidPtr *bool
	if signatureValidated {
		truth := true
		signatureValidPtr = &truth
	}

	uniqueKey := webhookpayload.ComputeUniqueKey(providerKey, data.EventID, string(data.EventType), body)

	args := riverjobs.WebhookProcessArgs{
		Provider:       providerKey,
		EventID:        data.EventID,
		EventType:      string(data.EventType),
		Body:           body,
		ClientIP:       clientIP,
		Signature:      signature,
		SignatureValid: signatureValidPtr,
		UniqueKey:      uniqueKey,
	}

	if err := enqueueWebhookJob(r, args); err != nil {
		log.WithError(err).Error("failed to enqueue NMI webhook")
		r.ErrorJSON(http.StatusInternalServerError, "Failed to enqueue webhook")
		return false
	}

	return true
}

func readRequestBody(body io.ReadCloser) ([]byte, error) {
	if body == nil {
		return []byte{}, nil
	}
	defer body.Close()
	return io.ReadAll(body)
}
