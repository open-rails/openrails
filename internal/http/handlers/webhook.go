package handlers

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/open-rails/openrails/internal/db/repo"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/integrations/stripeapi"
	"github.com/open-rails/openrails/internal/modules/funding"
	"github.com/open-rails/openrails/internal/modules/payments/processors"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/internal/modules/webhooks"
	riverjobs "github.com/open-rails/openrails/internal/river"
	"github.com/open-rails/openrails/internal/shared/iputil"
	"github.com/open-rails/openrails/internal/shared/webhookutil"
	"github.com/open-rails/openrails/pkg/api"
	"github.com/riverqueue/river"
	log "github.com/sirupsen/logrus"
)

// Per-processor webhook body caps. The primary memory-exhaustion fix is the
// global 1 MiB BodyLimit now applying to webhook routes (the blanket exemption
// was removed); these per-processor caps are tighter defense-in-depth. They are
// sized with headroom above real payloads to avoid 413-ing legitimate webhooks:
//   - CCBill background posts are form-encoded but carry many customer/transaction
//     fields, so 16 KiB rather than a couple KiB.
//   - Stripe "snapshot" events embed the full object (subscriptions, invoices with
//     line items) and can be tens of KiB, so 256 KiB.
//   - NMI JSON transaction webhooks are modest; 64 KiB is ample.
const (
	maxCCBillWebhookBytes   int64 = 16 << 10  // 16 KiB
	maxStripeWebhookBytes   int64 = 256 << 10 // 256 KiB
	maxNMIWebhookBytes      int64 = 64 << 10  // 64 KiB
	maxCoinbaseWebhookBytes int64 = 64 << 10  // 64 KiB; Hook0 USDC-funding events are small JSON
)

// readLimitedWebhookBody reads the request body capped at maxBytes via
// http.MaxBytesReader. If the body exceeds the cap it writes a 413 response and
// returns ok=false so the caller stops before any further processing.
func readLimitedWebhookBody(r *httprequest.Request, maxBytes int64) ([]byte, bool) {
	if r.Request == nil || r.Request.Body == nil {
		return []byte{}, true
	}
	r.Request.Body = http.MaxBytesReader(nil, r.Request.Body, maxBytes)
	body, err := readRequestBody(r.Request.Body)
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			log.WithField("max_bytes", maxBytes).Warn("webhook payload exceeded size limit")
			r.ErrorJSON(http.StatusRequestEntityTooLarge, "Webhook payload too large")
			return nil, false
		}
		r.ErrorJSON(http.StatusInternalServerError, "Failed to read request body")
		return nil, false
	}
	return body, true
}

func Webhook(r *httprequest.Request) {
	provider := webhookutil.CanonicalProvider(r.Param("provider"))
	clientIP := r.GetRemoteIP()
	log.WithFields(log.Fields{"provider": provider, "client_ip": clientIP}).Debug("Received webhook")
	if r.State == nil || r.State.Config == nil {
		r.ErrorJSON(http.StatusServiceUnavailable, "Webhook processing is not configured")
		return
	}
	isTestEnv := r.State.Config.IsTestEnv()
	if processors.IsNMIBacked(provider) {
		if enqueueNMIWebhook(r, provider, clientIP) {
			r.SuccessJSON(map[string]string{"status": "accepted"})
		}
		return
	}
	switch provider {
	case subscriptions.ProcessorCCBill:
		if !isTestEnv {
			if !iputil.IsValidCCBillIP(clientIP) {
				log.WithFields(log.Fields{"client_ip": clientIP, "processor": "ccbill", "event_type": r.Query("eventType")}).Warn("CCBill webhook rejected - unauthorized IP address")
				r.ErrorJSON(http.StatusForbidden, "Unauthorized webhook source")
				return
			}
			log.WithField("client_ip", clientIP).Debug("CCBill webhook authenticated - valid IP range")
		} else {
			log.WithField("client_ip", clientIP).Debug("CCBill webhook authentication bypassed - test env enabled")
		}
		if enqueueCCBillWebhook(r, clientIP) {
			r.SuccessJSON(map[string]string{"status": "accepted"})
		}
		return
	case subscriptions.ProcessorStripe:
		if enqueueStripeWebhook(r, clientIP) {
			r.SuccessJSON(map[string]string{"status": "accepted"})
		}
		return
	case funding.ProviderCoinbase, "coinbase-onramp", "coinbase_onramp":
		if applyCoinbaseUSDCFundingWebhook(r) {
			r.SuccessJSON(map[string]string{"status": "accepted"})
		}
		return
	default:
		r.ErrorJSON(http.StatusBadRequest, "Invalid provider")
		return
	}
}

func enqueueCCBillWebhook(r *httprequest.Request, clientIP string) bool {
	body, ok := readLimitedWebhookBody(r, maxCCBillWebhookBytes)
	if !ok {
		return false
	}
	prepared, err := webhookutil.PrepareCCBill(body, r.Query("eventType"))
	if err != nil {
		switch {
		case errors.Is(err, webhookutil.ErrWebhookPayloadInvalid):
			r.ErrorJSON(http.StatusBadRequest, "Invalid webhook payload")
		case errors.Is(err, webhookutil.ErrWebhookEventTypeMissing):
			r.ErrorJSON(http.StatusBadRequest, "Missing eventType parameter")
		case errors.Is(err, webhookutil.ErrWebhookEventTypeMismatch):
			r.ErrorJSON(http.StatusBadRequest, "Webhook event type mismatch")
		default:
			r.ErrorJSON(http.StatusBadRequest, "Invalid webhook payload")
		}
		return false
	}
	args := prepared.QueueArgs(clientIP)
	if err := enqueueWebhookJob(r, args); err != nil {
		log.WithError(err).Error("failed to enqueue CCBill webhook")
		r.ErrorJSON(http.StatusInternalServerError, "Failed to enqueue webhook")
		return false
	}
	return true
}

func enqueueStripeWebhook(r *httprequest.Request, clientIP string) bool {
	body, ok := readLimitedWebhookBody(r, maxStripeWebhookBytes)
	if !ok {
		return false
	}
	var secrets []string
	stripeSecretKey := ""
	if stripeProc := r.State.Config.GetStripeProcessor(); stripeProc != nil {
		stripeSecretKey = strings.TrimSpace(stripeProc.SecretKey)
		if s := strings.TrimSpace(stripeProc.WebhookSecret); s != "" {
			secrets = append(secrets, s)
		}
		if s := strings.TrimSpace(stripeProc.WebhookSecretThin); s != "" {
			secrets = append(secrets, s)
		}
	}
	prepared, err := prepareStripeMultiSecret(body, secrets, r.Request.Header.Get("Stripe-Signature"), 5*time.Minute)
	if err != nil {
		switch {
		case errors.Is(err, webhookutil.ErrWebhookSignatureRequired):
			r.ErrorJSON(http.StatusUnauthorized, "Webhook signature required")
		case errors.Is(err, webhookutil.ErrWebhookSignatureMissing):
			r.ErrorJSON(http.StatusUnauthorized, "Missing webhook signature")
		case errors.Is(err, webhookutil.ErrWebhookSignatureInvalid):
			r.ErrorJSON(http.StatusUnauthorized, "Invalid webhook signature")
		case errors.Is(err, webhookutil.ErrWebhookPayloadInvalid):
			r.ErrorJSON(http.StatusBadRequest, "Invalid webhook payload")
		default:
			r.ErrorJSON(http.StatusBadRequest, "Invalid webhook payload")
		}
		return false
	}
	// Stripe "thin" event destinations deliver a minimal payload without the
	// object. Hydrate it into the classic {data:{object}} shape so the River
	// processor only ever sees snapshot-style events.
	if hydrated, herr := hydrateThinStripeEvent(r.Request.Context(), stripeSecretKey, prepared.Body); herr != nil {
		log.WithError(herr).Error("failed to hydrate thin stripe event")
		r.ErrorJSON(http.StatusBadGateway, "Failed to hydrate thin event")
		return false
	} else if hydrated != nil {
		prepared.Body = hydrated
	}
	// Process synchronously in-request: Stripe webhooks drive subscription and
	// entitlement state, and we want that reflected immediately rather than after
	// a River worker picks the job up. Stripe retries on a non-2xx, so a transient
	// failure here is safe to surface as 500; a non-retryable (poison) event is
	// acked so Stripe stops retrying.
	if r.State.WebhookDispatcher == nil {
		log.Error("webhook dispatcher unavailable")
		r.ErrorJSON(http.StatusInternalServerError, "Webhook processing unavailable")
		return false
	}
	signatureVerified := true
	msg := &webhooks.WebhookMessage{
		Processor:      subscriptions.ProcessorStripe,
		EventID:        prepared.EventID,
		EventType:      prepared.EventType,
		Payload:        prepared.Body,
		IPAddress:      clientIP,
		Signature:      prepared.Signature,
		SignatureValid: &signatureVerified,
		ReceivedAt:     time.Now(),
	}
	if err := r.State.WebhookDispatcher.Process(r.Request.Context(), msg); err != nil {
		fields := log.Fields{"event_id": prepared.EventID, "event_type": prepared.EventType}
		if webhooks.IsWebhookErrorNonRetryable(err) {
			log.WithError(err).WithFields(fields).Warn("stripe webhook non-retryable error; acking to stop retries")
			return true
		}
		log.WithError(err).WithFields(fields).Error("stripe webhook processing failed")
		r.ErrorJSON(http.StatusInternalServerError, "Webhook processing failed")
		return false
	}
	return true
}

func applyCoinbaseUSDCFundingWebhook(r *httprequest.Request) bool {
	if r.State == nil || r.State.DB == nil || r.State.Config == nil {
		r.ErrorJSON(http.StatusServiceUnavailable, "Webhook processing is not configured")
		return false
	}
	body, ok := readLimitedWebhookBody(r, maxCoinbaseWebhookBytes)
	if !ok {
		return false
	}
	cfg := r.State.Config.USDCFunding
	var secret string
	if cfg != nil && cfg.Providers != nil && cfg.Providers[funding.ProviderCoinbase] != nil {
		secret = strings.TrimSpace(cfg.Providers[funding.ProviderCoinbase].WebhookSecret)
	}
	if secret == "" {
		r.ErrorJSON(http.StatusServiceUnavailable, "Coinbase webhook secret is not configured")
		return false
	}
	signature := r.Header("X-Hook0-Signature")
	if signature == "" {
		signature = r.Header("Hook0-Signature")
	}
	if err := verifyHook0Signature(body, signature, secret, r.Request.Header, time.Now(), 5*time.Minute); err != nil {
		r.ErrorJSON(http.StatusUnauthorized, "Invalid webhook signature")
		return false
	}
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		r.ErrorJSON(http.StatusBadRequest, "Invalid webhook payload")
		return false
	}
	sessionID, ok := coinbaseFundingSessionID(payload)
	if !ok {
		r.ErrorJSON(http.StatusBadRequest, "Missing funding session reference")
		return false
	}
	id, err := api.ParseUSDCFundingSessionID(sessionID)
	if err != nil {
		r.APIError(api.InvalidIDError("usdc_funding_session"))
		return false
	}
	svc := funding.NewService(repo.NewUSDCFundingSessionRepo(r.State.DB), r.State.Config)
	if r.State.SolanaRPC != nil {
		svc.WithSolanaBalanceReader(r.State.SolanaRPC)
	}
	_, err = svc.ApplyProviderWebhook(r.Request.Context(), funding.ProviderWebhookEvent{
		Provider:  funding.ProviderCoinbase,
		SessionID: id,
		EventType: stringField(payload, "eventType", "event_type", "type"),
		Status:    stringField(payload, "status"),
		Payload:   payload,
	})
	if err != nil {
		switch {
		case errors.Is(err, funding.ErrSessionNotFound):
			r.APIError(api.NotFoundError("usdc_funding_session"))
		case errors.Is(err, funding.ErrInvalidRequest):
			r.ErrorJSON(http.StatusBadRequest, err.Error())
		default:
			log.WithError(err).Error("coinbase usdc funding webhook failed")
			r.ErrorJSON(http.StatusInternalServerError, "Webhook processing failed")
		}
		return false
	}
	return true
}

func coinbaseFundingSessionID(payload map[string]any) (string, bool) {
	paths := [][]string{
		{"partnerUserRef"},
		{"partner_user_ref"},
		{"data", "partnerUserRef"},
		{"data", "partner_user_ref"},
		{"data", "session", "partnerUserRef"},
		{"data", "session", "partner_user_ref"},
		{"session", "partnerUserRef"},
		{"session", "partner_user_ref"},
	}
	for _, path := range paths {
		if value, ok := nestedString(payload, path...); ok {
			return value, true
		}
	}
	return "", false
}

func stringField(payload map[string]any, names ...string) string {
	for _, name := range names {
		if value, ok := nestedString(payload, name); ok {
			return value
		}
		if value, ok := nestedString(payload, "data", name); ok {
			return value
		}
	}
	return ""
}

func nestedString(payload map[string]any, path ...string) (string, bool) {
	var current any = payload
	for _, part := range path {
		obj, ok := current.(map[string]any)
		if !ok {
			return "", false
		}
		current, ok = obj[part]
		if !ok {
			return "", false
		}
	}
	value, ok := current.(string)
	if !ok || strings.TrimSpace(value) == "" {
		return "", false
	}
	return strings.TrimSpace(value), true
}

func verifyHook0Signature(body []byte, signatureHeader, secret string, headers http.Header, now time.Time, tolerance time.Duration) error {
	if strings.TrimSpace(secret) == "" {
		return webhookutil.ErrWebhookSignatureRequired
	}
	signatureHeader = strings.TrimSpace(signatureHeader)
	if signatureHeader == "" {
		return webhookutil.ErrWebhookSignatureMissing
	}
	parts := map[string]string{}
	for _, element := range strings.Split(signatureHeader, ",") {
		key, value, ok := strings.Cut(strings.TrimSpace(element), "=")
		if ok {
			parts[strings.TrimSpace(key)] = strings.TrimSpace(value)
		}
	}
	timestamp := parts["t"]
	headerNames := parts["h"]
	provided := parts["v1"]
	if timestamp == "" || headerNames == "" || provided == "" {
		return webhookutil.ErrWebhookSignatureInvalid
	}
	issuedAt, err := strconv.ParseInt(timestamp, 10, 64)
	if err != nil {
		return webhookutil.ErrWebhookSignatureInvalid
	}
	if tolerance > 0 {
		age := now.Sub(time.Unix(issuedAt, 0))
		if age < -tolerance || age > tolerance {
			return webhookutil.ErrWebhookSignatureInvalid
		}
	}
	headerValues := make([]string, 0)
	for _, name := range strings.Fields(headerNames) {
		headerValues = append(headerValues, headers.Get(name))
	}
	signedPayload := timestamp + "." + headerNames + "." + strings.Join(headerValues, ".") + "." + string(body)
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte(signedPayload))
	expected := mac.Sum(nil)
	actual, err := hex.DecodeString(provided)
	if err != nil || len(actual) != len(expected) {
		return webhookutil.ErrWebhookSignatureInvalid
	}
	if !hmac.Equal(actual, expected) {
		return webhookutil.ErrWebhookSignatureInvalid
	}
	return nil
}

// prepareStripeMultiSecret verifies the Stripe signature against each configured
// secret, accepting the first that validates. Snapshot and thin Event
// Destinations sign the same payload with their own secret, so a single endpoint
// must try both. Non-signature errors (missing header, invalid payload)
// short-circuit since they are not secret-specific.
func prepareStripeMultiSecret(body []byte, secrets []string, header string, tolerance time.Duration) (webhookutil.Prepared, error) {
	if len(secrets) == 0 {
		return webhookutil.PrepareStripe(body, "", header, tolerance)
	}
	var lastErr error
	for _, secret := range secrets {
		prepared, err := webhookutil.PrepareStripe(body, secret, header, tolerance)
		if err == nil {
			return prepared, nil
		}
		lastErr = err
		if !errors.Is(err, webhookutil.ErrWebhookSignatureInvalid) {
			return webhookutil.Prepared{}, err
		}
	}
	return webhookutil.Prepared{}, lastErr
}

// hydrateThinStripeEvent converts a Stripe "thin" event payload (minimal, with a
// related_object reference but no embedded object) into the classic
// {id,type,data:{object}} shape by fetching the referenced resource. Returns
// (nil, nil) when the payload is already a snapshot event (object present) or is
// not a hydratable thin event, so the caller passes the body through unchanged.
func hydrateThinStripeEvent(ctx context.Context, stripeSecretKey string, body []byte) ([]byte, error) {
	var envelope struct {
		ID   string `json:"id"`
		Type string `json:"type"`
		Data *struct {
			Object json.RawMessage `json:"object"`
		} `json:"data"`
		RelatedObject *struct {
			ID   string `json:"id"`
			Type string `json:"type"`
			URL  string `json:"url"`
		} `json:"related_object"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return nil, nil // let downstream parsing surface the malformed payload
	}
	if envelope.Data != nil && len(envelope.Data.Object) > 0 {
		return nil, nil // snapshot event: object already present
	}
	if envelope.RelatedObject == nil || strings.TrimSpace(envelope.RelatedObject.URL) == "" {
		return nil, nil // not a hydratable thin event
	}
	if strings.TrimSpace(stripeSecretKey) == "" {
		return nil, fmt.Errorf("stripe secret key not configured for thin event hydration")
	}

	url := strings.TrimSpace(envelope.RelatedObject.URL)
	if !strings.HasPrefix(url, "http") {
		url = "https://api.stripe.com" + url
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+stripeSecretKey)
	// Thin-event hydration is a pure read (GET of the related object); the
	// unconditionally write-blocked choke client works in every mode and makes
	// any future mutation on this path fail loudly.
	resp, err := stripeapi.ReadOnlyClient(15 * time.Second).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	object, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("fetch related object failed (%d)", resp.StatusCode)
	}

	synthesized := struct {
		ID   string `json:"id"`
		Type string `json:"type"`
		Data struct {
			Object json.RawMessage `json:"object"`
		} `json:"data"`
	}{ID: envelope.ID, Type: envelope.Type}
	synthesized.Data.Object = object
	return json.Marshal(synthesized)
}

func enqueueWebhookJob(r *httprequest.Request, args riverjobs.WebhookProcessArgs) error {
	if r.State == nil || r.State.RiverProducer == nil {
		return fmt.Errorf("river producer is not configured")
	}
	opts := &river.InsertOpts{Queue: riverjobs.QueueWebhooks, UniqueOpts: river.UniqueOpts{ByArgs: true, ByQueue: true}}
	_, err := r.State.RiverProducer.Insert(r.Request.Context(), args, opts)
	return err
}

func enqueueNMIWebhook(r *httprequest.Request, provider string, clientIP string) bool {
	body, ok := readLimitedWebhookBody(r, maxNMIWebhookBytes)
	if !ok {
		return false
	}
	providerKey := webhookutil.CanonicalProvider(provider)
	client, ok := r.State.NMIClients[providerKey]
	if !ok || client == nil {
		r.ErrorJSON(http.StatusNotFound, fmt.Sprintf("unknown nmi provider '%s'", providerKey))
		return false
	}
	signingKey := client.GetWebhookSecret()
	prepared, err := webhookutil.PrepareNMI(providerKey, body, signingKey, firstPresentHeader(r.Request.Header, "Webhook-Signature", "X-Signature", "X-NMI-Signature", "X-Mobius-Signature"))
	if err != nil {
		if errors.Is(err, webhookutil.ErrNMIWebhookSecretMissing) || errors.Is(err, webhookutil.ErrNMIWebhookSignatureMissing) {
			log.WithError(err).Error("Missing webhook signature for NMI webhook")
			r.ErrorJSON(http.StatusUnauthorized, "Missing webhook signature")
			return false
		}
		if errors.Is(err, webhookutil.ErrNMIWebhookSignatureInvalid) {
			log.WithError(err).Error("NMI webhook signature verification failed")
			r.ErrorJSON(http.StatusUnauthorized, "Invalid webhook signature")
			return false
		}
		if errors.Is(err, webhookutil.ErrWebhookPayloadInvalid) {
			log.WithError(err).Error("failed to parse NMI webhook JSON")
			r.ErrorJSON(http.StatusBadRequest, "Invalid JSON data")
			return false
		}
		if errors.Is(err, webhookutil.ErrWebhookEventIDMissing) {
			r.ErrorJSON(http.StatusBadRequest, "Missing event_id in payload")
			return false
		}
		log.WithError(err).Error("failed to prepare NMI webhook")
		r.ErrorJSON(http.StatusBadRequest, "Invalid webhook payload")
		return false
	}
	args := prepared.QueueArgs(clientIP)
	if err := enqueueWebhookJob(r, args); err != nil {
		log.WithError(err).Error("failed to enqueue NMI webhook")
		r.ErrorJSON(http.StatusInternalServerError, "Failed to enqueue webhook")
		return false
	}
	return true
}

func firstPresentHeader(header http.Header, names ...string) string {
	for _, name := range names {
		if value := strings.TrimSpace(header.Get(name)); value != "" {
			return value
		}
	}
	return ""
}

func readRequestBody(body io.ReadCloser) ([]byte, error) {
	if body == nil {
		return []byte{}, nil
	}
	defer body.Close()
	return io.ReadAll(body)
}
