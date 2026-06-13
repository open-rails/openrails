package server

import (
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/internal/modules/webhooks"
	"github.com/open-rails/openrails/internal/shared/webhookutil"
	"github.com/open-rails/openrails/internal/tenancy"
	"github.com/open-rails/openrails/pkg/tenant"
)

// TenantWebhookPrefix is the tenant-scoped webhook surface (issue #225). An
// ingress resolves the tenant and forwards to /v1/t/:tenant/webhooks/:provider;
// OpenRails re-resolves the tenant from the slug, loads THAT tenant's signing
// secret, and verifies the signature AFTER tenant resolution. The router is NOT
// the trust boundary — OpenRails always re-derives the secret and re-verifies.
const TenantWebhookPrefix = "/t/:tenant/webhooks"

// registerTenantWebhookRoutes mounts the tenant-scoped webhook surface. The
// single default tenant may still use the global /v1/webhooks/:provider surface.
func (s *Server) registerTenantWebhookRoutes(e *gin.Engine) {
	group := e.Group(StandaloneV1Prefix + TenantWebhookPrefix)
	group.POST("/:provider", s.tenantWebhookHandler())
	log.WithField("prefix", StandaloneV1Prefix+TenantWebhookPrefix).
		Info("tenant-scoped webhook routes registered")
}

// tenantWebhookHandler resolves the tenant from the path slug, loads that
// tenant's Stripe signing secret(s), and verifies the inbound signature BEFORE
// dispatching. Tenant resolution happens first, but the signature — verified with
// the resolved tenant's own secret — remains the trust boundary for billing
// semantics.
func (s *Server) tenantWebhookHandler() gin.HandlerFunc {
	return func(c *gin.Context) {
		ctx := c.Request.Context()
		provider := webhookutil.CanonicalProvider(c.Param("provider"))

		// 1. Resolve the tenant from the URL slug. An unknown/deleted tenant is a
		//    404 — we never fall back to the default tenant for a tenant-scoped URL.
		route, err := s.tenancy.ResolveBySlug(ctx, c.Param("tenant"))
		if err != nil {
			if errors.Is(err, tenancy.ErrTenantRouteUnresolved) {
				c.JSON(http.StatusNotFound, gin.H{"error": "unknown tenant"})
				return
			}
			log.WithError(err).Error("tenant webhook: resolve tenant failed")
			c.JSON(http.StatusInternalServerError, gin.H{"error": "tenant resolution failed"})
			return
		}

		// Pin the resolved tenant onto the context so downstream tenant-owned DB
		// access is correctly scoped.
		ctx = tenant.WithID(ctx, route.TenantID)

		// Only Stripe is tenant-credential-routed in this increment. Other
		// providers (CCBill IP-gated, NMI per-client) are not per-tenant secret
		// routed yet and fall through as unsupported on this surface.
		if provider != subscriptions.ProcessorStripe {
			c.JSON(http.StatusBadRequest, gin.H{"error": "provider not supported on tenant webhook surface"})
			return
		}

		// 2. Load THIS tenant's signing secret(s) AFTER resolution.
		creds, err := s.tenancy.LoadStripeCredentials(ctx, route.TenantID)
		if err != nil {
			log.WithError(err).Error("tenant webhook: load tenant credentials failed")
			// Distinguish a transient secret-backend outage (Vault unreachable/sealed)
			// from any other failure: a 503 tells Stripe to REDELIVER, so a brief Vault
			// blip never drops a webhook. We never fall through to "no secret" here —
			// an unverifiable webhook is rejected, not accepted.
			if errors.Is(err, tenancy.ErrSecretBackendUnavailable) {
				c.JSON(http.StatusServiceUnavailable, gin.H{"error": "secret backend temporarily unavailable, retry"})
				return
			}
			c.JSON(http.StatusInternalServerError, gin.H{"error": "credential load failed"})
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
			// No configured signing secret for this tenant: reject rather than
			// accept an unverifiable webhook. A missing secret is NEVER "skip
			// verification".
			c.JSON(http.StatusUnauthorized, gin.H{"error": "tenant webhook signing secret not configured"})
			return
		}

		body, err := io.ReadAll(c.Request.Body)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to read body"})
			return
		}

		// 3. Verify the signature with the tenant's own secret (the trust boundary).
		prepared, perr := verifyStripeTenant(body, secrets, c.GetHeader("Stripe-Signature"))
		if perr != nil {
			switch {
			case errors.Is(perr, webhookutil.ErrWebhookSignatureRequired),
				errors.Is(perr, webhookutil.ErrWebhookSignatureMissing),
				errors.Is(perr, webhookutil.ErrWebhookSignatureInvalid):
				c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid webhook signature"})
			default:
				c.JSON(http.StatusBadRequest, gin.H{"error": "invalid webhook payload"})
			}
			return
		}

		if s.runtime == nil || s.runtime.WebhookDispatcher == nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "webhook processing unavailable"})
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
				log.WithError(err).Warn("tenant stripe webhook non-retryable; acking")
				c.JSON(http.StatusOK, gin.H{"status": "accepted"})
				return
			}
			log.WithError(err).Error("tenant stripe webhook processing failed")
			c.JSON(http.StatusInternalServerError, gin.H{"error": "processing failed"})
			return
		}
		c.JSON(http.StatusOK, gin.H{"status": "accepted"})
	}
}

// verifyStripeTenant verifies the Stripe signature against each of the tenant's
// configured secrets, accepting the first that validates. Mirrors the global
// multi-secret behaviour but is fed the resolved TENANT's secrets.
func verifyStripeTenant(body []byte, secrets []string, header string) (webhookutil.Prepared, error) {
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
