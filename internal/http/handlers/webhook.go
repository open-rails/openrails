package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/api"
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/models"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/integrations/basistheory"
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
	// The account's identity selects the merchant: (rail, environment,
	// account_id) names one PSP, whose secret checks the signature.
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
// no-op.
func pinWebhookMerchantConn(r *httprequest.Request, merchantID billing.MerchantID) (func(), bool) {
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

// dispatchStripeWebhook dispatches a Stripe delivery verified for the pinned
// PSP and writes its response.
func dispatchStripeWebhook(r *httprequest.Request, signed stripeSigned, accountID string) {
	prepared, creds := signed.prepared, signed.creds
	// Stripe "thin" event destinations deliver a minimal payload without the
	// object. Hydrate it with the MERCHANT's secret key into the classic
	// {data:{object}} shape so dispatch only ever sees snapshot-style events.
	if hydrated, herr := hydrateThinStripeEvent(r.Request.Context(), strings.TrimSpace(creds.SecretKey), creds.AccountID, prepared.Body, r.State.StripeClients); herr != nil {
		if errors.Is(herr, errStripeWebhookAccountMismatch) {
			r.State.WebhookHealth.Rejected(r.Request.Context())
			r.APIError(api.NewAPIError(http.StatusBadRequest, api.ErrorTypeInvalidRequest, billing.CodeWebhookAccountMismatch, "Webhook account does not match payload"))
			return
		}
		log.WithError(herr).Error("failed to hydrate thin stripe event")
		r.ErrorCode(billing.CodeServiceUnavailable, "the provider event could not be read; retry")
		return
	} else if hydrated != nil {
		var err error
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
	if rail == subscriptions.RailCCBill && !ccbillWebhookIPAllowed(r, clientIP) {
		r.ErrorCode(billing.CodeResourceAccessDenied, "Unauthorized webhook source")
		return true, false
	}
	if r.State == nil || r.State.Merchants == nil {
		return false, false
	}
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
		account, signed, release, ok := routeSignedWebhook(r, string(models.RailNMI), accountID, checkNMIWebhook(r, body), nil, refuseNMIWebhook(r))
		if !ok {
			return true, false
		}
		defer release()
		return true, dispatchNMIWebhook(r, signed, account.AccountID)
	case rail == subscriptions.RailCCBill:
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
		// CCBill signs nothing (the source-IP allowlist authenticated the
		// delivery), so a live and a non-live PSP of one account cannot be
		// told apart.
		unsigned := func(context.Context, merchants.PSPIdentity) (signedWebhook[struct{}], error) {
			return signedWebhook[struct{}]{keyed: true}, nil
		}
		account, _, release, ok := routeSignedWebhook(r, subscriptions.RailCCBill, accountID, unsigned, nil, func(error) bool { rejectWebhook(r); return false })
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
		custodian, release, ok := resolveWebhookCustodianAccount(r, models.CustodianBasisTheory, tenantID, body)
		if !ok {
			return true, false
		}
		defer release()
		return true, processMerchantBasisTheoryWebhookBody(r, custodian.MerchantID, custodian.AccountID, custodian.Environment, body)
	case rail == subscriptions.RailStripe && routeAccountID != "":
		body, ok := readLimitedWebhookBody(r, maxStripeWebhookBytes)
		if !ok {
			return true, false
		}
		account, signed, release, ok := routeSignedWebhook(r, subscriptions.RailStripe, routeAccountID, checkStripeWebhook(r, body), stripeLivemodeMatches, refuseStripeWebhook(r))
		if !ok {
			return true, false
		}
		defer release()
		// handled=true, accepted=false: dispatchStripeWebhook writes its own
		// response (200 on success), so the caller must not write "accepted"
		// on top of it.
		dispatchStripeWebhook(r, signed, account.AccountID)
		return true, false
	default:
		return false, false
	}
}

// ccbillWebhookIPAllowed binds the request to the ONE CCBill source-IP gate
// (webhookauth.CCBillIPAllowed) that the embedded Service surface also calls.
func ccbillWebhookIPAllowed(r *httprequest.Request, clientIP string) bool {
	if r == nil || r.State == nil {
		return iputil.IsValidCCBillIP(clientIP)
	}
	return webhookauth.CCBillIPAllowed(r.Request.Context(), r.State.Config, ccbillAccountProbe(r), clientIP)
}

// ccbillAccountProbe asks whether the CCBill account the webhook names moves
// real money; a var so handler tests can stub it. Returning nil (no merchants
// service) fails the gate closed.
var ccbillAccountProbe = func(r *httprequest.Request) webhookauth.LiveRailProbe {
	if r.State.Merchants == nil {
		return nil
	}
	account := strings.TrimSpace(r.Param("account_id"))
	return func(ctx context.Context) (merchants.LiveRailPresence, error) {
		return r.State.Merchants.ProbeLiveAccount(ctx, "ccbill", account)
	}
}

// livePSPProbe asks whether the request's merchant has a live card PSP; a var so handler
// tests can stub it. Returning nil fails every gate on it closed.
var livePSPProbe = func(r *httprequest.Request) webhookauth.LiveRailProbe {
	if r.State.Merchants == nil {
		return nil
	}
	return func(ctx context.Context) (merchants.LiveRailPresence, error) {
		id, err := merchant.Require(ctx)
		if err != nil {
			return merchants.LiveRailUnknown, err
		}
		return r.State.Merchants.ProbeLiveCardPSP(ctx, id)
	}
}

// signedWebhook is what checking a delivery against one candidate PSP's
// secrets found.
type signedWebhook[T any] struct {
	verified T
	// keyed: the PSP declares a secret to check with. mismatch: only the
	// signature failed, made with another secret.
	keyed, mismatch bool
	// err is why the delivery did not verify; nil when it did.
	err error
}

// routeSignedWebhook resolves the PSP an account-routed delivery belongs to
// and pins its merchant, its id and the merchant's DB connection; release
// must be deferred (see pinWebhookMerchantConn). An account may be a live and
// a non-live PSP (a Stripe acct_ is both), so the delivery belongs to the one
// whose secrets verify it: every candidate is checked, one none verifies is
// refused, and one both verify is attributed by tieBreak or not at all.
// refuse answers a refused delivery and reports whether it failed
// verification.
func routeSignedWebhook[T any](r *httprequest.Request, rail, accountID string,
	check func(context.Context, merchants.PSPIdentity) (signedWebhook[T], error),
	tieBreak func(merchants.PSPIdentity, T) bool,
	refuse func(error) bool,
) (merchants.PSPIdentity, T, func(), bool) {
	noop := func() {}
	var none T
	ctx := r.Request.Context()
	fields := log.Fields{"rail": rail, "account_id": accountID}
	candidates, err := r.State.Merchants.ResolvePSPsByAccount(ctx, rail, accountID)
	if err != nil {
		log.WithError(err).WithFields(fields).Error("webhook PSP resolution failed")
		r.ErrorCode(billing.CodeInternalError, "PSP resolution failed")
		return merchants.PSPIdentity{}, none, noop, false
	}
	if len(candidates) == 0 {
		rejectWebhook(r)
		return merchants.PSPIdentity{}, none, noop, false
	}
	checks := make([]signedWebhook[T], len(candidates))
	var won []int
	for i, psp := range candidates {
		c, err := check(merchant.WithID(ctx, psp.MerchantID), psp)
		if err != nil {
			if errors.Is(err, merchants.ErrConfigUnavailable) {
				r.ErrorCode(billing.CodeServiceUnavailable, "Merchant configuration temporarily unavailable, retry")
			} else {
				log.WithError(err).WithFields(fields).Error("merchant webhook: load PSP secrets failed")
				r.ErrorCode(billing.CodeInternalError, "Credential load failed")
			}
			return merchants.PSPIdentity{}, none, noop, false
		}
		checks[i] = c
		if c.err == nil {
			won = append(won, i)
		}
	}
	if len(won) > 1 && tieBreak != nil {
		won = slices.DeleteFunc(won, func(i int) bool { return !tieBreak(candidates[i], checks[i].verified) })
	}
	switch len(won) {
	case 1:
		psp := candidates[won[0]]
		r.Request = r.Request.WithContext(db.WithPSPID(merchant.WithID(ctx, psp.MerchantID), psp.ID))
		release, ok := pinWebhookMerchantConn(r, psp.MerchantID)
		if !ok {
			return merchants.PSPIdentity{}, none, noop, false
		}
		return psp, checks[won[0]].verified, release, true
	case 0:
		report := -1
		for i, c := range checks {
			if c.keyed && (report < 0 || checks[report].mismatch && !c.mismatch) {
				report = i
			}
		}
		if report < 0 {
			warnUndeclaredWebhook(rail, accountID)
			report = 0
		}
		if refuse(checks[report].err) {
			for _, psp := range candidates {
				r.State.WebhookHealth.Rejected(db.WithPSPID(merchant.WithID(ctx, psp.MerchantID), psp.ID))
			}
		}
		return merchants.PSPIdentity{}, none, noop, false
	}
	log.WithFields(fields).Error("webhook refused: the delivery verifies for both the live and the non-live PSP of the account, so it cannot be attributed to either")
	r.ErrorCode(billing.CodeInternalError, "PSP resolution failed")
	return merchants.PSPIdentity{}, none, noop, false
}

// resolveWebhookCustodianAccount resolves the custodian a tenant identity
// belongs to and pins its merchant, but no PSP id: a custodian may back
// several PSPs, and its event is about the instrument, not a gateway. A tenant
// that is a custodian in both money classes is the one whose class's key
// verifies the event.
func resolveWebhookCustodianAccount(r *httprequest.Request, kind, tenantID string, body []byte) (merchants.CustodianIdentity, func(), bool) {
	noop := func() {}
	ctx := r.Request.Context()
	candidates, err := r.State.Merchants.ResolveCustodiansByIdentity(ctx, kind, tenantID)
	if err != nil {
		log.WithError(err).WithFields(log.Fields{"custodian": kind, "account_id": tenantID}).Error("webhook custodian resolution failed")
		r.ErrorCode(billing.CodeInternalError, "Custodian resolution failed")
		return merchants.CustodianIdentity{}, noop, false
	}
	if len(candidates) == 0 {
		rejectWebhook(r)
		return merchants.CustodianIdentity{}, noop, false
	}
	custodian := candidates[0]
	if len(candidates) > 1 {
		sig, version := r.Header(basistheory.SignatureHeader), r.Header(basistheory.SignatureVersionHeader)
		var won []merchants.CustodianIdentity
		for _, c := range candidates {
			if basisTheoryVerifier(c.Environment).Verify(ctx, body, sig, version) == nil {
				won = append(won, c)
			}
		}
		switch len(won) {
		case 1:
			custodian = won[0]
		case 0:
			for _, c := range candidates {
				r.State.WebhookHealth.Rejected(db.WithCustodianID(merchant.WithID(ctx, c.MerchantID), c.ID))
			}
			r.ErrorCode(billing.CodeAuthenticationRequired, "Invalid webhook signature")
			return merchants.CustodianIdentity{}, noop, false
		default:
			log.WithFields(log.Fields{"custodian": kind, "account_id": tenantID}).Error("webhook refused: the event verifies for both money classes of the tenant")
			r.ErrorCode(billing.CodeInternalError, "Custodian resolution failed")
			return merchants.CustodianIdentity{}, noop, false
		}
	}
	// Pin the custodian the event came from, as the PSP routes pin theirs, so
	// anything enqueued here is custodian-addressed, never one of its PSPs.
	ctx = merchant.WithID(ctx, custodian.MerchantID)
	r.Request = r.Request.WithContext(db.WithCustodianID(ctx, custodian.ID))
	release, ok := pinWebhookMerchantConn(r, custodian.MerchantID)
	if !ok {
		return merchants.CustodianIdentity{}, noop, false
	}
	return custodian, release, true
}

// nmiSigned is an NMI delivery and the key that verified it.
type nmiSigned struct {
	prepared webhookutil.Prepared
	key      string
}

// checkNMIWebhook checks an NMI delivery against one PSP's secrets.
func checkNMIWebhook(r *httprequest.Request, body []byte) func(context.Context, merchants.PSPIdentity) (signedWebhook[nmiSigned], error) {
	// NMI sends one signature header, `Webhook-Signature: t=<ts>,s=<hex>`
	// (docs/rails/nmi.md). No other spelling is accepted, so a forged
	// signature has one header to aim at.
	header := strings.TrimSpace(r.Request.Header.Get("Webhook-Signature"))
	rail := string(models.RailNMI)
	return func(ctx context.Context, psp merchants.PSPIdentity) (signedWebhook[nmiSigned], error) {
		keys, err := r.State.Merchants.LoadNMIWebhookSecrets(ctx, psp.MerchantID, psp.ID)
		if err != nil {
			return signedWebhook[nmiSigned]{}, err
		}
		key := keys.Current
		prepared, err := webhookutil.PrepareNMI(rail, body, key, header)
		// The rotated-out secret verifies only inside its bounded overlap.
		if errors.Is(err, webhookutil.ErrNMIWebhookSignatureInvalid) && strings.TrimSpace(keys.Previous) != "" {
			if again, againErr := webhookutil.PrepareNMI(rail, body, keys.Previous, header); againErr == nil {
				prepared, err, key = again, nil, keys.Previous
			}
		}
		return signedWebhook[nmiSigned]{
			verified: nmiSigned{prepared: prepared, key: key},
			keyed:    strings.TrimSpace(keys.Current) != "",
			mismatch: errors.Is(err, webhookutil.ErrNMIWebhookSignatureInvalid),
			err:      err,
		}, nil
	}
}

// refuseNMIWebhook answers an NMI delivery no PSP verified.
func refuseNMIWebhook(r *httprequest.Request) func(error) bool {
	return func(err error) bool {
		switch {
		case errors.Is(err, webhookutil.ErrNMIWebhookSecretMissing),
			errors.Is(err, webhookutil.ErrNMIWebhookSignatureMissing):
			r.ErrorCode(billing.CodeAuthenticationRequired, "Missing webhook signature")
			return true
		case errors.Is(err, webhookutil.ErrNMIWebhookSignatureInvalid):
			r.ErrorCode(billing.CodeAuthenticationRequired, "Invalid webhook signature")
			return true
		case errors.Is(err, webhookutil.ErrWebhookPayloadInvalid):
			r.ErrorCode(billing.CodeInvalidParam, "Invalid JSON data")
		case errors.Is(err, webhookutil.ErrWebhookEventIDMissing):
			r.ErrorCode(billing.CodeInvalidParam, "Missing event_id in payload")
		default:
			r.ErrorCode(billing.CodeInvalidParam, "Invalid webhook payload")
		}
		return false
	}
}

// dispatchNMIWebhook dispatches an NMI delivery verified for the pinned PSP.
func dispatchNMIWebhook(r *httprequest.Request, signed nmiSigned, accountID string) bool {
	prepared := signed.prepared
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
		SigningSecret:  signed.key,
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

// stripeSigned is a Stripe delivery and the credentials of the PSP that
// verified it.
type stripeSigned struct {
	prepared webhookutil.Prepared
	creds    merchants.StripeCredentials
}

var errStripeWebhookUnkeyed = errors.New("no webhook signing secret is declared")

// checkStripeWebhook checks a Stripe delivery against one PSP's secrets.
func checkStripeWebhook(r *httprequest.Request, body []byte) func(context.Context, merchants.PSPIdentity) (signedWebhook[stripeSigned], error) {
	header := r.Header("Stripe-Signature")
	return func(ctx context.Context, psp merchants.PSPIdentity) (signedWebhook[stripeSigned], error) {
		creds, err := r.State.Merchants.LoadStripeCredentialsForPSP(ctx, psp.MerchantID, psp.ID)
		if err != nil {
			return signedWebhook[stripeSigned]{}, err
		}
		var secrets []string
		if s := strings.TrimSpace(creds.WebhookSigningSecret); s != "" {
			secrets = append(secrets, s)
		}
		if s := strings.TrimSpace(creds.WebhookSigningThin); s != "" {
			secrets = append(secrets, s)
		}
		// Through an api_version rollover the superseded endpoint keeps
		// delivering with the previous secret; accepting it keeps the rollover
		// gapless.
		if s := strings.TrimSpace(creds.WebhookSigningPrevious); s != "" {
			secrets = append(secrets, s)
		}
		if len(secrets) == 0 {
			return signedWebhook[stripeSigned]{err: errStripeWebhookUnkeyed}, nil
		}
		prepared, err := prepareStripeMultiSecret(body, secrets, header, 5*time.Minute)
		return signedWebhook[stripeSigned]{
			verified: stripeSigned{prepared: prepared, creds: creds},
			keyed:    true,
			mismatch: errors.Is(err, webhookutil.ErrWebhookSignatureInvalid),
			err:      err,
		}, nil
	}
}

// refuseStripeWebhook answers a Stripe delivery no PSP verified.
func refuseStripeWebhook(r *httprequest.Request) func(error) bool {
	return func(err error) bool {
		switch {
		case errors.Is(err, errStripeWebhookUnkeyed):
			rejectWebhook(r)
		case errors.Is(err, webhookutil.ErrWebhookSignatureRequired),
			errors.Is(err, webhookutil.ErrWebhookSignatureMissing),
			errors.Is(err, webhookutil.ErrWebhookSignatureInvalid):
			r.ErrorCode(billing.CodeAuthenticationRequired, "Invalid webhook signature")
			return true
		default:
			r.ErrorCode(billing.CodeInvalidParam, "Invalid webhook payload")
		}
		return false
	}
}

// stripeLivemodeMatches attributes a delivery both of an account's PSPs
// verify: Stripe says in livemode which one it is.
func stripeLivemodeMatches(psp merchants.PSPIdentity, signed stripeSigned) bool {
	var event struct {
		Livemode *bool `json:"livemode"`
	}
	if json.Unmarshal(signed.prepared.Body, &event) != nil || event.Livemode == nil {
		return false
	}
	return *event.Livemode == config.LiveEnvironment(psp.Rail, psp.Environment)
}

// WebhookQuery is a provider callback's query: CCBill names its event type
// there.
type WebhookQuery struct {
	EventType string `form:"eventType"`
}

func prepareCCBillWebhookWithAccountID(r *httprequest.Request, body []byte) (webhookutil.Prepared, string, bool) {
	var query WebhookQuery
	if !r.BindQuery(&query) {
		return webhookutil.Prepared{}, "", false
	}
	prepared, err := webhookutil.PrepareCCBill(body, query.EventType)
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
	// The caller pinned the routed PSP, so every row this event materialises
	// is attributable.
	ctx := r.Request.Context()
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
// warnUndeclaredWebhook says why an event for one of the merchant's accounts
// is refused: no declared credentials verify it, so it is never recorded.
func warnUndeclaredWebhook(rail, accountID string) {
	log.WithFields(log.Fields{"rail": rail, "account_id": accountID}).
		Warn("webhook refused: the account has no declared webhook signing secret (undeclared or missing), so its events are not recorded")
}

func rejectWebhook(r *httprequest.Request) {
	r.ErrorCode(billing.CodeAuthenticationRequired, "Invalid webhook signature")
}

// WebhookReceipt answers a provider's webhook: accepted, or refused with the
// registered code of a refusal the provider should not retry.
type WebhookReceipt struct {
	Status string  `json:"status"`
	Code   *string `json:"code"`
}
