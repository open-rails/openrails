package handlers

import (
	"bytes"
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

	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/internal/processors"
	riverjobs "github.com/open-rails/openrails/internal/river"
	"github.com/open-rails/openrails/internal/services"
	ipverify "github.com/open-rails/openrails/internal/utils"
	"github.com/riverqueue/river"
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

func enqueueCCBillWebhook(r *Request, clientIP string) bool {
	body, err := readRequestBody(r.Request.Body)
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, "Failed to read request body")
		return false
	}

	body, err = normalizeCCBillPayload(body)
	if err != nil {
		r.ErrorJSON(http.StatusBadRequest, "Invalid webhook payload")
		return false
	}

	eventType := strings.TrimSpace(r.Query("eventType"))
	if eventType == "" {
		r.ErrorJSON(http.StatusBadRequest, "Missing eventType parameter")
		return false
	}

	uniqueKey := computeWebhookUniqueKey(services.ProcessorCCBill, "", eventType, body)

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
	if err = verifyStripeSignature(secret, sig, body, 5*time.Minute); err != nil {
		r.ErrorJSON(http.StatusUnauthorized, "Invalid webhook signature")
		return false
	}
	truth := true
	signatureValidPtr = &truth

	eventID, eventType, err := parseStripeEventMeta(body)
	if err != nil {
		r.ErrorJSON(http.StatusBadRequest, "Invalid webhook payload")
		return false
	}

	uniqueKey := computeWebhookUniqueKey(services.ProcessorStripe, eventID, eventType, body)

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

func computeWebhookUniqueKey(provider, eventID, eventType string, body []byte) string {
	provider = strings.TrimSpace(strings.ToLower(provider))
	eventID = strings.TrimSpace(eventID)
	if eventID != "" {
		return fmt.Sprintf("webhook:%s:%s", provider, eventID)
	}
	hash := sha256.Sum256(append([]byte(provider+"|"+eventType+"|"), body...))
	return fmt.Sprintf("webhook:%s:%s", provider, hex.EncodeToString(hash[:8]))
}

func enqueueNMIWebhook(r *Request, provider string, clientIP string) bool {
	// Read the request body for signature verification
	body, err := readRequestBody(r.Request.Body)
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, "Failed to read request body")
		return false
	}

	providerKey := strings.TrimSpace(strings.ToLower(provider))
	if providerKey == "" {
		providerKey = "mobius"
	}

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

	// Try PHP-style signature verification if Webhook-Signature header is present
	sigHeader := r.Request.Header.Get("Webhook-Signature")
	signature := sigHeader
	if sigHeader != "" {
		if err := verifyNMIWebhookSignature(signingKey, sigHeader, body); err != nil {
			log.WithError(err).Error("NMI webhook signature verification failed")
			r.ErrorJSON(http.StatusUnauthorized, "Invalid webhook signature")
			return false
		}
		log.Info("Webhook signature verified successfully")
		signatureValidated = true
	} else {
		// Fallback to legacy signature headers
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
			return false
		}
		if err := client.VerifyWebhookSignature(body, signature); err != nil {
			log.WithError(err).Error("NMI webhook signature verification failed")
			r.ErrorJSON(http.StatusUnauthorized, "Invalid webhook signature")
			return false
		}
		signatureValidated = true
	}

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

	uniqueKey := computeWebhookUniqueKey(providerKey, data.EventID, string(data.EventType), body)

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
