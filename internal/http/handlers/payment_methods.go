package handlers

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/api"
	identity "github.com/open-rails/openrails/internal/billingidentity"
	"github.com/open-rails/openrails/internal/cardguard"
	"github.com/open-rails/openrails/internal/db/models"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/integrations/nmi"
	"github.com/open-rails/openrails/internal/intents"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/internal/modules/abuse"
	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/open-rails/openrails/internal/modules/paymentmethods"
	"github.com/open-rails/openrails/internal/modules/payments/rails"
	billingservice "github.com/open-rails/openrails/internal/service"
	"github.com/open-rails/openrails/internal/shared/uuidutil"
	log "github.com/sirupsen/logrus"
)

const (
	createPaymentMethodTimeout = 28 * time.Second
	// Replacement performs a provider read, one bounded mutation, and a
	// confirmation read. Leave enough room for all three NMI deadlines plus
	// the durable ledger transitions around them.
	updatePaymentMethodTimeout              = 50 * time.Second
	codePaymentMethodProviderOutcomeUnknown = "provider_outcome_unknown"
	codePaymentMethodDeleteFailed           = "payment_method_delete_failed"
	codePaymentMethodDeleteUnsupported      = "payment_method_delete_unsupported"
	codePaymentMethodUpdateFailed           = "payment_method_update_failed"
	codePaymentMethodUpdateRetryRequired    = "payment_method_update_retry_required"
)

// ListPaymentMethods (GET /me/payment-methods) is one page of the caller's
// saved cards, newest first.
func ListPaymentMethods(r *httprequest.Request) {
	customer, ok := selfAccountPayer(r)
	if !ok {
		return
	}
	listPaymentMethods(r, customer)
}

// ListCustomerPaymentMethods (GET /admin/customers/{customer_id}/payment-methods)
// is one page of a customer's saved cards, newest first.
func ListCustomerPaymentMethods(r *httprequest.Request) {
	customer, ok := commerceCustomer(r, customerIDParam(r.Param("customer_id")))
	if !ok {
		return
	}
	ids, ok := listIDs(r, billing.ParsePaymentMethodID)
	if !ok {
		return
	}
	if ids == nil {
		listPaymentMethods(r, customer)
		return
	}
	methods, err := r.State.PaymentMethodService.ListByIDs(r.Request.Context(), customer.UUID(), uuidutil.Of(ids))
	if err != nil {
		writeRefusal(r, err, "failed to list payment methods")
		return
	}
	out, err := paymentMethodsView(r, customer, methods)
	if err != nil {
		r.InternalError("failed to read payment methods", err)
		return
	}
	r.SuccessJSON(billing.ListPage[billing.PaymentMethod]{Items: out})
}

func listPaymentMethods(r *httprequest.Request, customer identity.CustomerID) {
	page, ok := r.Page()
	if !ok {
		return
	}
	methods, err := r.State.PaymentMethodService.ListPage(r.Request.Context(), customer.UUID(), page)
	if err != nil {
		writeRefusal(r, err, "failed to list payment methods")
		return
	}
	out, err := paymentMethodsView(r, customer, methods.Items)
	if err != nil {
		r.InternalError("failed to read payment methods", err)
		return
	}
	r.SuccessJSON(billing.ListPage[billing.PaymentMethod]{Items: out, Next: methods.Next})
}

// CreatePaymentMethod (POST /me/payment-methods) saves a card with a PSP.
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
	if req.PSPID.IsZero() {
		r.APIError(api.Coded(billing.CodeInvalidParam, "psp_id must name a PSP").WithParam("psp_id"))
		return
	}
	pspID := req.PSPID.UUID()
	if req.Card != nil {
		if !cardFieldAdmitted(r, strings.TrimSpace(req.PaymentToken) != "", billingDetailStrings(req.BillingDetails)...) {
			return
		}
	} else if strings.TrimSpace(req.PaymentToken) == "" {
		r.APIError(api.Coded(billing.CodeInvalidParam, "payment_token or card is required").WithParam("payment_token"))
		return
	}
	if refuseBlockedCardAttempt(r, user.ID) {
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
		PaymentToken: strings.TrimSpace(req.PaymentToken),
		Card:         req.Card,
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
	if e2eRunID := strings.TrimSpace(r.Header("X-E2E-Run-ID")); e2eRunID != "" {
		create.Metadata["e2e_run_id"] = e2eRunID
	}

	pm, err := r.State.RailPaymentMethodService.CreatePaymentMethod(ctx, user.ID, create)
	if err != nil {
		log.WithError(err).WithFields(log.Fields{"request_id": r.RequestID(), "user_id": user.ID}).Error("Failed to create payment method")
		switch {
		case errors.Is(err, paymentmethods.ErrPSPRequired), errors.Is(err, paymentmethods.ErrPSPUnavailable),
			errors.Is(err, paymentmethods.ErrPaymentMethodsUnsupportedOnRail):
			r.APIError(api.Coded(billing.CodeInvalidParam, err.Error()).WithParam("psp_id"))
			return
		case errors.Is(err, merchants.ErrSecretBackendUnavailable):
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

const codePaymentMethodUpdateUnsupported = "payment_method_update_unsupported"

// createPaymentMethodProviderError is the refusal of a card save the provider
// may or may not have made; nil for any other error. The provider's own text
// is never echoed.
func createPaymentMethodProviderError(err error) *api.APIError {
	if !nmi.IsTransportAmbiguous(err) {
		return nil
	}
	return api.Coded(codePaymentMethodProviderOutcomeUnknown, "The payment provider did not confirm whether the card was saved. Refresh your payment methods before trying again.")
}

// ReplacePaymentMethodCard (PUT /me/payment-methods/{id}) replaces a saved
// card in place; 202 while the provider converges.
func ReplacePaymentMethodCard(r *httprequest.Request) {
	user := r.GetUser()
	if user == nil {
		r.ErrorCode(billing.CodeAuthenticationRequired, "")
		return
	}
	var body billing.ReplacePaymentMethodCardParams
	if !r.BindJSON(&body) {
		return
	}
	defer body.Card.Zero()
	methodID, err := billing.ParsePaymentMethodID(r.Param("id"))
	if err != nil || methodID.IsZero() {
		r.APIError(api.Coded(billing.CodeInvalidParam, "invalid payment method id").WithParam("id"))
		return
	}

	token := strings.TrimSpace(body.PaymentToken)
	attemptKey := ""
	if body.Card != nil {
		if !cardFieldAdmitted(r, token != "", billingDetailStrings(body.BillingDetails)...) {
			return
		}
		// A retry of this replacement is recognised by its Idempotency-Key,
		// never by the card.
		if attemptKey = strings.TrimSpace(r.Header("Idempotency-Key")); attemptKey == "" {
			attemptKey = uuid.NewString()
		} else if len(attemptKey) > 255 || cardguard.ContainsPAN(attemptKey) {
			r.APIError(api.Coded(billing.CodeInvalidParam, "Idempotency-Key must be at most 255 bytes and carry no card data"))
			return
		}
	} else if token == "" {
		r.APIError(api.Coded(billing.CodeInvalidParam, "payment_token or card is required").WithParam("payment_token"))
		return
	}

	pm, ok := ownedPaymentMethod(r, methodID.UUID(), user.ID)
	if !ok {
		return
	}
	if !rails.SupportsPaymentMethodCRUD(pm.Rail) {
		r.APIError(api.Coded(codePaymentMethodUpdateUnsupported, paymentmethods.RailPaymentMethodsUnsupported(string(pm.Rail)).Error()))
		return
	}

	update := &paymentmethods.UpdatePaymentMethodRequest{PaymentToken: &token, Card: body.Card, AttemptKey: attemptKey}
	if body.BillingDetails != nil {
		details := billingDetailsInput(body.BillingDetails)
		update.NameOnCard, update.Address1, update.Address2 = &details.Name, &details.Line1, &details.Line2
		update.City, update.State, update.Zip, update.Country = &details.City, &details.State, &details.PostalCode, &details.Country
		update.Phone, update.Email = &details.Phone, &details.Email
	}

	ctx, cancel := r.Budget(updatePaymentMethodTimeout)
	defer cancel()

	updated, err := r.State.RailPaymentMethodService.UpdatePaymentMethod(ctx, pm, update)
	if err != nil {
		fields := log.Fields{"payment_method_id": pm.ID, "user_id": user.ID, "rail": pm.Rail}
		switch {
		case writeCardEntryError(r, err):
			return
		case errors.Is(err, paymentmethods.ErrPaymentMethodDeleteUnsafe):
			r.ErrorCode(billing.CodeResourceConflict, "Payment method changed before the update could be accepted")
			return
		case errors.Is(err, paymentmethods.ErrPaymentMethodDeleteProcessing):
			r.ErrorCode(billing.CodeResourceConflict, "Payment method deletion is already processing")
			return
		case errors.Is(err, paymentmethods.ErrPaymentMethodCustodianUnsupported), errors.Is(err, paymentmethods.ErrPaymentMethodsUnsupportedOnRail):
			r.ErrorCode(codePaymentMethodUpdateUnsupported, err.Error())
			return
		case errors.Is(err, merchants.ErrSecretBackendUnavailable), errors.Is(err, paymentmethods.ErrPaymentMethodProviderUnavailable):
			log.WithError(err).WithFields(fields).Warn("Payment method update unavailable because provider credentials could not be loaded")
			r.ErrorCode(billing.CodeServiceUnavailable, "Payment rail credentials are temporarily unavailable")
			return
		case errors.Is(err, paymentmethods.ErrPaymentMethodUpdateProcessing):
			log.WithError(err).WithFields(fields).Info("Payment method update is still converging")
			r.Status(http.StatusAccepted)
			return
		case errors.Is(err, paymentmethods.ErrPaymentMethodRetokenize):
			log.WithError(err).WithFields(fields).Info("Payment method update requires a fresh token")
			r.ErrorCode(codePaymentMethodUpdateRetryRequired, "The card was not updated. Enter the card again to create a fresh token.")
			return
		}
		var validation *paymentmethods.PaymentMethodUpdateValidationError
		if errors.As(err, &validation) {
			r.ErrorCode(billing.CodeInvalidParam, validation.Message)
			return
		}
		var refused *paymentmethods.PaymentMethodError
		if errors.As(err, &refused) {
			log.WithError(err).WithFields(fields).Info("Replacement card refused by the issuer; the previous card stays in use")
			writePaymentMethodError(r, refused)
			return
		}
		var terminal *paymentmethods.PaymentMethodUpdateFailedError
		if errors.As(err, &terminal) {
			log.WithError(err).WithFields(fields).Error("Payment method update failed permanently")
			r.ErrorCode(codePaymentMethodUpdateFailed, "Payment method could not be updated at the payment provider")
			return
		}
		r.InternalError("Failed to update payment method", err)
		return
	}
	writePaymentMethod(r, http.StatusOK, identity.CustomerIDFromString(user.ID), updated)
}

// DeletePaymentMethod (DELETE /me/payment-methods/{id}): 204, or 202 while
// the provider converges.
func DeletePaymentMethod(r *httprequest.Request) {
	user := r.GetUser()
	if user == nil {
		r.ErrorCode(billing.CodeAuthenticationRequired, "")
		return
	}
	deletePaymentMethodForCustomer(r, user.ID)
}

// DeleteCustomerPaymentMethod is DeletePaymentMethod for merchant staff, after
// permission and customer scope are checked.
func DeleteCustomerPaymentMethod(r *httprequest.Request) {
	customer, ok := commerceCustomer(r, customerIDParam(r.Param("customer_id")))
	if !ok {
		return
	}
	deletePaymentMethodForCustomer(r, customer.String())
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
	log.WithFields(log.Fields{"payment_method_id": pm.ID, "user_id": customerID, "rail": pm.Rail}).Info("Payment method successfully deleted")
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
	fields := log.Fields{"payment_method_id": pm.ID, "user_id": userID, "rail": pm.Rail}
	switch {
	case errors.Is(err, paymentmethods.ErrPaymentMethodDeleteProcessing):
		log.WithError(err).WithFields(fields).Info("Payment method deletion is still converging")
		r.Status(http.StatusAccepted)
	case errors.Is(err, paymentmethods.ErrPaymentMethodInUse):
		r.ErrorCode(billing.CodeResourceConflict, "Cannot delete payment method linked to an active, pending, or past-due subscription")
	case errors.Is(err, paymentmethods.ErrPaymentMethodDeleteUnsafe):
		r.ErrorCode(billing.CodeResourceConflict, "Payment method cannot be deleted safely; contact support")
	case errors.Is(err, paymentmethods.ErrPaymentMethodCustodianUnsupported):
		r.APIError(api.Coded(codePaymentMethodDeleteUnsupported, err.Error()).WithMetadata(map[string]any{"custodian": pm.Custodian}))
	case errors.Is(err, paymentmethods.ErrPaymentMethodsUnsupportedOnRail):
		r.APIError(api.Coded(codePaymentMethodDeleteUnsupported, err.Error()).WithMetadata(map[string]any{"rail": pm.Rail}))
	case errors.Is(err, intents.ErrRateCeilingTripped):
		// #732: the operator alert is raised by the destructive-operation
		// ceiling; this surface returns a stable refusal and never deletes locally.
		log.WithError(err).WithFields(fields).Warn("Payment method delete refused by destructive-operation rate ceiling")
		r.ErrorCode(billing.CodeRateLimitExceeded, "Destructive operation rate limit reached; try again later or contact support")
	case errors.Is(err, merchants.ErrSecretBackendUnavailable), errors.Is(err, paymentmethods.ErrPaymentMethodProviderUnavailable):
		log.WithError(err).WithFields(fields).Warn("Payment method delete unavailable because provider credentials could not be loaded")
		r.ErrorCode(billing.CodeServiceUnavailable, "Payment rail credentials are temporarily unavailable")
	default:
		var terminal *paymentmethods.PaymentMethodDeleteFailedError
		if errors.As(err, &terminal) {
			log.WithError(err).WithFields(fields).Error("Payment method deletion failed permanently at the provider")
			r.ErrorCode(codePaymentMethodDeleteFailed, "Payment method could not be deleted at the payment provider")
			return
		}
		r.InternalError("Failed to delete payment method", err)
	}
}

// SetCollectionPaymentMethod (PUT /me/collection-payment-method) makes one of
// the caller's cards collect its invoices in one currency.
func SetCollectionPaymentMethod(r *httprequest.Request) {
	payer, ok := selfAccountPayer(r)
	if !ok {
		return
	}
	var req billing.CollectionPaymentMethod
	if !r.BindJSON(&req) {
		return
	}
	currency, ok := serviceRequiredCurrency(r, req.Currency)
	if !ok {
		return
	}
	if err := money.RequireBillingCurrency(currency); err != nil {
		r.APIError(api.Coded(billing.CodeInvalidParam, err.Error()).WithParam("currency"))
		return
	}
	if req.PaymentMethodID.IsZero() {
		r.APIError(api.Coded(billing.CodeInvalidParam, "payment_method_id is required").WithParam("payment_method_id"))
		return
	}
	svc, err := billingservice.New(r.State)
	if err != nil {
		r.InternalError("billing service unavailable", err)
		return
	}
	if err := svc.SetInvoiceCollectionPaymentMethod(r.Request.Context(), payer, currency, req.PaymentMethodID.UUID()); err != nil {
		if errors.Is(err, money.ErrCollectionPaymentMethodInvalid) {
			r.ErrorCode(billing.CodeCollectionPaymentMethodInvalid, "")
			return
		}
		r.InternalError("failed to set collection payment method", err)
		return
	}
	r.SuccessJSON(billing.CollectionPaymentMethod{Currency: currency, PaymentMethodID: req.PaymentMethodID})
}

func writePaymentMethod(r *httprequest.Request, status int, customer identity.CustomerID, pm *models.PaymentMethod) {
	out, err := paymentMethodsView(r, customer, []*models.PaymentMethod{pm})
	if err != nil {
		r.InternalError("failed to read payment method", err)
		return
	}
	r.JSON(status, out[0])
}

// paymentMethodsView builds the wire methods of one customer: each card's
// derived health, the subscriptions it pays and the currencies it collects.
func paymentMethodsView(r *httprequest.Request, customer identity.CustomerID, methods []*models.PaymentMethod) ([]billing.PaymentMethod, error) {
	out := make([]billing.PaymentMethod, 0, len(methods))
	if len(methods) == 0 {
		return out, nil
	}
	ctx := r.Request.Context()
	charges, err := r.State.PaymentMethodService.LatestCharges(ctx, methods)
	if err != nil {
		return nil, err
	}
	collects, err := money.NewMoneyService(r.State.DB, r.Clock).CollectionPaymentMethodCurrencies(ctx, customer)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	for _, pm := range methods {
		var charge *models.PaymentMethodCharge
		if c, ok := charges[pm.ID]; ok {
			charge = &c
		}
		out = append(out, PaymentMethodToAPI(pm, charge, collects[pm.ID], now))
	}
	return out, nil
}

// PaymentMethodToAPI is a stored card on the wire.
func PaymentMethodToAPI(pm *models.PaymentMethod, charge *models.PaymentMethodCharge, collects []string, now time.Time) billing.PaymentMethod {
	subs := make([]billing.PaymentMethodSubscription, 0, len(pm.Subscriptions))
	for _, s := range pm.Subscriptions {
		item := billing.PaymentMethodSubscription{ID: billing.SubscriptionID(s.ID), CreatedAt: s.CreatedAt}
		if s.Product != nil {
			item.DisplayName = s.Product.DisplayName
		}
		subs = append(subs, item)
	}
	if collects == nil {
		collects = []string{}
	}
	out := billing.PaymentMethod{
		ID:                   billing.PaymentMethodID(pm.ID),
		CustomerID:           billing.CustomerID(pm.CustomerID),
		Rail:                 string(pm.Rail),
		Card:                 pm.Card.Details(),
		BillingDetails:       billingDetailsFromMetadata(pm.Metadata),
		Health:               paymentMethodHealth(pm.Card, charge, now),
		Subscriptions:        subs,
		CollectionCurrencies: collects,
		CreatedAt:            pm.CreatedAt,
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

func billingDetailStrings(d *billing.BillingDetails) []string {
	in := billingDetailsInput(d)
	return []string{in.Name, in.Email, in.Phone, in.Line1, in.Line2, in.City, in.State, in.PostalCode, in.Country}
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
