package handlers

import (
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/internal/db/models"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/integrations/basistheory"
	"github.com/open-rails/openrails/internal/modules/webhooks"
	"github.com/open-rails/openrails/pkg/merchant"
)

// Basis Theory webhook ingestion (#795, route /webhooks/basistheory →
// rail vaulted_card). Signature = RSA-PSS SHA-256 against BT's CDN-published
// public key (BT-SIGNATURE / BT-SIGNATURE-VERSION) — no per-merchant secret,
// so one process-wide verifier per key URL serves every merchant.

var (
	btVerifierMu       sync.Mutex
	btVerifiers        = map[string]*basistheory.WebhookVerifier{}
	btKeyURLOverride   string
	btKeyURLOverrideMu sync.RWMutex
)

// SetBasisTheoryWebhookKeyURL overrides the CDN key URL process-wide —
// integration-test seam (a locally served keypair). "" restores the default.
func SetBasisTheoryWebhookKeyURL(u string) {
	btKeyURLOverrideMu.Lock()
	btKeyURLOverride = strings.TrimSpace(u)
	btKeyURLOverrideMu.Unlock()
}

func basisTheoryVerifier(r *httprequest.Request) *basistheory.WebhookVerifier {
	btKeyURLOverrideMu.RLock()
	keyURL := btKeyURLOverride
	btKeyURLOverrideMu.RUnlock()
	if keyURL == "" {
		keyURL = webhooks.BasisTheoryWebhookKeyURL(r.State.Config)
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
// surface routes by (the vaulted_card account_id is the BT tenant id).
func basisTheoryWebhookTenantID(body []byte) string {
	var evt basistheory.Event
	if err := json.Unmarshal(body, &evt); err != nil {
		return ""
	}
	return strings.TrimSpace(evt.TenantID)
}

func processMerchantBasisTheoryWebhook(r *httprequest.Request, merchantID merchant.ID, accountID string) bool {
	body, ok := readLimitedWebhookBody(r, maxBTWebhookBytes)
	if !ok {
		return false
	}
	if accountID == "" {
		// The envelope's tenant id pins the account so records stamp correctly.
		accountID = basisTheoryWebhookTenantID(body)
	}
	return processMerchantBasisTheoryWebhookBody(r, merchantID, accountID, body)
}

func processMerchantBasisTheoryWebhookBody(r *httprequest.Request, merchantID merchant.ID, accountID string, body []byte) bool {
	rail := string(models.RailVaultedCard)
	sig := r.Header(basistheory.SignatureHeader)
	sigVersion := r.Header(basistheory.SignatureVersionHeader)
	if err := basisTheoryVerifier(r).Verify(body, sig, sigVersion); err != nil {
		r.State.WebhookHealth.Rejected(r.Request.Context(), rail)
		log.Warn("basistheory webhook signature verification failed")
		r.ErrorJSON(http.StatusUnauthorized, "Invalid webhook signature")
		return false
	}
	var evt basistheory.Event
	if err := json.Unmarshal(body, &evt); err != nil || strings.TrimSpace(evt.ID) == "" || strings.TrimSpace(evt.Type) == "" {
		r.ErrorJSON(http.StatusBadRequest, "Invalid webhook payload")
		return false
	}
	r.State.WebhookHealth.Accepted(r.Request.Context(), rail)
	if r.State.WebhookDispatcher == nil {
		r.ErrorJSON(http.StatusInternalServerError, "Webhook processing unavailable")
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
		PspID:          accountID,
	}
	if err := r.State.WebhookDispatcher.Process(r.Request.Context(), msg); err != nil {
		if webhooks.IsWebhookErrorNonRetryable(err) {
			return true
		}
		log.WithError(err).Error("merchant basistheory webhook processing failed")
		r.ErrorJSON(http.StatusInternalServerError, "Webhook processing failed")
		return false
	}
	return true
}
