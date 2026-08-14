package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	neturl "net/url"
	"path"
	"strings"
	"time"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/models"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/integrations/stripeapi"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/internal/modules/payments/rails"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/internal/modules/webhooks"
	"github.com/open-rails/openrails/internal/shared/iputil"
	"github.com/open-rails/openrails/internal/shared/webhookutil"
	"github.com/open-rails/openrails/internal/webhookauth"
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
	// The global webhook surface attributes events only to the construction-time
	// configured merchant (set by an embedded host scoped to one merchant). A standalone
	// multi-merchant deployment pins no process-wide merchant, so there is no
	// merchant to attribute this event to — fail closed with an explicit pointer
	// to the per-merchant surface rather than letting a downstream merchant.Require
	// surface a generic error (audit OR-API-C2).
	mid, err := merchant.Require(r.Request.Context())
	if err != nil {
		if handled, accepted := processPSPWebhook(r, provider, strings.TrimSpace(r.Param("account_id")), clientIP); handled {
			if accepted {
				r.SuccessJSON(map[string]string{"status": "accepted"})
			}
			return
		}
		log.WithFields(log.Fields{"provider": provider, "client_ip": clientIP}).
			Warn("global webhook surface hit with no configured merchant and no resolvable PSP")
		r.ErrorJSON(http.StatusNotFound, "No merchant is configured for the global webhook surface and no PSP could be resolved from the webhook")
		return
	}
	// #788: ONE ingestion seam — the pinned-merchant global surface routes
	// through the SAME verify/dispatch primitive as the merchant-scoped and
	// Host-routed surfaces, resolving credentials from the armed rail state.
	processResolvedMerchantWebhook(r, provider, mid, strings.TrimSpace(r.Param("account_id")))
}

func MerchantWebhook(r *httprequest.Request) {
	if r.State == nil || r.State.Merchants == nil {
		r.ErrorJSON(http.StatusServiceUnavailable, "Merchant webhook routing is not configured")
		return
	}
	provider, ok := canonicalWebhookRail(r)
	if !ok {
		return
	}
	route, err := r.State.Merchants.ResolveBySlug(r.Request.Context(), r.Param("merchant"))
	if err != nil {
		if errors.Is(err, merchants.ErrMerchantRouteUnresolved) {
			r.ErrorJSON(http.StatusNotFound, "Unknown merchant")
			return
		}
		log.WithError(err).Error("merchant webhook: resolve merchant failed")
		r.ErrorJSON(http.StatusInternalServerError, "Merchant resolution failed")
		return
	}
	ctx := merchant.WithID(r.Request.Context(), route.MerchantID)
	r.Request = r.Request.WithContext(ctx)
	processResolvedMerchantWebhook(r, provider, route.MerchantID, strings.TrimSpace(r.Param("account_id")))
}

// HostWebhook returns the Host-routed webhook handler (#734): resolve pins the
// merchant from the request's Host header — the SAME resolver merchant-scoped
// route resolution and the issuer-consistency check use — instead of a URL
// slug (contrast MerchantWebhook), so
// the exact same verify/dispatch primitive (processResolvedMerchantWebhook)
// runs unchanged once the merchant is known. This is the engine half of saas
// #15's "api.<slug>.<domain>" hostname scheme: the mount is opt-in
// (pkg/embedded's RegisterHostWebhookRoutes, gated on an attached control
// plane); an unresolvable Host (unknown/disabled/ambiguous merchant) is a hard
// 404, never a fall-through to payload-derived resolution.
func HostWebhook(resolve merchant.HostResolver) func(r *httprequest.Request) {
	return func(r *httprequest.Request) {
		if resolve == nil {
			r.ErrorJSON(http.StatusServiceUnavailable, "Host-routed webhook resolution is not configured")
			return
		}
		provider, ok := canonicalWebhookRail(r)
		if !ok {
			return
		}
		mid, err := resolve(r.Request.Context(), r.Request.Host)
		if err != nil || mid.IsZero() {
			r.ErrorJSON(http.StatusNotFound, "Unknown merchant")
			return
		}
		ctx := merchant.WithID(r.Request.Context(), mid)
		r.Request = r.Request.WithContext(ctx)
		processResolvedMerchantWebhook(r, provider, mid, strings.TrimSpace(r.Param("account_id")))
	}
}

// pinWebhookMerchantConn pins the resolved merchant's DB connection (the
// app.merchant_id GUC) on the request for the rest of the dispatch, and returns
// the release the caller must defer.
//
// The webhook surfaces resolve their merchant INSIDE the handler — from the URL
// slug (MerchantWebhook), the Host header (HostWebhook) or the payload's account
// identity (processPSPWebhook) — so middleware.MerchantDBConnMW
// cannot have run: at middleware time there is no merchant to pin. Without this,
// every RLS-forced read the dispatch performs outside a MerchantTx (the price
// lookup behind a CCBill NewSaleSuccess, subscription/customer lookups) runs on
// the base pool and, under the production openrails_app role, matches ZERO ROWS
// AND RAISES NOTHING — the webhook fails as "price not found" for a price that
// exists. Writes were never affected (they go through MerchantTx), which is why
// this only ever showed up as a phantom missing row.
//
// Nested calls are a no-op (db.WithMerchantConn returns the existing pin), so
// the Stripe-by-account path that re-enters processResolvedMerchantWebhook is
// safe.
func pinWebhookMerchantConn(r *httprequest.Request, merchantID merchant.ID) (func(), bool) {
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
		if processMerchantCCBillWebhook(r, clientIP) {
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
	if accountID != "" {
		// #641: per-account endpoint — verify with THIS Stripe account's secret and
		// stamp records with it. Unknown account is rejected (no primary fallback).
		var found bool
		creds, found, err = r.State.Merchants.LoadStripeCredentialsForAccount(r.Request.Context(), merchantID, accountID)
		if err == nil && !found {
			r.ErrorJSON(http.StatusNotFound, "Unknown PSP")
			return
		}
		if err == nil && found {
			if pid, ok, rerr := r.State.Merchants.ResolvePSPID(r.Request.Context(), merchantID, provider, accountID); rerr == nil && ok {
				r.Request = r.Request.WithContext(db.WithPSPID(r.Request.Context(), pid))
			}
		}
	} else {
		creds, err = r.State.Merchants.LoadStripeCredentials(r.Request.Context(), merchantID)
		// or#893: stamp with the account whose secret verifies this event — the
		// same scope LoadStripeCredentials just resolved. Rows this event creates
		// must be attributed, and this is the only account it can have come from.
		if err == nil {
			if pid, ok, rerr := r.State.Merchants.ResolveActivePSPIDForRail(r.Request.Context(), merchantID, provider); rerr == nil && ok {
				r.Request = r.Request.WithContext(db.WithPSPID(r.Request.Context(), pid))
			}
		}
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
		r.ErrorJSON(http.StatusUnauthorized, "Merchant webhook signing secret not configured")
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
	r.State.WebhookHealth.Accepted(r.Request.Context(), subscriptions.RailStripe)
	// Stripe "thin" event destinations deliver a minimal payload without the
	// object. Hydrate it with the MERCHANT's secret key into the classic
	// {data:{object}} shape so dispatch only ever sees snapshot-style events.
	if hydrated, herr := hydrateThinStripeEvent(r.Request.Context(), strings.TrimSpace(creds.SecretKey), prepared.Body); herr != nil {
		log.WithError(herr).Error("failed to hydrate thin stripe event")
		r.ErrorJSON(http.StatusBadGateway, "Failed to hydrate thin event")
		return
	} else if hydrated != nil {
		prepared.Body = hydrated
	}
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
		r.ErrorJSON(http.StatusNotFound, "Unknown custodian account")
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
		r.ErrorJSON(http.StatusNotFound, "Unknown PSP")
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
	var signingKey string
	var err error
	if accountID != "" {
		// #641: per-account endpoint — verify with THIS account's secret only;
		// an unknown account is rejected (no fallback to the primary).
		var found bool
		signingKey, found, err = r.State.Merchants.LoadNMIWebhookSigningSecretForAccount(r.Request.Context(), merchantID, accountID)
		if err == nil && !found {
			r.ErrorJSON(http.StatusNotFound, "Unknown PSP")
			return false
		}
		// Pin the routed account so records this event creates are stamped with it
		// (overriding the repo's primary-default stamping).
		if err == nil && found {
			if pid, ok, rerr := r.State.Merchants.ResolvePSPID(r.Request.Context(), merchantID, provider, accountID); rerr == nil && ok {
				r.Request = r.Request.WithContext(db.WithPSPID(r.Request.Context(), pid))
			}
		}
	} else {
		signingKey, err = r.State.Merchants.LoadNMIWebhookSigningSecret(r.Request.Context(), merchantID, provider)
		// or#893: see the Stripe branch — attribute with the account whose secret
		// verifies the event.
		if err == nil {
			if pid, ok, rerr := r.State.Merchants.ResolveActivePSPIDForRail(r.Request.Context(), merchantID, provider); rerr == nil && ok {
				r.Request = r.Request.WithContext(db.WithPSPID(r.Request.Context(), pid))
			}
		}
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
	// or#893: ONE signature header. NMI sends `Webhook-Signature: t=<ts>,s=<hex>`
	// (docs/rails/nmi.md, live-verified in tests/nmi_webhook_signature_http_test.go),
	// and the embedded service seam has only ever read that name. The three
	// X-… spellings were speculative aliases: accepting them widened the set of
	// headers an attacker could aim a forged signature at for no gateway that
	// ever sends them.
	prepared, err := webhookutil.PrepareNMI(provider, body, signingKey, strings.TrimSpace(r.Request.Header.Get("Webhook-Signature")))
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

func processMerchantCCBillWebhook(r *httprequest.Request, clientIP string) bool {
	body, ok := readLimitedWebhookBody(r, maxCCBillWebhookBytes)
	if !ok {
		return false
	}
	prepared, accountID, ok := prepareCCBillWebhookWithAccountID(r, body)
	if !ok {
		return false
	}
	// or#893: the payload's clientAccnum-clientSubacc IS the CCBill account
	// identity, and prepare above already refuses a payload without it — so this
	// route knows exactly which PSP sent the event and must not discard it. The
	// per-account route resolves the same thing through the URL.
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

	// SEC-24 item 5. The payload-supplied value is ALWAYS a path, never a URL.
	// The previous `if !strings.HasPrefix(url, "http")` was the inverse of a
	// guard: an absolute URL was accepted verbatim and then handed
	// `Authorization: Bearer sk_live_…`, so a leaked webhook secret escalated
	// into live-API-key exfiltration. The host is ours and is not negotiable.
	url, err := stripeRelatedObjectURL(envelope.RelatedObject.URL)
	if err != nil {
		return nil, err
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

// stripeAPIBase is the ONE host thin-event hydration may reach. Not derived
// from the payload, not configurable per request.
const stripeAPIBase = "https://api.stripe.com"

// stripeRelatedObjectURL treats the payload's related-object value strictly as a
// PATH under the Stripe API host. Anything that could redirect the request
// elsewhere — a scheme, a protocol-relative "//host", a backslash, a control
// character — is refused rather than normalised, because a normaliser is
// something to be outwitted and a refusal is not.
func stripeRelatedObjectURL(raw string) (string, error) {
	p := strings.TrimSpace(raw)
	if p == "" {
		return "", fmt.Errorf("thin event related object has no url")
	}
	if strings.ContainsAny(p, "\\\x00") || strings.ContainsRune(p, '\n') || strings.ContainsRune(p, '\r') {
		return "", fmt.Errorf("thin event related object url is not a plain path")
	}
	if strings.Contains(p, "://") || strings.HasPrefix(p, "//") {
		return "", fmt.Errorf("thin event related object url must be a path, not an absolute or protocol-relative url")
	}
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	// A parsed path must stay a path: no host, no scheme, no ".." escape.
	u, err := neturl.Parse(p)
	if err != nil {
		return "", fmt.Errorf("thin event related object url is unparseable: %w", err)
	}
	if u.Scheme != "" || u.Host != "" || u.User != nil {
		return "", fmt.Errorf("thin event related object url must be a path, not an absolute url")
	}
	if u.Path != path.Clean(u.Path) {
		return "", fmt.Errorf("thin event related object path is not canonical")
	}
	return stripeAPIBase + u.String(), nil
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
