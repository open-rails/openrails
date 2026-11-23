package handlers

import (
	"encoding/json"
	"strings"
	"sync"
	"time"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db/models"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/integrations/basistheory"
	"github.com/open-rails/openrails/internal/logx"
	"github.com/open-rails/openrails/internal/modules/webhooks"
)

// Basis Theory webhooks: the event source is basis_theory, a custodian, not a
// rail. The signature is RSA-PSS SHA-256 against BT's CDN-published public key
// (BT-SIGNATURE / BT-SIGNATURE-VERSION), with no per-merchant secret, so one
// process-wide verifier per key URL serves every merchant.

var (
	btVerifierMu       sync.Mutex
	btVerifiers        = map[string]*basistheory.WebhookVerifier{}
	btKeyURLOverride   string
	btKeyURLOverrideMu sync.RWMutex
)

func basisTheoryVerifier(environment string) *basistheory.WebhookVerifier {
	btKeyURLOverrideMu.RLock()
	keyURL := btKeyURLOverride
	btKeyURLOverrideMu.RUnlock()
	if keyURL == "" {
		keyURL = webhooks.BasisTheoryWebhookKeyURL(environment)
	}
	btVerifierMu.Lock()
	defer btVerifierMu.Unlock()
	v, ok := btVerifiers[keyURL]
	if !ok {
		v = basistheory.NewWebhookVerifier(keyURL)
		btVerifiers[keyURL] = v
	}
	return v
}

// basisTheoryWebhookTenantID extracts the tenant identity the payload-derived
// surface routes by: the custodian's own account_id.
func basisTheoryWebhookTenantID(body []byte) string {
	var evt basistheory.Event
	if err := json.Unmarshal(body, &evt); err != nil {
		return ""
	}
	return strings.TrimSpace(evt.TenantID)
}

func processMerchantBasisTheoryWebhookBody(r *httprequest.Request, merchantID billing.MerchantID, tenantID, environment string, body []byte) bool {
	rail := string(models.EventSourceBasisTheory)
	sig := r.Header(basistheory.SignatureHeader)
	sigVersion := r.Header(basistheory.SignatureVersionHeader)
	if err := basisTheoryVerifier(environment).Verify(r.Request.Context(), body, sig, sigVersion); err != nil {
		r.State.WebhookHealth.Rejected(r.Request.Context())
		r.Log.WarnContext(r.Request.Context(), "basistheory webhook signature verification failed")
		r.ErrorCode(billing.CodeAuthenticationRequired, "Invalid webhook signature")
		return false
	}
	var evt basistheory.Event
	if err := json.Unmarshal(body, &evt); err != nil || strings.TrimSpace(evt.ID) == "" || strings.TrimSpace(evt.Type) == "" {
		r.ErrorCode(billing.CodeInvalidParam, "Invalid webhook payload")
		return false
	}
	r.State.WebhookHealth.Accepted(r.Request.Context())
	if r.State.WebhookDispatcher == nil {
		r.ErrorCode(billing.CodeInternalError, "Webhook processing unavailable")
		return false
	}
	verified := true
	msg := &webhooks.WebhookMessage{
		Rail:           rail,
		EventID:        evt.ID,
		EventType:      evt.Type,
		Payload:        body,
		IPAddress:      r.ClientIP(),
		Signature:      sig,
		SignatureValid: &verified,
		ReceivedAt:     time.Now(),
		// A custodian event carries the custodian's identity, never a PSP's: a
		// tenant id in a field named PspID would be a category error.
		CustodianAccountID: tenantID,
	}
	if err := r.State.WebhookDispatcher.Process(r.Request.Context(), msg); err != nil {
		if webhooks.IsWebhookErrorNonRetryable(err) {
			return true
		}
		r.Log.ErrorContext(r.Request.Context(), "merchant basistheory webhook processing failed", logx.Err(err))
		r.ErrorCode(billing.CodeInternalError, "Webhook processing failed")
		return false
	}
	return true
}
