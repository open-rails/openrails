package handlers

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/open-rails/openrails/catalog"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/api"
	identity "github.com/open-rails/openrails/internal/billingidentity"
	"github.com/open-rails/openrails/internal/cardguard"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/integrations/nmi"
	"github.com/open-rails/openrails/internal/intents"
	"github.com/open-rails/openrails/internal/logx"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/internal/modules/abuse"
	"github.com/open-rails/openrails/internal/modules/idempotency"
	"github.com/open-rails/openrails/internal/modules/mandates"
	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/open-rails/openrails/internal/modules/payment"
	"github.com/open-rails/openrails/internal/modules/paymentmethods"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/internal/paymentpage"
	"github.com/open-rails/openrails/internal/shared/normalize"
	"github.com/open-rails/openrails/internal/shared/uuidutil"
)

const createPaymentMethodTimeout = 28 * time.Second

// ListPaymentMethods (GET /me/payment-methods) is one page of the caller's
// saved cards, newest first.
func ListPaymentMethods(r *httprequest.Request) {
	customer, ok := selfAccountPayer(r)
	if !ok {
		return
	}
	listPaymentMethods(r, customer, false)
}

// ListCustomerPaymentMethods (GET /admin/customers/{customer_id}/payment-methods)
// is one page of a customer's saved cards, newest first.
func ListCustomerPaymentMethods(r *httprequest.Request) {
	customer, ok := commerceCustomer(r, customerIDParam(r.Param("customer_id")))
	if !ok {
		return
	}
	var query billing.PaymentMethodListParams
	if !r.BindQuery(&query) {
		return
	}
	ids, ok := listIDs(r, query.IDs)
	if !ok {
		return
	}
	if ids == nil {
		listPaymentMethods(r, customer, true)
		return
	}
	methods, err := r.State.PaymentMethodService.ListByIDs(r.Request.Context(), customer.UUID(), uuidutil.Of(ids))
	if err != nil {
		writeRefusal(r, err, "failed to list payment methods")
		return
	}
	out, err := paymentMethodsView(r, customer, methods, true)
	if err != nil {
		r.InternalError("failed to read payment methods", err)
		return
	}
	r.SuccessJSON(billing.ListPage[billing.PaymentMethod]{Items: out})
}

// listPaymentMethods is one page of the customer's cards; ended includes
// the agreements that can no longer authorize a charge.
func listPaymentMethods(r *httprequest.Request, customer identity.CustomerID, ended bool) {
	page, ok := r.Page()
	if !ok {
		return
	}
	methods, err := r.State.PaymentMethodService.ListPage(r.Request.Context(), customer.UUID(), page)
	if err != nil {
		writeRefusal(r, err, "failed to list payment methods")
		return
	}
	out, err := paymentMethodsView(r, customer, methods.Items, ended)
	if err != nil {
		r.InternalError("failed to read payment methods", err)
		return
	}
	if _, ok := paymentpage.FromContext(r.Request.Context()); ok {
		out = paymentpage.SavedCards(out)
	}
	r.SuccessJSON(billing.ListPage[billing.PaymentMethod]{Items: out, Next: methods.Next})
}

// CreatePaymentMethod (POST /me/payment-methods) saves a card with a PSP in
// one call. A Stripe card the bank wants authenticated answers requires_action
// with its next_action. With an Idempotency-Key, a retry answers the first
// saved card on every rail.
func CreatePaymentMethod(r *httprequest.Request) {
	user := r.GetUser()
	if user == nil {
		r.ErrorCode(billing.CodeAuthenticationRequired, "")
		return
	}
	var req billing.CreatePaymentMethodParams
	if !r.BindJSON(&req) {
		return
	}
	defer req.Card.Zero()
	if refuseCardNumber(r, req) || refuseBlockedCardAttempt(r, user.ID) {
		return
	}
	key := strings.TrimSpace(r.Header("Idempotency-Key"))
	if key == "" {
		savePaymentMethod(r, user, req)
		return
	}
	if len(key) > 255 || cardguard.ContainsPAN(key) {
		r.APIError(api.Coded(billing.CodeInvalidParam, "Idempotency-Key is 1 to 255 bytes, without card data").WithParam("Idempotency-Key"))
		return
	}
	if r.State.Idempotency == nil {
		r.ErrorCode(billing.CodeServiceUnavailable, "card save unavailable")
		return
	}
	savedOnce(r, user, req, key)
}

// createPaymentMethodOperation names card saves in the idempotency store.
const createPaymentMethodOperation = "payment_methods.create"

// savedCardReplay is what a card save's key answers again: the first saved
// card, as it was answered. The digest leaves card data out.
type savedCardReplay struct {
	Digest string          `json:"digest"`
	Status int             `json:"status"`
	Body   json.RawMessage `json:"body"`
}

// savedOnce saves the card under its customer's key: a saved card is
// answered again; a refusal, or a card still awaiting the bank, runs again.
func savedOnce(r *httprequest.Request, user *payment.UserIdentity, req billing.CreatePaymentMethodParams, key string) {
	terms, err := json.Marshal(struct {
		PSPID          billing.PSPID           `json:"psp_id"`
		Token          string                  `json:"token"`
		Card           bool                    `json:"card"`
		BillingDetails *billing.BillingDetails `json:"billing_details"`
	}{req.PSPID, strings.TrimSpace(req.Token), req.Card != nil, req.BillingDetails})
	if err != nil {
		r.InternalError("encode card save", err)
		return
	}
	sum := sha256.Sum256(terms)
	digest := hex.EncodeToString(sum[:])
	ctx := r.Request.Context()
	claim, rec, err := r.State.Idempotency.Begin(ctx, createPaymentMethodOperation, user.ID+":"+key)
	if err != nil {
		r.InternalError("claim idempotency key", err)
		return
	}
	if claim == nil {
		var first savedCardReplay
		switch {
		case rec == nil || rec.Status != idempotency.StatusSucceeded:
			r.ErrorCode(billing.CodeIdempotencyKeyInUse, "")
		case json.Unmarshal(rec.Result, &first) != nil:
			r.InternalError("read idempotency key", errors.New("unreadable card save replay"))
		case first.Digest != digest:
			r.ErrorCode(billing.CodeIdempotencyKeyReused, "the Idempotency-Key was first sent with another card save")
		default:
			r.SetHeader("Cache-Control", "no-store")
			r.JSON(first.Status, first.Body)
		}
		return
	}
	work, stop := claim.Hold(ctx)
	r.Request = r.Request.WithContext(work)
	recorded := r.Record()
	savePaymentMethod(r, user, req)
	stop()
	r.Request = r.Request.WithContext(ctx)
	status, body := recorded()
	var answered struct {
		Status billing.PaymentMethodStatus `json:"status"`
	}
	if status != http.StatusCreated || json.Unmarshal(body, &answered) != nil || answered.Status == billing.PaymentMethodRequiresAction {
		_ = claim.Fail(ctx, fmt.Errorf("card save answered %d", status))
		return
	}
	stored, err := json.Marshal(savedCardReplay{Digest: digest, Status: status, Body: body})
	if err == nil {
		err = claim.Complete(ctx, stored)
	}
	if err != nil && !errors.Is(err, idempotency.ErrClaimLost) {
		r.Log.ErrorContext(ctx, "card save: idempotency key not settled", logx.Err(err))
	}
}

func savePaymentMethod(r *httprequest.Request, user *payment.UserIdentity, req billing.CreatePaymentMethodParams) {
	if req.PSPID.IsZero() {
		r.APIError(api.Coded(billing.CodeInvalidParam, "psp_id must name a PSP").WithParam("psp_id"))
		return
	}
	pspID := req.PSPID.UUID()
	if mid, err := merchant.Require(r.Request.Context()); err == nil && merchants.Of(r.State.DB) != nil {
		registry := merchants.Of(r.State.DB)
		psp, found, readErr := registry.PSPScopeByID(r.Request.Context(), mid, pspID)
		if readErr != nil {
			r.ErrorCode(billing.CodeServiceUnavailable, "card custody unavailable")
			return
		}
		platform, platformErr := registry.PlatformCardCustodian(r.Request.Context(), mid)
		if platformErr != nil {
			r.ErrorCode(billing.CodeServiceUnavailable, "card custody unavailable")
			return
		}
		// The same verified token-intent flow supports a merchant-owned BT
		// tenant. Preserve its actual custody kind; it is not a platform vault.
		if platform == nil && found && psp.CustodianID != nil {
			custodian, ok, err := registry.CustodianScopeByID(r.Request.Context(), mid, *psp.CustodianID)
			if err != nil {
				r.ErrorCode(billing.CodeServiceUnavailable, "card custody unavailable")
				return
			}
			if ok && !custodian.Archived && custodian.Kind == models.CustodianBasisTheory {
				platform = &custodian
			}
		}
		if platform != nil {
			if !found || psp.Archived || psp.Rail != string(models.RailNMI) {
				r.APIError(api.Coded(billing.CodeInvalidParam, "platform card setup requires an available NMI PSP").WithParam("psp_id"))
				return
			}
			if r.State.PurchaseService == nil {
				r.ErrorCode(billing.CodeServiceUnavailable, "card capture unavailable")
				return
			}
			ctx, cancel := r.Budget(createPaymentMethodTimeout)
			defer cancel()
			saved, err := r.State.PurchaseService.SaveBasisTheoryCard(db.WithCustodianID(ctx, platform.ID), req, user)
			writeSavedCard(r, user, http.StatusCreated, saved, err)
			return
		}
		custodianKind := ""
		if found && psp.CustodianID != nil {
			custodian, found, readErr := registry.CustodianScopeByID(r.Request.Context(), mid, *psp.CustodianID)
			if readErr != nil || !found {
				r.ErrorCode(billing.CodeServiceUnavailable, "card custody unavailable")
				return
			}
			custodianKind = custodian.Kind
		}
		if custodianKind == models.CustodianHyperSwitch {
			if r.State.OrderAttemptService == nil {
				r.ErrorCode(billing.CodeServiceUnavailable, "card capture unavailable")
				return
			}
			ctx, cancel := r.Budget(createPaymentMethodTimeout)
			defer cancel()
			saved, err := r.State.OrderAttemptService.SaveCustodianCard(ctx, req, r.Header("Idempotency-Key"), user, billingDetailsInput(req.BillingDetails).metadata())
			writeSavedCard(r, user, http.StatusCreated, saved, err)
			return
		}
	}
	if req.Card != nil {
		if !cardFieldAdmitted(r, strings.TrimSpace(req.Token) != "") {
			return
		}
	} else if strings.TrimSpace(req.Token) == "" {
		r.APIError(api.Coded(billing.CodeInvalidParam, "token or card is required").WithParam("token"))
		return
	}
	if mid, err := merchant.Require(r.Request.Context()); err == nil && pspRail(r, mid.UUID(), pspID) == string(models.RailStripe) {
		if req.Card != nil {
			r.APIError(api.Coded(billing.CodeInvalidParam, "a Stripe card is entered in Stripe's fields").WithParam("card"))
			return
		}
		saveStripeCard(r, user, pspID, req.Token)
		return
	}

	// The NMI transport has a 25-second ceiling. Leave enough time for the
	// local write while still completing before the browser's 30-second cap.
	ctx, cancel := r.Budget(createPaymentMethodTimeout)
	defer cancel()

	details := billingDetailsInput(req.BillingDetails)
	if details.Email == "" && user.Email != nil {
		details.Email = strings.TrimSpace(*user.Email)
	}
	create := &paymentmethods.CreatePaymentMethodRequest{
		PaymentToken: strings.TrimSpace(req.Token),
		Card:         (*cardguard.Card)(req.Card),
		PSPID:        pspID,
		NameOnCard:   details.Name,
		Address1:     details.Line1,
		Address2:     details.Line2,
		City:         details.City,
		State:        details.State,
		Zip:          details.PostalCode,
		Country:      details.Country,
		Phone:        details.Phone,
		Email:        details.Email,
		Metadata:     details.metadata(),
	}

	pm, err := r.State.RailPaymentMethodService.CreatePaymentMethod(ctx, user.ID, create)
	if err != nil {
		r.Log.ErrorContext(ctx, "Failed to create payment method", logx.Err(err), "request_id", r.RequestID(), "user_id", user.ID)
		switch {
		case errors.Is(err, paymentmethods.ErrPSPRequired), errors.Is(err, paymentmethods.ErrPSPUnavailable),
			errors.Is(err, paymentmethods.ErrPaymentMethodsUnsupportedOnRail):
			r.APIError(api.Coded(billing.CodeInvalidParam, err.Error()).WithParam("psp_id"))
			return
		case errors.Is(err, merchants.ErrConfigUnavailable):
			r.ErrorCode(billing.CodeServiceUnavailable, "payment rail credentials are temporarily unavailable")
			return
		case errors.Is(err, paymentmethods.ErrPaymentDuplicateRefused):
			r.ErrorCode(billing.CodePaymentDuplicateRefused, err.Error())
			return
		case writeCardEntryError(r, err):
			return
		}
		if ambiguous := createPaymentMethodProviderError(err); ambiguous != nil {
			r.APIError(ambiguous)
			return
		}
		// Only a card the provider refused counts toward card-testing blocks.
		if paymentmethods.CardRefused(err) {
			recordCardFailure(r, abuse.CustomerSubject(user.ID), abuse.AddressSubject(r.ClientIP()), abuse.MerchantSubject)
		}
		var pmErr *paymentmethods.PaymentMethodError
		if errors.As(err, &pmErr) {
			writePaymentMethodError(r, pmErr)
			return
		}
		r.ErrorCode(billing.CodePaymentProviderRejected, "failed to create payment method")
		return
	}
	writePaymentMethod(r, http.StatusCreated, identity.CustomerIDFromString(user.ID), pm)
}

// pspRail is the rail of one of the merchant's PSPs, "" when unknown.
func pspRail(r *httprequest.Request, merchantID, pspID uuid.UUID) string {
	psp, err := r.State.DB.Gen(r.Request.Context()).GetPSP(r.Request.Context(), gen.GetPSPParams{MerchantID: merchantID, ID: pspID})
	if err != nil {
		return ""
	}
	return psp.Rail
}

// saveStripeCard saves a card the page's Stripe fields tokenized.
func saveStripeCard(r *httprequest.Request, user *payment.UserIdentity, psp uuid.UUID, token string) {
	resolver := r.State.StripeEngines()
	if resolver == nil || r.State.PurchaseService == nil {
		r.ErrorCode(billing.CodeServiceUnavailable, "Stripe card save unavailable")
		return
	}
	ctx, cancel := r.Budget(createPaymentMethodTimeout)
	defer cancel()
	saved, err := r.State.PurchaseService.SaveStripeCard(ctx, psp, token, user, resolver)
	writeSavedCard(r, user, http.StatusCreated, saved, err)
}

// ConfirmPaymentMethod (POST /me/payment-methods/{id}/confirm) finishes a
// card save the bank asked the customer to authenticate.
func ConfirmPaymentMethod(r *httprequest.Request) {
	user := r.GetUser()
	if user == nil {
		r.ErrorCode(billing.CodeAuthenticationRequired, "")
		return
	}
	id, err := billing.ParsePaymentMethodID(r.Param("id"))
	if err != nil || id.IsZero() {
		r.ErrorCode(billing.CodeResourceNotFound, "Payment method not found")
		return
	}
	method, err := paymentmethods.NewPaymentMethodRepo(r.State.DB).GetByID(r.Request.Context(), id.UUID())
	if err != nil || method.CustomerID.String() != user.ID {
		r.ErrorCode(billing.CodeResourceNotFound, "Payment method not found")
		return
	}
	var req billing.ConfirmPaymentMethodParams
	if r.Request.ContentLength != 0 && !r.BindJSON(&req) || refuseCardNumber(r, req) {
		return
	}
	if method.Custodian == models.CustodianHyperSwitch {
		if r.State.OrderAttemptService == nil {
			r.ErrorCode(billing.CodeServiceUnavailable, "card capture unavailable")
			return
		}
		ctx, cancel := r.Budget(createPaymentMethodTimeout)
		defer cancel()
		saved, err := r.State.OrderAttemptService.ConfirmCustodianCard(ctx, id.UUID(), user, req.Capture)
		writeSavedCard(r, user, http.StatusOK, saved, err)
		return
	}
	if req.Capture != nil {
		r.ErrorCode(billing.CodeInvalidParam, "capture belongs to a custodian payment method")
		return
	}
	resolver := r.State.StripeEngines()
	if resolver == nil || r.State.PurchaseService == nil {
		r.ErrorCode(billing.CodeServiceUnavailable, "Stripe card save unavailable")
		return
	}
	saved, err := r.State.PurchaseService.ConfirmStripeCard(r.Request.Context(), id.UUID(), user, resolver)
	writeSavedCard(r, user, http.StatusOK, saved, err)
}

func writeSavedCard(r *httprequest.Request, user *payment.UserIdentity, status int, saved payment.SavedCard, err error) {
	var refused *paymentmethods.PaymentMethodError
	switch {
	case errors.As(err, &refused):
		recordCardFailure(r, abuse.CustomerSubject(user.ID), abuse.AddressSubject(r.ClientIP()), abuse.MerchantSubject)
		writePaymentMethodError(r, refused)
		return
	case errors.Is(err, paymentmethods.ErrPaymentMethodNotFound):
		r.ErrorCode(billing.CodeResourceNotFound, "Payment method not found")
		return
	case errors.Is(err, payment.ErrPaymentMethodStale):
		writePaymentMethodStale(r)
		return
	case errors.Is(err, payment.ErrOrderAttemptValidation):
		r.APIError(api.Coded(billing.CodeInvalidParam, "invalid card save request").WithParam("psp_id"))
		return
	case errors.Is(err, payment.ErrCardCaptureUnavailable):
		r.ErrorCode(billing.CodeServiceUnavailable, "card capture unavailable")
		return
	case errors.Is(err, payment.ErrOrderAttemptConflict):
		r.ErrorCode(billing.CodeResourceConflict, "card save conflicts with its accepted request")
		return
	case errors.Is(err, payment.ErrOrderAttemptExpired):
		r.ErrorCode(billing.CodePaymentMethodStale, "card save expired")
		return
	case err != nil:
		writeRefusal(r, err, "failed to save the card")
		return
	}
	out, err := paymentMethodsView(r, identity.CustomerIDFromString(user.ID), []*models.PaymentMethod{saved.Method}, false)
	if err != nil {
		r.InternalError("failed to read payment method", err)
		return
	}
	out[0].NextAction = saved.NextAction
	r.SetHeader("Cache-Control", "no-store")
	r.JSON(status, out[0])
}

// createPaymentMethodProviderError is the refusal of a card save the provider
// may or may not have made; nil for any other error. The provider's own text
// is never echoed.
func createPaymentMethodProviderError(err error) *api.APIError {
	if !nmi.IsTransportAmbiguous(err) {
		return nil
	}
	return api.Coded(billing.CodeProviderOutcomeUnknown, "The payment provider did not confirm whether the card was saved. Refresh your payment methods before trying again.")
}

// DeletePaymentMethod returns 204 only after synchronous provider confirmation.
func DeletePaymentMethod(r *httprequest.Request) {
	user := r.GetUser()
	if user == nil {
		r.ErrorCode(billing.CodeAuthenticationRequired, "")
		return
	}
	deletePaymentMethodForCustomer(r, user.ID)
}

// DeleteCustomerPaymentMethod retains the merchant actor and the same provider,
// in-use and destructive-admission guards as an authenticated customer request.
func DeleteCustomerPaymentMethod(r *httprequest.Request) {
	customer, ok := commerceCustomer(r, customerIDParam(r.Param("customer_id")))
	if !ok {
		return
	}
	deletePaymentMethodForCustomer(r, customer.UUID().String())
}

func deletePaymentMethodForCustomer(r *httprequest.Request, customerID string) {
	methodID, err := billing.ParsePaymentMethodID(r.Param("id"))
	if err != nil || methodID.IsZero() {
		r.APIError(api.Coded(billing.CodeInvalidParam, "invalid payment method id").WithParam("id"))
		return
	}
	pm, ok := ownedPaymentMethod(r, methodID.UUID(), customerID)
	if !ok {
		return
	}
	if err := r.State.RailPaymentMethodService.DeletePaymentMethod(r.Request.Context(), pm); err != nil {
		respondPaymentMethodDeleteError(r, pm, customerID, err)
		return
	}
	r.Log.InfoContext(r.Request.Context(), "Payment method successfully deleted", "payment_method_id", pm.ID, "user_id", customerID, "rail", pm.Rail)
	r.Status(http.StatusNoContent)
}

// ownedPaymentMethod loads a method the customer owns; another customer's is
// indistinguishable from a missing one.
func ownedPaymentMethod(r *httprequest.Request, id uuid.UUID, customerID string) (*models.PaymentMethod, bool) {
	pm, err := r.State.PaymentMethodService.ValidatePaymentMethodOperation(r.Request.Context(), id, customerID)
	switch {
	case errors.Is(err, paymentmethods.ErrPaymentMethodNotFound), errors.Is(err, paymentmethods.ErrPaymentMethodAccessDenied):
		r.ErrorCode(billing.CodeResourceNotFound, "Payment method not found")
		return nil, false
	case err != nil:
		r.InternalError("Failed to validate payment method", err)
		return nil, false
	}
	return pm, true
}

func respondPaymentMethodDeleteError(r *httprequest.Request, pm *models.PaymentMethod, userID string, err error) {
	fields := []any{"payment_method_id", pm.ID, "user_id", userID, "rail", pm.Rail}
	switch {
	case errors.Is(err, paymentmethods.ErrPaymentMethodDeleteProcessing):
		r.Log.With(fields...).InfoContext(r.Request.Context(), "Payment method deletion was not confirmed; explicit retry required", logx.Err(err))
		r.ErrorCode(billing.CodeServiceUnavailable, "Card removal could not be confirmed. Please try again.")
	case errors.Is(err, paymentmethods.ErrPaymentMethodInUse):
		r.ErrorCode(billing.CodeResourceConflict, "A subscription still uses this payment method: add a new card first")
	case errors.Is(err, paymentmethods.ErrPaymentMethodDeleteUnsafe):
		r.ErrorCode(billing.CodeResourceConflict, "Payment method cannot be deleted safely; contact support")
	case errors.Is(err, paymentmethods.ErrPaymentMethodCustodianUnsupported):
		r.APIError(api.Coded(billing.CodePaymentMethodDeleteUnsupported, err.Error()).WithMetadata(map[string]any{"custodian": pm.Custodian}))
	case errors.Is(err, paymentmethods.ErrPaymentMethodsUnsupportedOnRail):
		r.APIError(api.Coded(billing.CodePaymentMethodDeleteUnsupported, err.Error()).WithMetadata(map[string]any{"rail": pm.Rail}))
	case errors.Is(err, intents.ErrRateCeilingTripped):
		// #732: the operator alert is raised by the destructive-operation
		// ceiling; this surface returns a stable refusal and never deletes locally.
		r.Log.With(fields...).WarnContext(r.Request.Context(), "Payment method delete refused by destructive-operation rate ceiling", logx.Err(err))
		r.ErrorCode(billing.CodeRateLimitExceeded, "Destructive operation rate limit reached; try again later or contact support")
	case errors.Is(err, merchants.ErrConfigUnavailable), errors.Is(err, paymentmethods.ErrPaymentMethodProviderUnavailable):
		r.Log.With(fields...).WarnContext(r.Request.Context(), "Payment method delete unavailable because provider credentials could not be loaded", logx.Err(err))
		r.ErrorCode(billing.CodeServiceUnavailable, "Payment rail credentials are temporarily unavailable")
	default:
		var terminal *paymentmethods.PaymentMethodDeleteFailedError
		if errors.As(err, &terminal) {
			r.Log.With(fields...).ErrorContext(r.Request.Context(), "Payment method deletion failed permanently at the provider", logx.Err(err))
			r.ErrorCode(billing.CodePaymentMethodDeleteFailed, "Payment method could not be deleted at the payment provider")
			return
		}
		writeRefusal(r, err, "Failed to delete payment method")
	}
}

// SetDefaultPaymentMethod (PUT /me/default-payment-methods/{currency}) makes
// one of the caller's cards their default for a currency: it collects their
// invoices there and pays every card subscription in it without a card of its
// own, which move onto it.
func SetDefaultPaymentMethod(r *httprequest.Request) {
	payer, ok := selfAccountPayer(r)
	if !ok {
		return
	}
	currency, ok := serviceRequiredCurrency(r, r.Param("currency"))
	if !ok {
		return
	}
	if err := money.RequireBillingCurrency(currency); err != nil {
		r.APIError(api.Coded(billing.CodeInvalidParam, err.Error()).WithParam("currency"))
		return
	}
	var req billing.SetDefaultPaymentMethodParams
	if !r.BindJSON(&req) {
		return
	}
	if req.PaymentMethodID.IsZero() {
		r.APIError(api.Coded(billing.CodeInvalidParam, "payment_method_id is required").WithParam("payment_method_id"))
		return
	}
	ctx, cancel := r.Budget(30 * time.Second)
	defer cancel()
	mid, err := merchant.Require(ctx)
	if err != nil {
		r.InternalError("merchant scope", err)
		return
	}
	change := subscriptions.DefaultPaymentMethodChange{
		Customer: payer.UUID(), Currency: currency, PaymentMethodID: req.PaymentMethodID.UUID(),
		Verify: r.State.RecurringVerifier(mid.UUID(), intents.OriginUser), Swaps: r.State.PaymentSourceUpdateIntents,
	}
	if err := r.State.SubscriptionLifecycleService.SetDefaultPaymentMethod(ctx, change); err != nil {
		writeMoveRefusal(r, err, "failed to set the default payment method")
		return
	}
	r.SuccessJSON(billing.DefaultPaymentMethod{Currency: catalog.Currency(currency), PaymentMethodID: req.PaymentMethodID})
}

func writePaymentMethod(r *httprequest.Request, status int, customer identity.CustomerID, pm *models.PaymentMethod) {
	out, err := paymentMethodsView(r, customer, []*models.PaymentMethod{pm}, false)
	if err != nil {
		r.InternalError("failed to read payment method", err)
		return
	}
	r.JSON(status, out[0])
}

// paymentMethodsView builds the wire methods of one customer: each card's
// derived health, the subscriptions it pays, the currencies it is the
// default for and its agreements (ended ones too when ended).
func paymentMethodsView(r *httprequest.Request, customer identity.CustomerID, methods []*models.PaymentMethod, ended bool) ([]billing.PaymentMethod, error) {
	out := make([]billing.PaymentMethod, 0, len(methods))
	if len(methods) == 0 {
		return out, nil
	}
	ctx := r.Request.Context()
	charges, err := r.State.PaymentMethodService.LatestCharges(ctx, methods)
	if err != nil {
		return nil, err
	}
	defaults, err := money.NewMoneyService(r.State.DB, r.Clock).DefaultPaymentMethodCurrencies(ctx, customer)
	if err != nil {
		return nil, err
	}
	mid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	ids := make([]uuid.UUID, len(methods))
	for i, pm := range methods {
		ids[i] = pm.ID
	}
	agreements, err := mandates.ForPaymentMethods(ctx, r.State.DB.Gen(ctx), mid.UUID(), ids, ended)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	for _, pm := range methods {
		var charge *models.PaymentMethodCharge
		if c, ok := charges[pm.ID]; ok {
			charge = &c
		}
		view := PaymentMethodToAPI(pm, charge, defaults[pm.ID], now)
		view.Mandates = []billing.Mandate{}
		for _, m := range agreements[pm.ID] {
			view.Mandates = append(view.Mandates, m)
			view.Reusable = view.Reusable || (m.Kind == billing.MandateCardOnFile && m.Status == billing.MandateActive)
		}
		out = append(out, view)
	}
	return out, nil
}

// PaymentMethodToAPI is a stored card on the wire.
func PaymentMethodToAPI(pm *models.PaymentMethod, charge *models.PaymentMethodCharge, defaults []string, now time.Time) billing.PaymentMethod {
	subs := make([]billing.PaymentMethodSubscription, 0, len(pm.Subscriptions))
	for _, s := range pm.Subscriptions {
		if s == nil || (s.Status != models.StatusActive && s.Status != models.StatusPending && s.Status != models.StatusPastDue) {
			continue
		}
		item := billing.PaymentMethodSubscription{ID: billing.SubscriptionID(s.ID), CreatedAt: s.CreatedAt}
		if s.Product != nil {
			item.DisplayName = s.Product.DisplayName
		}
		subs = append(subs, item)
	}
	if defaults == nil {
		defaults = []string{}
	}
	out := billing.PaymentMethod{
		ID:                  billing.PaymentMethodID(pm.ID),
		CustomerID:          billing.CustomerID(pm.CustomerID),
		Rail:                billing.Rail(pm.Rail),
		Status:              billing.PaymentMethodStatus(pm.Status),
		Card:                pm.Card.Details(),
		BillingDetails:      billingDetailsFromMetadata(pm.Metadata),
		Health:              paymentMethodHealth(pm.Card, charge, now),
		ContactCardholderAt: pm.ContactCardholderAt,
		Mandates:            []billing.Mandate{},
		Subscriptions:       subs,
		DefaultCurrencies:   normalize.Strings[catalog.Currency](defaults),
		CreatedAt:           pm.CreatedAt,
	}
	if !pm.Chargeable() {
		out.Health.Active = false
	}
	if pm.ReplacedByID != nil {
		replacement := billing.PaymentMethodID(*pm.ReplacedByID)
		out.ReplacedBy = &replacement
	}
	if pm.PspID != nil {
		psp := billing.PSPID(*pm.PspID)
		out.PSPID = &psp
	}
	return out
}

// paymentMethodHealth derives the card's standing: active unless it expired
// or its most recent charge failed.
func paymentMethodHealth(card models.Card, charge *models.PaymentMethodCharge, now time.Time) billing.PaymentMethodHealth {
	h := billing.PaymentMethodHealth{Active: true}
	if expires := card.ExpiresAt(); !expires.IsZero() {
		status := billing.CardExpiryValid
		switch {
		case !now.Before(expires):
			status = billing.CardExpiryExpired
			h.Active = false
		case now.AddDate(0, 0, 60).After(expires):
			status = billing.CardExpiryExpiringSoon
		}
		h.ExpiryStatus = &status
	}
	if charge != nil {
		at, outcome := charge.LastChargedAt, billing.ChargeSucceeded
		if charge.Status != "succeeded" {
			outcome = billing.ChargeFailed
			h.Active = false
		}
		h.LastChargedAt, h.LastChargeOutcome = &at, &outcome
	}
	return h
}

// billingInput is billing details as the provider takes them.
type billingInput struct {
	Name, Email, Phone, Line1, Line2, City, State, PostalCode, Country string
}

func billingDetailsInput(d *billing.BillingDetails) billingInput {
	var in billingInput
	if d == nil {
		return in
	}
	in.Name, in.Email, in.Phone = trimmed(d.Name), trimmed(d.Email), trimmed(d.Phone)
	if a := d.Address; a != nil {
		in.Line1, in.Line2, in.City, in.State = trimmed(a.Line1), trimmed(a.Line2), trimmed(a.City), trimmed(a.State)
		in.PostalCode, in.Country = trimmed(a.PostalCode), trimmed(a.Country)
	}
	return in
}

// metadata is how a card's billing details are stored.
func (in billingInput) metadata() map[string]any {
	out := map[string]any{}
	for key, value := range map[string]string{
		"name_on_card": in.Name, "billing_email": in.Email, "billing_phone": in.Phone,
		"billing_address1": in.Line1, "billing_address2": in.Line2, "billing_city": in.City,
		"billing_state": in.State, "postal_code": in.PostalCode, "billing_country": in.Country,
	} {
		if value != "" {
			out[key] = value
		}
	}
	return out
}

func billingDetailsFromMetadata(metadata map[string]any) *billing.BillingDetails {
	get := func(key string) *string {
		if v, ok := metadata[key].(string); ok && strings.TrimSpace(v) != "" {
			v = strings.TrimSpace(v)
			return &v
		}
		return nil
	}
	details := &billing.BillingDetails{Name: get("name_on_card"), Email: get("billing_email"), Phone: get("billing_phone")}
	address := &billing.BillingAddress{
		Line1: get("billing_address1"), Line2: get("billing_address2"), City: get("billing_city"),
		State: get("billing_state"), PostalCode: get("postal_code"), Country: get("billing_country"),
	}
	if *address != (billing.BillingAddress{}) {
		details.Address = address
	}
	if *details == (billing.BillingDetails{}) {
		return nil
	}
	return details
}

func trimmed(s *string) string {
	if s == nil {
		return ""
	}
	return strings.TrimSpace(*s)
}
