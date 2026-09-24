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

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/models"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/internal/modules/payments/rails"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/internal/modules/webhooks"
	"github.com/open-rails/openrails/internal/shared/iputil"
	"github.com/open-rails/openrails/internal/shared/webhookutil"
	"github.com/open-rails/openrails/internal/webhookauth"
	"github.com/open-rails/openrails/pkg/api"
	"github.com/open-rails/openrails/pkg/merchant"
	log "github.com/sirupsen/logrus"
)

// Per-rail webhook body caps. The primary memory-exhaustion fix is the
// global 1 MiB BodyLimit now applying to webhook routes (the blanket exemption
// was removed); these per-rail caps are tighter defense-in-depth. They are
// sized with headroom above real payloads to avoid 413-ing legitimate webhooks:
//   - CCBill background posts are form-encoded but carry many customer/transaction
//     fields, so 16 KiB rather than a couple KiB.
//   - Stripe "snapshot" events embed the full object (subscriptions, invoices with
//     line items) and can be tens of KiB, so 256 KiB.
//   - NMI JSON transaction webhooks are modest; 64 KiB is ample.
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
			r.ErrorJSON(http.StatusRequestEntityTooLarge, "Webhook payload too large")
			return nil, false
		}
		r.ErrorJSON(http.StatusInternalServerError, "Failed to read request body")
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
		r.ErrorJSON(http.StatusServiceUnavailable, "Webhook processing is not configured")
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
			r.SuccessJSON(map[string]string{"status": "accepted"})
		}
		return
	}
	rejectWebhook(r)
}

// pinWebhookMerchantConn pins the resolved merchant's DB connection (the
// app.merchant_id GUC) on the request for the rest of the dispatch, and returns
// the release the caller must defer.
//
// The webhook surfaces resolve their merchant INSIDE the handler — from the URL
// configured runtime binding or provider account
// identity (processPSPWebhook) — so middleware.MerchantDBConnMW
// cannot have run: at middleware time there is no merchant to pin. Without this,
// downstream reads and writes would not share the resolved merchant's request
// connection and transaction context. Queries enforce merchant predicates;
// this pin preserves that scope across the webhook dispatch. It does not set
// a PostgreSQL role or rely on row-level security.
//
// Nested calls are a no-op (db.WithMerchantConn returns the existing pin), so
// the Stripe-by-account path that re-enters processResolvedMerchantWebhook is
// safe.
func pinWebhookMerchantConn(r *httprequest.Request, merchantID merchant.ID) (func(), bool) {
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
		r.ErrorJSON(http.StatusInternalServerError, "Webhook processing failed")
		return func() {}, false
	}
	r.Request = r.Request.WithContext(ctx)
	return release, true
}

func processResolvedMerchantWebhook(r *httprequest.Request, provider string, merchantID merchant.ID, accountID string) {
	if strings.TrimSpace(accountID) == "" {
		r.ErrorJSON(http.StatusBadRequest, "Webhook account_id is required")
		return
	}
	release, ok := pinWebhookMerchantConn(r, merchantID)
	if !ok {
		return
	}
	defer release()

	if rails.IsNMI(models.Rail(provider)) {
		if processMerchantNMIWebhook(r, provider, merchantID, accountID) {
			r.SuccessJSON(map[string]string{"status": "accepted"})
		}
		return
	}
	if provider == string(models.EventSourceBasisTheory) {
		if processMerchantBasisTheoryWebhook(r, merchantID, accountID) {
			r.SuccessJSON(map[string]string{"status": "accepted"})
		}
		return
	}
	if provider == subscriptions.RailCCBill {
		clientIP := r.ClientIP()
		if !ccbillWebhookIPAllowed(r, clientIP) {
			r.State.WebhookHealth.Rejected(r.Request.Context(), subscriptions.RailCCBill)
			r.ErrorJSON(http.StatusForbidden, "Unauthorized webhook source")
			return
		}
		if processMerchantCCBillWebhook(r, clientIP, accountID) {
			r.SuccessJSON(map[string]string{"status": "accepted"})
		}
		return
	}
	if provider != subscriptions.RailStripe {
		r.ErrorJSON(http.StatusBadRequest, "Provider not supported on merchant webhook surface")
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
			r.ErrorJSON(http.StatusServiceUnavailable, "Secret backend temporarily unavailable, retry")
			return
		}
		log.WithError(err).Error("merchant webhook: load merchant credentials failed")
		r.ErrorJSON(http.StatusInternalServerError, "Credential load failed")
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
	// #856: through an api_version rollover the superseded endpoint keeps
	// delivering with the OLD secret. Accepting it is what makes the rollover
	// gapless — deliveries already queued there still verify.
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
			r.State.WebhookHealth.Rejected(r.Request.Context(), subscriptions.RailStripe)
			r.ErrorJSON(http.StatusUnauthorized, "Invalid webhook signature")
		default:
			r.ErrorJSON(http.StatusBadRequest, "Invalid webhook payload")
		}
		return
	}
	// Stripe "thin" event destinations deliver a minimal payload without the
	// object. Hydrate it with the MERCHANT's secret key into the classic
	// {data:{object}} shape so dispatch only ever sees snapshot-style events.
	if hydrated, herr := hydrateThinStripeEvent(r.Request.Context(), strings.TrimSpace(creds.SecretKey), creds.AccountID, prepared.Body, r.State.StripeClients); herr != nil {
		if errors.Is(herr, errStripeWebhookAccountMismatch) {
			r.State.WebhookHealth.Rejected(r.Request.Context(), subscriptions.RailStripe)
			r.APIError(api.NewAPIError(http.StatusBadRequest, api.ErrorTypeInvalidRequest, "webhook_account_mismatch", "Webhook account does not match payload"))
			return
		}
		log.WithError(herr).Error("failed to hydrate thin stripe event")
		r.ErrorJSON(http.StatusBadGateway, "Failed to hydrate thin event")
		return
	} else if hydrated != nil {
		prepared.Body = hydrated
		prepared.EventID, prepared.EventType, err = webhookutil.ParseStripeEventMeta(hydrated)
		if err != nil {
			r.ErrorJSON(http.StatusBadGateway, "Invalid hydrated event")
			return
		}
	}
	r.State.WebhookHealth.Accepted(r.Request.Context(), subscriptions.RailStripe)
	if r.State.WebhookDispatcher == nil {
		r.ErrorJSON(http.StatusInternalServerError, "Webhook processing unavailable")
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
			r.SuccessJSON(map[string]string{"status": "accepted"})
			return
		}
		log.WithError(err).Error("merchant stripe webhook processing failed")
		r.ErrorJSON(http.StatusInternalServerError, "Webhook processing failed")
		return
	}
	r.SuccessJSON(map[string]string{"status": "accepted"})
}

func processPSPWebhook(r *httprequest.Request, rail, routeAccountID, clientIP string) (handled bool, accepted bool) {
	if strings.TrimSpace(routeAccountID) == "" {
		r.ErrorJSON(http.StatusBadRequest, "Webhook account_id is required")
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
			r.ErrorJSON(http.StatusBadRequest, "NMI webhook payload is missing merchant account identity")
			return true, false
		}
		if accountID != routeAccountID {
			r.ErrorJSON(http.StatusBadRequest, "Webhook account does not match payload")
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
			r.ErrorJSON(http.StatusForbidden, "Unauthorized webhook source")
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
			r.ErrorJSON(http.StatusBadRequest, "Webhook account does not match payload")
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
			r.ErrorJSON(http.StatusBadRequest, "Basis Theory webhook payload is missing tenant identity")
			return true, false
		}
		if tenantID != routeAccountID {
			r.ErrorJSON(http.StatusBadRequest, "Webhook account does not match payload")
			return true, false
		}
		// or#880: a custodian event routes by the CUSTODIAN's tenant identity.
		// It resolves a CUSTODIAN, not a PSP — one custodian may back several
		// PSPs, and the event is about the instrument, not about a gateway.
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
		// SEC-24 item 7: handled=true, accepted=FALSE. processResolvedMerchantWebhook
		// writes its OWN response — 401 on a failed signature, 200 on success.
		// Returning accepted=true made the caller write {"status":"accepted"}
		// again on top, so a body-parsing monitor saw a REJECTED FORGERY
		// reported as accepted. The status was always right; the body lied.
		processResolvedMerchantWebhook(r, subscriptions.RailStripe, account.MerchantID, account.AccountID)
		return true, false
	default:
		return false, false
	}
}

func webhookProviderEnvironment(r *httprequest.Request) string {
	return config.ExpectedProviderEnvironment(r != nil && r.State != nil && r.State.Config != nil && r.State.Config.IsTestMode())
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

// resolveWebhookCustodianAccount is the custody sibling: it resolves the
// CUSTODIAN a tenant identity belongs to (or#880) and pins its merchant.
// Unlike a rail-routed webhook it pins NO psp id — a custodian may back
// several PSPs, and a custodian event is about the instrument, not a gateway.
func resolveWebhookCustodianAccount(r *httprequest.Request, kind, environment, tenantID string) (merchants.CustodianIdentity, func(), bool) {
	noop := func() {}
	custodian, ok, err := r.State.Merchants.ResolveCustodianByIdentity(r.Request.Context(), kind, environment, tenantID)
	if err != nil {
		log.WithError(err).WithFields(log.Fields{"custodian": kind, "environment": environment, "account_id": tenantID}).Error("webhook custodian resolution failed")
		r.ErrorJSON(http.StatusInternalServerError, "Custodian resolution failed")
		return merchants.CustodianIdentity{}, noop, false
	}
	if !ok {
		rejectWebhook(r)
		return merchants.CustodianIdentity{}, noop, false
	}
	// or#893/or#795: pin the custodian the event demonstrably came from, the way
	// the PSP routes pin theirs. Nothing on this plane enqueues an intent today,
	// but anything that starts to is custodian-addressed by construction — the
	// event identifies a custodian that backs many PSPs, never one of them.
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
		r.ErrorJSON(http.StatusInternalServerError, "PSP resolution failed")
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

func processMerchantNMIWebhook(r *httprequest.Request, provider string, merchantID merchant.ID, accountID string) bool {
	body, ok := readLimitedWebhookBody(r, maxNMIWebhookBytes)
	if !ok {
		return false
	}
	return processMerchantNMIWebhookBody(r, provider, merchantID, accountID, body)
}

func processMerchantNMIWebhookBody(r *httprequest.Request, provider string, merchantID merchant.ID, accountID string, body []byte) bool {
	if strings.TrimSpace(accountID) == "" {
		r.ErrorJSON(http.StatusBadRequest, "Webhook account_id is required")
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
			r.ErrorJSON(http.StatusServiceUnavailable, "Secret backend temporarily unavailable, retry")
			return false
		}
		log.WithError(err).Error("merchant webhook: load nmi signing secret failed")
		r.ErrorJSON(http.StatusInternalServerError, "Credential load failed")
		return false
	}
	pspID, found, resolveErr := r.State.Merchants.ResolvePSPID(r.Request.Context(), merchantID, provider, accountID)
	if !bindResolvedWebhookPSP(r, pspID, found, resolveErr) {
		return false
	}
	// or#893: ONE signature header. NMI sends `Webhook-Signature: t=<ts>,s=<hex>`
	// (docs/rails/nmi.md, live-verified in tests/nmi_webhook_signature_http_test.go),
	// and the embedded service seam has only ever read that name. The three
	// X-… spellings were speculative aliases: accepting them widened the set of
	// headers an attacker could aim a forged signature at for no gateway that
	// ever sends them.
	header := strings.TrimSpace(r.Request.Header.Get("Webhook-Signature"))
	signingKey := keys.Current
	prepared, err := webhookutil.PrepareNMI(provider, body, signingKey, header)
	// SEC-29: the rotated-out secret verifies only inside its bounded overlap.
	if errors.Is(err, webhookutil.ErrNMIWebhookSignatureInvalid) && strings.TrimSpace(keys.Previous) != "" {
		if again, againErr := webhookutil.PrepareNMI(provider, body, keys.Previous, header); againErr == nil {
			prepared, err, signingKey = again, nil, keys.Previous
		}
	}
	if err != nil {
		switch {
		case errors.Is(err, webhookutil.ErrNMIWebhookSecretMissing),
			errors.Is(err, webhookutil.ErrNMIWebhookSignatureMissing):
			r.State.WebhookHealth.Rejected(r.Request.Context(), string(models.RailNMI))
			r.ErrorJSON(http.StatusUnauthorized, "Missing webhook signature")
		case errors.Is(err, webhookutil.ErrNMIWebhookSignatureInvalid):
			r.State.WebhookHealth.Rejected(r.Request.Context(), string(models.RailNMI))
			r.ErrorJSON(http.StatusUnauthorized, "Invalid webhook signature")
		case errors.Is(err, webhookutil.ErrWebhookPayloadInvalid):
			r.ErrorJSON(http.StatusBadRequest, "Invalid JSON data")
		case errors.Is(err, webhookutil.ErrWebhookEventIDMissing):
			r.ErrorJSON(http.StatusBadRequest, "Missing event_id in payload")
		default:
			r.ErrorJSON(http.StatusBadRequest, "Invalid webhook payload")
		}
		return false
	}
	if payloadAccount := nmiWebhookAccountID(body); payloadAccount != "" && payloadAccount != accountID {
		r.ErrorJSON(http.StatusBadRequest, "Webhook account does not match payload")
		return false
	}
	r.State.WebhookHealth.Accepted(r.Request.Context(), prepared.Rail)
	if r.State.WebhookDispatcher == nil {
		r.ErrorJSON(http.StatusInternalServerError, "Webhook processing unavailable")
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
		r.ErrorJSON(http.StatusInternalServerError, "Webhook processing failed")
		return false
	}
	return true
}

// Credentials alone do not identify the account that will own the receipt.
// Every merchant-specific dispatcher must retain a concrete persisted PSP.
func bindResolvedWebhookPSP(r *httprequest.Request, id uuid.UUID, found bool, err error) bool {
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, "Webhook account resolution failed")
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
		r.ErrorJSON(http.StatusBadRequest, "Webhook account does not match payload")
		return false
	}
	mid, err := merchant.Require(r.Request.Context())
	if err != nil || r.State.Merchants == nil {
		r.ErrorJSON(http.StatusServiceUnavailable, "Merchant webhook routing is not configured")
		return false
	}
	pspID, found, err := r.State.Merchants.ResolvePSPID(r.Request.Context(), mid, subscriptions.RailCCBill, routeAccountID)
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, "Webhook account resolution failed")
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
			r.ErrorJSON(http.StatusBadRequest, "Invalid webhook payload")
		case errors.Is(err, webhookutil.ErrWebhookEventTypeMissing):
			r.ErrorJSON(http.StatusBadRequest, "Missing eventType parameter")
		case errors.Is(err, webhookutil.ErrWebhookEventTypeMismatch):
			r.ErrorJSON(http.StatusBadRequest, "Webhook event type mismatch")
		default:
			r.ErrorJSON(http.StatusBadRequest, "Invalid webhook payload")
		}
		return webhookutil.Prepared{}, "", false
	}
	accountID := ccbillWebhookAccountID(prepared.Body)
	if accountID == "" {
		r.ErrorJSON(http.StatusBadRequest, "CCBill webhook payload is missing client account identity")
		return webhookutil.Prepared{}, "", false
	}
	return prepared, accountID, true
}

func processMerchantCCBillWebhookPrepared(r *httprequest.Request, clientIP string, prepared webhookutil.Prepared, accountID string) bool {
	// CCBill has no HMAC: IP-allowlisted + well-formed IS its verified-accepted.
	r.State.WebhookHealth.Accepted(r.Request.Context(), subscriptions.RailCCBill)
	if r.State.WebhookDispatcher == nil {
		r.ErrorJSON(http.StatusInternalServerError, "Webhook processing unavailable")
		return false
	}
	// Stamp the routed PSP so every row this event materialises is attributable
	// (or#893). The per-account route pins it in resolveWebhookPSP;
	// the merchant-slug route resolves it from the payload's own account identity.
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
			r.SuccessJSON(map[string]string{"status": "refused", "code": code})
			return false
		}
		if webhooks.IsWebhookErrorNonRetryable(err) {
			return true
		}
		log.WithError(err).Error("merchant ccbill webhook processing failed")
		r.ErrorJSON(http.StatusInternalServerError, "Webhook processing failed")
		return false
	}
	return true
}

// ccbillWebhookMessage builds the dispatch message for a CCBill event. CCBill
// has no signature — authentication is the source-IP allowlist — so
// SignatureValid is deliberately left nil (never claimed, #668), matching the
// River path (Prepared.QueueArgs on an unverified Prepared).
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
	// #697: composite CCBill identity is dash-joined (clientAccnum-clientSubacc),
	// matching CCBill's own convention and the declared account_id format.
	return clientAccnum + "-" + clientSubacc
}

// canonicalWebhookRail resolves the URL's rail segment, writing a 400 with the
// rename when the segment is a retired alias (or#893). The rail segment is the
// gateway kind; a PSP is named by :account_id or the payload's account identity.
func canonicalWebhookRail(r *httprequest.Request) (string, bool) {
	provider, err := webhookutil.CanonicalRail(r.Param("provider"))
	if err != nil {
		r.ErrorJSON(http.StatusBadRequest, err.Error())
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
// signature identically (SEC-33), so a webhook route never reveals which
// provider accounts a deployment serves.
func rejectWebhook(r *httprequest.Request) {
	r.ErrorJSON(http.StatusUnauthorized, "Invalid webhook signature")
}
