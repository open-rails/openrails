package server

import (
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/internal/modules/webhooks"
	"github.com/open-rails/openrails/internal/shared/webhookutil"
	"github.com/open-rails/openrails/pkg/merchant"
)

// MerchantWebhookPrefix is the merchant-scoped webhook surface (issue #529). An
// operator configures each provider to call /v1/merchants/:merchant/webhooks/:provider;
// OpenRails re-resolves the merchant from the slug, loads THAT merchant's signing
// secret, and verifies the signature AFTER merchant resolution. The router is NOT
// the trust boundary — OpenRails always re-derives the secret and re-verifies.
const MerchantWebhookPrefix = "/merchants/:merchant/webhooks"

// registerMerchantWebhookRoutes mounts the merchant-scoped webhook surface. The
// single default merchant may still use the global /v1/webhooks/:provider surface.
func (s *Server) registerMerchantWebhookRoutes(e *gin.Engine) {
	group := e.Group(StandaloneV1Prefix + MerchantWebhookPrefix)
	group.POST("/:provider", s.merchantWebhookHandler())
	log.WithField("prefix", StandaloneV1Prefix+MerchantWebhookPrefix).
		Info("merchant-scoped webhook routes registered")
}

// merchantWebhookHandler resolves the merchant from the path slug, loads that
// tenant's Stripe signing secret(s), and verifies the inbound signature BEFORE
// dispatching. Merchant resolution happens first, but the signature — verified with
// the resolved merchant's own secret — remains the trust boundary for billing
// semantics.
func (s *Server) merchantWebhookHandler() gin.HandlerFunc {
	return func(c *gin.Context) {
		ctx := c.Request.Context()
		provider := webhookutil.CanonicalProvider(c.Param("provider"))

		// 1. Resolve the merchant from the URL slug. An unknown/deleted tenant is a
		//    404 — we never fall back to the default merchant for a merchant-scoped URL.
		route, err := s.merchants.ResolveBySlug(ctx, c.Param("merchant"))
		if err != nil {
			if errors.Is(err, merchants.ErrMerchantRouteUnresolved) {
				jsonError(c, http.StatusNotFound, "unknown tenant")
				return
			}
			log.WithError(err).Error("merchant webhook: resolve tenant failed")
			jsonError(c, http.StatusInternalServerError, "merchant resolution failed")
			return
		}

		// Pin the resolved merchant onto the context so downstream merchant-owned DB
		// access is correctly scoped.
		ctx = merchant.WithID(ctx, route.MerchantID)

		// Only Stripe is merchant-credential-routed in this increment. Other
		// providers (CCBill IP-gated, NMI per-client) are not per-merchant secret
		// routed yet and fall through as unsupported on this surface.
		if provider != subscriptions.ProcessorStripe {
			jsonError(c, http.StatusBadRequest, "provider not supported on merchant webhook surface")
			return
		}

		// 2. Load THIS merchant's signing secret(s) AFTER resolution.
		creds, err := s.merchants.LoadStripeCredentials(ctx, route.MerchantID)
		if err != nil {
			log.WithError(err).Error("merchant webhook: load merchant credentials failed")
			// Distinguish a transient secret-backend outage (Vault unreachable/sealed)
			// from any other failure: a 503 tells Stripe to REDELIVER, so a brief Vault
			// blip never drops a webhook. We never fall through to "no secret" here —
			// an unverifiable webhook is rejected, not accepted.
			if errors.Is(err, merchants.ErrSecretBackendUnavailable) {
				jsonError(c, http.StatusServiceUnavailable, "secret backend temporarily unavailable, retry")
				return
			}
			jsonError(c, http.StatusInternalServerError, "credential load failed")
			return
		}
		var secrets []string
		if creds.WebhookSigningSecret != "" {
			secrets = append(secrets, creds.WebhookSigningSecret)
		}
		if creds.WebhookSigningThin != "" {
			secrets = append(secrets, creds.WebhookSigningThin)
		}
		if len(secrets) == 0 {
			// No configured signing secret for this merchant: reject rather than
			// accept an unverifiable webhook. A missing secret is NEVER "skip
			// verification".
			jsonError(c, http.StatusUnauthorized, "merchant webhook signing secret not configured")
			return
		}

		body, err := io.ReadAll(c.Request.Body)
		if err != nil {
			jsonError(c, http.StatusInternalServerError, "failed to read body")
			return
		}

		// 3. Verify the signature with the merchant's own secret (the trust boundary).
		prepared, perr := verifyStripeMerchant(body, secrets, c.GetHeader("Stripe-Signature"))
		if perr != nil {
			switch {
			case errors.Is(perr, webhookutil.ErrWebhookSignatureRequired),
				errors.Is(perr, webhookutil.ErrWebhookSignatureMissing),
				errors.Is(perr, webhookutil.ErrWebhookSignatureInvalid):
				jsonError(c, http.StatusUnauthorized, "invalid webhook signature")
			default:
				jsonError(c, http.StatusBadRequest, "invalid webhook payload")
			}
			return
		}

		if s.runtime == nil || s.runtime.WebhookDispatcher == nil {
			jsonError(c, http.StatusInternalServerError, "webhook processing unavailable")
			return
		}
		signatureVerified := true
		msg := &webhooks.WebhookMessage{
			Processor:      subscriptions.ProcessorStripe,
			EventID:        prepared.EventID,
			EventType:      prepared.EventType,
			Payload:        prepared.Body,
			IPAddress:      c.ClientIP(),
			Signature:      prepared.Signature,
			SignatureValid: &signatureVerified,
			ReceivedAt:     time.Now(),
		}
		if err := s.runtime.WebhookDispatcher.Process(ctx, msg); err != nil {
			if webhooks.IsWebhookErrorNonRetryable(err) {
				log.WithError(err).Warn("merchant stripe webhook non-retryable; acking")
				c.JSON(http.StatusOK, gin.H{"status": "accepted"})
				return
			}
			log.WithError(err).Error("merchant stripe webhook processing failed")
			jsonError(c, http.StatusInternalServerError, "processing failed")
			return
		}
		c.JSON(http.StatusOK, gin.H{"status": "accepted"})
	}
}

// verifyStripeMerchant verifies the Stripe signature against each of the merchant's
// configured secrets, accepting the first that validates. Mirrors the global
// multi-secret behaviour but is fed the resolved MERCHANT's secrets.
func verifyStripeMerchant(body []byte, secrets []string, header string) (webhookutil.Prepared, error) {
	var lastErr error
	for _, secret := range secrets {
		prepared, err := webhookutil.PrepareStripe(body, secret, header, 5*time.Minute)
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
