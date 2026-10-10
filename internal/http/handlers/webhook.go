package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/api"
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/models"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/internal/modules/payments/rails"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/internal/modules/webhooks"
	"github.com/open-rails/openrails/internal/shared/iputil"
	"github.com/open-rails/openrails/internal/shared/webhookutil"
	"github.com/open-rails/openrails/internal/webhookauth"
	log "github.com/sirupsen/logrus"
)

// Per-rail webhook body caps, tighter than DefaultMaxBodyBytes and sized with
// headroom above real payloads:
//   - CCBill form posts carry many customer/transaction fields: 16 KiB.
//   - Stripe snapshot events embed the full object: 256 KiB.
//   - NMI JSON transaction webhooks are modest: 64 KiB.
const (
	maxCCBillWebhookBytes int64 = 16 << 10  // 16 KiB
	maxStripeWebhookBytes int64 = 256 << 10 // 256 KiB
	maxNMIWebhookBytes    int64 = 64 << 10  // 64 KiB
	maxBTWebhookBytes     int64 = 64 << 10  // 64 KiB (BT event envelopes are small JSON)
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
			r.ErrorCode(billing.CodeRequestBodyTooLarge, "webhook payload too large")
			return nil, false
		}
		r.ErrorCode(billing.CodeInternalError, "Failed to read request body")
		return nil, false
	}
	return body, true
}

func Webhook(r *httprequest.Request) {
	provider, ok := canonicalWebhookRail(r)
	if !ok {
		return
	}
	clientIP := r.ClientIP()
	log.WithFields(log.Fields{"provider": provider, "client_ip": clientIP}).Debug("Received webhook")
	if r.State == nil || r.State.Config == nil {
		r.ErrorCode(billing.CodeServiceUnavailable, "Webhook processing is not configured")
		return
	}
	if bound := r.State.ConfiguredMerchant(); !bound.IsZero() {
		r.Request = r.Request.WithContext(merchant.WithID(r.Request.Context(), bound))
		processResolvedMerchantWebhook(r, provider, bound, strings.TrimSpace(r.Param("account_id")))
		return
	}
	// Account identity selects scope. Never trust an ambient merchant context
	// supplied by a host middleware instead of resolving the configured account.
	if handled, accepted := processPSPWebhook(r, provider, strings.TrimSpace(r.Param("account_id")), clientIP); handled {
		if accepted {
			r.SuccessJSON(WebhookReceipt{Status: "accepted"})
		}
		return
	}
	rejectWebhook(r)
}

// pinWebhookMerchantConn pins the resolved merchant's DB connection (the
// openrails.merchant_id GUC) for the rest of the dispatch and returns the
// release the caller must defer. A webhook resolves its merchant inside the
// handler, so MerchantDBConnMW cannot have pinned it. Nested calls are a
// no-op, so re-entering processResolvedMerchantWebhook is safe.
func pinWebhookMerchantConn(r *httprequest.Request, merchantID billing.MerchantID) (func(), bool) {
	if r != nil && r.State != nil {
		if bound := r.State.ConfiguredMerchant(); !bound.IsZero() && bound != merchantID {
			rejectWebhook(r)
			return func() {}, false
		}
	}

	if r == nil || r.State == nil || r.State.DB == nil || merchantID.IsZero() {
		return func() {}, true
	}
	ctx, release, err := r.State.DB.WithMerchantConn(merchant.WithID(r.Request.Context(), merchantID))
	if err != nil {
		log.WithError(err).WithField("merchant_id", merchantID.String()).
			Error("webhook: merchant db connection setup failed")
		r.ErrorCode(billing.CodeInternalError, "Webhook processing failed")
		return func() {}, false
	}
	r.Request = r.Request.WithContext(ctx)
	return release, true
}

func processResolvedMerchantWebhook(r *httprequest.Request, provider string, merchantID billing.MerchantID, accountID string) {
	if strings.TrimSpace(accountID) == "" {
		r.ErrorCode(billing.CodeInvalidParam, "Webhook account_id is required")
		return
	}
	release, ok := pinWebhookMerchantConn(r, merchantID)
	if !ok {
		return
	}
	defer release()

	if rails.IsNMI(models.Rail(provider)) {
		if processMerchantNMIWebhook(r, provider, merchantID, accountID) {
			r.SuccessJSON(WebhookReceipt{Status: "accepted"})
		}
		return
	}
	if provider == string(models.EventSourceBasisTheory) {
		if processMerchantBasisTheoryWebhook(r, merchantID, accountID) {
			r.SuccessJSON(WebhookReceipt{Status: "accepted"})
		}
		return
	}
	if provider == subscriptions.RailCCBill {
		// Pin the routed PSP first, so a refused source counts against it.
		if r.State.Merchants != nil {
			pspID, found, err := r.State.Merchants.ResolvePSPID(r.Request.Context(), merchantID, provider, accountID)
			if !bindResolvedWebhookPSP(r, pspID, found, err) {
				return
			}
		}
		clientIP := r.ClientIP()
		if !ccbillWebhookIPAllowed(r, clientIP) {
			r.State.WebhookHealth.Rejected(r.Request.Context())
			r.ErrorCode(billing.CodeResourceAccessDenied, "Unauthorized webhook source")
			return
		}
		if processMerchantCCBillWebhook(r, clientIP, accountID) {
			r.SuccessJSON(WebhookReceipt{Status: "accepted"})
		}
		return
	}
	if provider != subscriptions.RailStripe {
		r.ErrorCode(billing.CodeInvalidParam, "Provider not supported on merchant webhook surface")
		return
	}

	body, ok := readLimitedWebhookBody(r, maxStripeWebhookBytes)
	if !ok {
		return
	}
	var creds merchants.StripeCredentials
	var err error
	var found bool
	creds, found, err = r.State.Merchants.LoadStripeCredentialsForAccount(r.Request.Context(), merchantID, accountID)
	if err == nil && !found {
		rejectWebhook(r)
		return
	}
	if err != nil {
		if errors.Is(err, merchants.ErrSecretBackendUnavailable) {
			r.ErrorCode(billing.CodeServiceUnavailable, "Secret backend temporarily unavailable, retry")
			return
		}
		log.WithError(err).Error("merchant webhook: load merchant credentials failed")
		r.ErrorCode(billing.CodeInternalError, "Credential load failed")
		return
	}
	pspID, found, resolveErr := r.State.Merchants.ResolvePSPID(r.Request.Context(), merchantID, provider, accountID)
	if !bindResolvedWebhookPSP(r, pspID, found, resolveErr) {
		return
	}
	var secrets []string
	if s := strings.TrimSpace(creds.WebhookSigningSecret); s != "" {
		secrets = append(secrets, s)
	}
	if s := strings.TrimSpace(creds.WebhookSigningThin); s != "" {
		secrets = append(secrets, s)
	}
	// Through an api_version rollover the superseded endpoint keeps delivering
	// with the previous secret; accepting it keeps the rollover gapless.
	if s := strings.TrimSpace(creds.WebhookSigningPrevious); s != "" {
		secrets = append(secrets, s)
	}
	if len(secrets) == 0 {
		rejectWebhook(r)
		return
	}
	prepared, err := prepareStripeMultiSecret(body, secrets, r.Header("Stripe-Signature"), 5*time.Minute)
	if err != nil {
		switch {
		case errors.Is(err, webhookutil.ErrWebhookSignatureRequired),
			errors.Is(err, webhookutil.ErrWebhookSignatureMissing),
			errors.Is(err, webhookutil.ErrWebhookSignatureInvalid):
			r.State.WebhookHealth.Rejected(r.Request.Context())
			r.ErrorCode(billing.CodeAuthenticationRequired, "Invalid webhook signature")
		default:
			r.ErrorCode(billing.CodeInvalidParam, "Invalid webhook payload")
		}
		return
	}
	// Stripe "thin" event destinations deliver a minimal payload without the
	// object. Hydrate it with the MERCHANT's secret key into the classic
	// {data:{object}} shape so dispatch only ever sees snapshot-style events.
	if hydrated, herr := hydrateThinStripeEvent(r.Request.Context(), strings.TrimSpace(creds.SecretKey), creds.AccountID, prepared.Body, r.State.StripeClients); herr != nil {
		if errors.Is(herr, errStripeWebhookAccountMismatch) {
			r.State.WebhookHealth.Rejected(r.Request.Context())
			r.APIError(api.NewAPIError(http.StatusBadRequest, api.ErrorTypeInvalidRequest, "webhook_account_mismatch", "Webhook account does not match payload"))
			return
		}
		log.WithError(herr).Error("failed to hydrate thin stripe event")
		r.ErrorCode(billing.CodeServiceUnavailable, "the provider event could not be read; retry")
		return
	} else if hydrated != nil {
		prepared.Body = hydrated
		prepared.EventID, prepared.EventType, err = webhookutil.ParseStripeEventMeta(hydrated)
		if err != nil {
			r.ErrorCode(billing.CodeServiceUnavailable, "the provider event could not be read; retry")
			return
		}
	}
	r.State.WebhookHealth.Accepted(r.Request.Context())
	if r.State.WebhookDispatcher == nil {
		r.ErrorCode(billing.CodeInternalError, "Webhook processing unavailable")
		return
	}
	signatureVerified := true
	msg := &webhooks.WebhookMessage{
		Rail:           subscriptions.RailStripe,
		EventID:        prepared.EventID,
		EventType:      prepared.EventType,
		Payload:        prepared.Body,
		IPAddress:      r.ClientIP(),
		Signature:      prepared.Signature,
		SignatureValid: &signatureVerified,
		ReceivedAt:     time.Now(),
		PspID:          accountID,
	}
	if err := r.State.WebhookDispatcher.Process(r.Request.Context(), msg); err != nil {
		if webhooks.IsWebhookErrorNonRetryable(err) {
			r.SuccessJSON(WebhookReceipt{Status: "accepted"})
			return
		}
		log.WithError(err).Error("merchant stripe webhook processing failed")
		r.ErrorCode(billing.CodeInternalError, "Webhook processing failed")
		return
	}
	r.SuccessJSON(WebhookReceipt{Status: "accepted"})
}

func processPSPWebhook(r *httprequest.Request, rail, routeAccountID, clientIP string) (handled bool, accepted bool) {
	if strings.TrimSpace(routeAccountID) == "" {
		r.ErrorCode(billing.CodeInvalidParam, "Webhook account_id is required")
		return true, false
	}
	if r.State == nil || r.State.Merchants == nil {
		return false, false
	}
	environment := webhookProviderEnvironment(r)
	switch {
	case rails.IsNMI(models.Rail(rail)):
		body, ok := readLimitedWebhookBody(r, maxNMIWebhookBytes)
		if !ok {
			return true, false
		}
		accountID := nmiWebhookAccountID(body)
		if accountID == "" {
			r.ErrorCode(billing.CodeInvalidParam, "NMI webhook payload is missing merchant account identity")
			return true, false
		}
		if accountID != routeAccountID {
			r.ErrorCode(billing.CodeInvalidParam, "Webhook account does not match payload")
			return true, false
		}
		account, release, ok := resolveWebhookPSP(r, string(models.RailNMI), environment, accountID)
		if !ok {
			return true, false
		}
		defer release()
		return true, processMerchantNMIWebhookBody(r, string(models.RailNMI), account.MerchantID, account.AccountID, body)
	case rail == subscriptions.RailCCBill:
		if !ccbillWebhookIPAllowed(r, clientIP) {
			r.ErrorCode(billing.CodeResourceAccessDenied, "Unauthorized webhook source")
			return true, false
		}
		body, ok := readLimitedWebhookBody(r, maxCCBillWebhookBytes)
		if !ok {
			return true, false
		}
		prepared, accountID, ok := prepareCCBillWebhookWithAccountID(r, body)
		if !ok {
			return true, false
		}
		if accountID != routeAccountID {
			r.ErrorCode(billing.CodeInvalidParam, "Webhook account does not match payload")
			return true, false
		}
		account, release, ok := resolveWebhookPSP(r, subscriptions.RailCCBill, environment, accountID)
		if !ok {
			return true, false
		}
		defer release()
		return true, processMerchantCCBillWebhookPrepared(r, clientIP, prepared, account.AccountID)
	case rail == string(models.EventSourceBasisTheory):
		body, ok := readLimitedWebhookBody(r, maxBTWebhookBytes)
		if !ok {
			return true, false
		}
		tenantID := basisTheoryWebhookTenantID(body)
		if tenantID == "" {
			r.ErrorCode(billing.CodeInvalidParam, "Basis Theory webhook payload is missing tenant identity")
			return true, false
		}
		if tenantID != routeAccountID {
			r.ErrorCode(billing.CodeInvalidParam, "Webhook account does not match payload")
			return true, false
		}
		// A custodian event routes by the custodian's tenant identity: it
		// resolves a custodian, not a PSP (one custodian may back several).
		custodian, release, ok := resolveWebhookCustodianAccount(r, models.CustodianBasisTheory, environment, tenantID)
		if !ok {
			return true, false
		}
		defer release()
		return true, processMerchantBasisTheoryWebhookBody(r, custodian.MerchantID, custodian.AccountID, body)
	case rail == subscriptions.RailStripe && routeAccountID != "":
		account, release, ok := resolveWebhookPSP(r, subscriptions.RailStripe, environment, routeAccountID)
		if !ok {
			return true, false
		}
		defer release()
		// handled=true, accepted=false: processResolvedMerchantWebhook writes
		// its own response (401 on a bad signature, 200 on success), so the
		// caller must not write "accepted" on top of a refused forgery.
		processResolvedMerchantWebhook(r, subscriptions.RailStripe, account.MerchantID, account.AccountID)
		return true, false
	default:
		return false, false
	}
}

func webhookProviderEnvironment(r *httprequest.Request) string {
	return config.ExpectedProviderEnvironment(r != nil && r.State != nil && r.State.Config != nil && config.IsTestMode(r.State.Config))
}

// ccbillWebhookIPAllowed binds the request to the ONE CCBill source-IP gate
// (webhookauth.CCBillIPAllowed) that the embedded Service surface also calls.
func ccbillWebhookIPAllowed(r *httprequest.Request, clientIP string) bool {
	if r == nil || r.State == nil {
		return iputil.IsValidCCBillIP(clientIP)
	}
	return webhookauth.CCBillIPAllowed(r.Request.Context(), r.State.Config, ccbillLivePSPProbe(r), clientIP)
}

// ccbillLivePSPProbe binds the catalog probe to the request; a var so handler
// tests can stub the DB-backed merchants service. Returning nil (no merchants
// service) fails the gate closed.
var ccbillLivePSPProbe = func(r *httprequest.Request) webhookauth.LiveRailProbe {
	if r.State.Merchants == nil {
		return nil
	}
	return func(ctx context.Context) (merchants.LiveRailPresence, error) {
		return r.State.Merchants.ProbeLiveRailPSPs(ctx, subscriptions.RailCCBill)
	}
}

// resolveWebhookPSP resolves the merchant + PSP a payload-identified
// account belongs to, pins BOTH on the request (merchant id, psp id) and pins the
// merchant's DB connection. The returned release must be deferred by the caller —
// see pinWebhookMerchantConn for why the pin cannot live in middleware.
func resolveWebhookPSP(r *httprequest.Request, rail, environment, accountID string) (merchants.PSPIdentity, func(), bool) {
	return pinWebhookAccount(r, rail, environment, accountID,
		r.State.Merchants.ResolvePSPByIdentity)
}

// resolveWebhookCustodianAccount resolves the custodian a tenant identity
// belongs to and pins its merchant, but no PSP id: a custodian may back
// several PSPs, and its event is about the instrument, not a gateway.
func resolveWebhookCustodianAccount(r *httprequest.Request, kind, environment, tenantID string) (merchants.CustodianIdentity, func(), bool) {
	noop := func() {}
	custodian, ok, err := r.State.Merchants.ResolveCustodianByIdentity(r.Request.Context(), kind, environment, tenantID)
	if err != nil {
		log.WithError(err).WithFields(log.Fields{"custodian": kind, "environment": environment, "account_id": tenantID}).Error("webhook custodian resolution failed")
		r.ErrorCode(billing.CodeInternalError, "Custodian resolution failed")
		return merchants.CustodianIdentity{}, noop, false
	}
	if !ok {
		rejectWebhook(r)
		return merchants.CustodianIdentity{}, noop, false
	}
	// Pin the custodian the event came from, as the PSP routes pin theirs, so
	// anything enqueued here is custodian-addressed, never one of its PSPs.
	ctx := merchant.WithID(r.Request.Context(), custodian.MerchantID)
	r.Request = r.Request.WithContext(db.WithCustodianID(ctx, custodian.ID))
	release, ok := pinWebhookMerchantConn(r, custodian.MerchantID)
	if !ok {
		return merchants.CustodianIdentity{}, noop, false
	}
	return custodian, release, true
}

func pinWebhookAccount(r *httprequest.Request, rail, environment, accountID string,
	resolve func(context.Context, string, string, string) (merchants.PSPIdentity, bool, error),
) (merchants.PSPIdentity, func(), bool) {
	noop := func() {}
	account, ok, err := resolve(r.Request.Context(), rail, environment, accountID)
	if err != nil {
		log.WithError(err).WithFields(log.Fields{"rail": rail, "environment": environment, "account_id": accountID}).Error("webhook PSP resolution failed")
		r.ErrorCode(billing.CodeInternalError, "PSP resolution failed")
		return merchants.PSPIdentity{}, noop, false
	}
	if !ok {
		rejectWebhook(r)
		return merchants.PSPIdentity{}, noop, false
	}
	ctx := merchant.WithID(r.Request.Context(), account.MerchantID)
	ctx = db.WithPSPID(ctx, account.ID)
	r.Request = r.Request.WithContext(ctx)
	release, ok := pinWebhookMerchantConn(r, account.MerchantID)
	if !ok {
		return merchants.PSPIdentity{}, noop, false
	}
	return account, release, true
}

func processMerchantNMIWebhook(r *httprequest.Request, provider string, merchantID billing.MerchantID, accountID string) bool {
	body, ok := readLimitedWebhookBody(r, maxNMIWebhookBytes)
	if !ok {
		return false
	}
	return processMerchantNMIWebhookBody(r, provider, merchantID, accountID, body)
}

func processMerchantNMIWebhookBody(r *httprequest.Request, provider string, merchantID billing.MerchantID, accountID string, body []byte) bool {
	if strings.TrimSpace(accountID) == "" {
		r.ErrorCode(billing.CodeInvalidParam, "Webhook account_id is required")
		return false
	}
	var keys merchants.NMIWebhookSecrets
	var err error
	var found bool
	keys, found, err = r.State.Merchants.LoadNMIWebhookSigningSecretForAccount(r.Request.Context(), merchantID, accountID)
	if err == nil && !found {
		rejectWebhook(r)
		return false
	}
	if err != nil {
		if errors.Is(err, merchants.ErrSecretBackendUnavailable) {
			r.ErrorCode(billing.CodeServiceUnavailable, "Secret backend temporarily unavailable, retry")
			return false
		}
		log.WithError(err).Error("merchant webhook: load nmi signing secret failed")
		r.ErrorCode(billing.CodeInternalError, "Credential load failed")
		return false
	}
	pspID, found, resolveErr := r.State.Merchants.ResolvePSPID(r.Request.Context(), merchantID, provider, accountID)
	if !bindResolvedWebhookPSP(r, pspID, found, resolveErr) {
		return false
	}
	// NMI sends one signature header, `Webhook-Signature: t=<ts>,s=<hex>`
	// (docs/rails/nmi.md). No other spelling is accepted, so a forged
	// signature has one header to aim at.
	header := strings.TrimSpace(r.Request.Header.Get("Webhook-Signature"))
	signingKey := keys.Current
	prepared, err := webhookutil.PrepareNMI(provider, body, signingKey, header)
	// The rotated-out secret verifies only inside its bounded overlap.
	if errors.Is(err, webhookutil.ErrNMIWebhookSignatureInvalid) && strings.TrimSpace(keys.Previous) != "" {
		if again, againErr := webhookutil.PrepareNMI(provider, body, keys.Previous, header); againErr == nil {
			prepared, err, signingKey = again, nil, keys.Previous
		}
	}
	if err != nil {
		switch {
		case errors.Is(err, webhookutil.ErrNMIWebhookSecretMissing),
			errors.Is(err, webhookutil.ErrNMIWebhookSignatureMissing):
			r.State.WebhookHealth.Rejected(r.Request.Context())
			r.ErrorCode(billing.CodeAuthenticationRequired, "Missing webhook signature")
		case errors.Is(err, webhookutil.ErrNMIWebhookSignatureInvalid):
			r.State.WebhookHealth.Rejected(r.Request.Context())
			r.ErrorCode(billing.CodeAuthenticationRequired, "Invalid webhook signature")
		case errors.Is(err, webhookutil.ErrWebhookPayloadInvalid):
			r.ErrorCode(billing.CodeInvalidParam, "Invalid JSON data")
		case errors.Is(err, webhookutil.ErrWebhookEventIDMissing):
			r.ErrorCode(billing.CodeInvalidParam, "Missing event_id in payload")
		default:
			r.ErrorCode(billing.CodeInvalidParam, "Invalid webhook payload")
		}
		return false
	}
	if payloadAccount := nmiWebhookAccountID(body); payloadAccount != "" && payloadAccount != accountID {
		r.ErrorCode(billing.CodeInvalidParam, "Webhook account does not match payload")
		return false
	}
	r.State.WebhookHealth.Accepted(r.Request.Context())
	if r.State.WebhookDispatcher == nil {
		r.ErrorCode(billing.CodeInternalError, "Webhook processing unavailable")
		return false
	}
	signatureVerified := true
	msg := &webhooks.WebhookMessage{
		Rail:           prepared.Rail,
		EventID:        prepared.EventID,
		EventType:      prepared.EventType,
		Payload:        prepared.Body,
		IPAddress:      r.ClientIP(),
		Signature:      prepared.Signature,
		SigningSecret:  signingKey,
		SignatureValid: &signatureVerified,
		ReceivedAt:     time.Now(),
		PspID:          accountID,
	}
	if err := r.State.WebhookDispatcher.Process(r.Request.Context(), msg); err != nil {
		if webhooks.IsWebhookErrorNonRetryable(err) {
			return true
		}
		log.WithError(err).Error("merchant nmi webhook processing failed")
		r.ErrorCode(billing.CodeInternalError, "Webhook processing failed")
		return false
	}
	return true
}

// Credentials alone do not identify the account that will own the receipt.
// Every merchant-specific dispatcher must retain a concrete persisted PSP.
func bindResolvedWebhookPSP(r *httprequest.Request, id uuid.UUID, found bool, err error) bool {
	if err != nil {
		r.ErrorCode(billing.CodeInternalError, "Webhook account resolution failed")
		return false
	}
	if !found || id == uuid.Nil {
		rejectWebhook(r)
		return false
	}
	r.Request = r.Request.WithContext(db.WithPSPID(r.Request.Context(), id))
	return true
}

func processMerchantCCBillWebhook(r *httprequest.Request, clientIP, routeAccountID string) bool {
	body, ok := readLimitedWebhookBody(r, maxCCBillWebhookBytes)
	if !ok {
		return false
	}
	prepared, accountID, ok := prepareCCBillWebhookWithAccountID(r, body)
	if !ok {
		return false
	}
	if accountID != routeAccountID {
		r.ErrorCode(billing.CodeInvalidParam, "Webhook account does not match payload")
		return false
	}
	mid, err := merchant.Require(r.Request.Context())
	if err != nil || r.State.Merchants == nil {
		r.ErrorCode(billing.CodeServiceUnavailable, "Merchant webhook routing is not configured")
		return false
	}
	pspID, found, err := r.State.Merchants.ResolvePSPID(r.Request.Context(), mid, subscriptions.RailCCBill, routeAccountID)
	if err != nil {
		r.ErrorCode(billing.CodeInternalError, "Webhook account resolution failed")
		return false
	}
	if !found {
		rejectWebhook(r)
		return false
	}
	r.Request = r.Request.WithContext(db.WithPSPID(r.Request.Context(), pspID))
	return processMerchantCCBillWebhookPrepared(r, clientIP, prepared, accountID)
}

func prepareCCBillWebhookWithAccountID(r *httprequest.Request, body []byte) (webhookutil.Prepared, string, bool) {
	prepared, err := webhookutil.PrepareCCBill(body, r.Query("eventType"))
	if err != nil {
		switch {
		case errors.Is(err, webhookutil.ErrWebhookPayloadInvalid):
			r.ErrorCode(billing.CodeInvalidParam, "Invalid webhook payload")
		case errors.Is(err, webhookutil.ErrWebhookEventTypeMissing):
			r.ErrorCode(billing.CodeInvalidParam, "Missing eventType parameter")
		case errors.Is(err, webhookutil.ErrWebhookEventTypeMismatch):
			r.ErrorCode(billing.CodeInvalidParam, "Webhook event type mismatch")
		default:
			r.ErrorCode(billing.CodeInvalidParam, "Invalid webhook payload")
		}
		return webhookutil.Prepared{}, "", false
	}
	accountID := ccbillWebhookAccountID(prepared.Body)
	if accountID == "" {
		r.ErrorCode(billing.CodeInvalidParam, "CCBill webhook payload is missing client account identity")
		return webhookutil.Prepared{}, "", false
	}
	return prepared, accountID, true
}

func processMerchantCCBillWebhookPrepared(r *httprequest.Request, clientIP string, prepared webhookutil.Prepared, accountID string) bool {
	// CCBill has no HMAC: IP-allowlisted + well-formed IS its verified-accepted.
	r.State.WebhookHealth.Accepted(r.Request.Context())
	if r.State.WebhookDispatcher == nil {
		r.ErrorCode(billing.CodeInternalError, "Webhook processing unavailable")
		return false
	}
	// Stamp the routed PSP so every row this event materialises is
	// attributable: the caller usually pinned it, else the payload's account
	// identity resolves it.
	ctx := r.Request.Context()
	if accountID != "" && r.State.Merchants != nil {
		if mid, ok := merchant.FromContext(ctx); ok && !mid.IsZero() {
			if pid, found, rerr := r.State.Merchants.ResolvePSPID(ctx, mid, subscriptions.RailCCBill, accountID); rerr == nil && found {
				ctx = db.WithPSPID(ctx, pid)
				r.Request = r.Request.WithContext(ctx)
			}
		}
	}
	msg := ccbillWebhookMessage(clientIP, prepared, accountID)
	if err := r.State.WebhookDispatcher.Process(ctx, msg); err != nil {
		if code := webhooks.WebhookRefusalCode(err); code != "" {
			r.SuccessJSON(WebhookReceipt{Status: "refused", Code: &code})
			return false
		}
		if webhooks.IsWebhookErrorNonRetryable(err) {
			return true
		}
		log.WithError(err).Error("merchant ccbill webhook processing failed")
		r.ErrorCode(billing.CodeInternalError, "Webhook processing failed")
		return false
	}
	return true
}

// ccbillWebhookMessage builds the dispatch message for a CCBill event. CCBill
// has no signature (the source-IP allowlist authenticates it), so
// SignatureValid stays nil: never claimed.
func ccbillWebhookMessage(clientIP string, prepared webhookutil.Prepared, accountID string) *webhooks.WebhookMessage {
	msg := &webhooks.WebhookMessage{
		Rail:       subscriptions.RailCCBill,
		EventID:    prepared.EventID,
		EventType:  prepared.EventType,
		Payload:    prepared.Body,
		IPAddress:  clientIP,
		Signature:  prepared.Signature,
		ReceivedAt: time.Now(),
	}
	if accountID != "" {
		msg.PspID = accountID
	}
	return msg
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

func nmiWebhookAccountID(body []byte) string {
	var envelope struct {
		EventBody json.RawMessage `json:"event_body"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil || len(envelope.EventBody) == 0 {
		return ""
	}
	var payload struct {
		Merchant *struct {
			ID webhooks.Stringish `json:"id"`
		} `json:"merchant"`
	}
	if err := json.Unmarshal(envelope.EventBody, &payload); err != nil || payload.Merchant == nil {
		return ""
	}
	return payload.Merchant.ID.Trimmed()
}

func ccbillWebhookAccountID(body []byte) string {
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return ""
	}
	clientAccnum := strings.TrimSpace(fmt.Sprint(payload["clientAccnum"]))
	clientSubacc := strings.TrimSpace(fmt.Sprint(payload["clientSubacc"]))
	if clientAccnum == "" || clientAccnum == "<nil>" {
		return ""
	}
	if clientSubacc == "" || clientSubacc == "<nil>" {
		return clientAccnum
	}
	// The composite CCBill identity is dash-joined (clientAccnum-clientSubacc),
	// CCBill's own convention and the declared account_id format.
	return clientAccnum + "-" + clientSubacc
}

// canonicalWebhookRail resolves the URL's rail segment, writing a 400 with the
// rename when the segment is a retired alias. The rail segment is the gateway
// kind; a PSP is named by :account_id or the payload's account identity.
func canonicalWebhookRail(r *httprequest.Request) (string, bool) {
	provider, err := webhookutil.CanonicalRail(r.Param("rail"))
	if err != nil {
		r.ErrorCode(billing.CodeInvalidParam, err.Error())
		return "", false
	}
	return provider, true
}

func readRequestBody(body io.ReadCloser) ([]byte, error) {
	if body == nil {
		return []byte{}, nil
	}
	defer body.Close()
	return io.ReadAll(body)
}

// rejectWebhook answers an unknown account, an unconfigured secret and a bad
// signature identically, so a webhook route never reveals which provider
// accounts a deployment serves.
func rejectWebhook(r *httprequest.Request) {
	r.ErrorCode(billing.CodeAuthenticationRequired, "Invalid webhook signature")
}

// WebhookReceipt answers a provider's webhook: accepted, or refused with the
// registered code of a refusal the provider should not retry.
type WebhookReceipt struct {
	Status string  `json:"status"`
	Code   *string `json:"code"`
}
