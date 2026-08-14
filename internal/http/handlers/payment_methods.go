package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/db/models"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/integrations/nmi"
	"github.com/open-rails/openrails/internal/intents"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/internal/modules/paymentmethods"
	"github.com/open-rails/openrails/internal/modules/payments/rails"
	sharedformat "github.com/open-rails/openrails/internal/shared/format"
	"github.com/open-rails/openrails/pkg/api"
	log "github.com/sirupsen/logrus"
)

const (
	createPaymentMethodTimeout              = 28 * time.Second
	codePaymentMethodProviderOutcomeUnknown = "provider_outcome_unknown"
	codePaymentMethodDeleteFailed           = "payment_method_delete_failed"
	codePaymentMethodDeleteUnsupported      = "payment_method_delete_unsupported"
)

type listPaymentMethodsQuery struct {
	Limit  int `form:"limit"`
	Offset int `form:"offset"`
}

type paymentMethodURI struct {
	ID string `uri:"id" binding:"required"`
}

type createPaymentMethodRequest struct {
	PaymentToken   string `json:"payment_token" binding:"required"`
	NameOnCard     string `json:"name_on_card" binding:"omitempty"`
	FirstName      string `json:"first_name" binding:"omitempty"`
	LastName       string `json:"last_name" binding:"omitempty"`
	Address1       string `json:"address1" binding:"omitempty"`
	City           string `json:"city" binding:"omitempty"`
	State          string `json:"state" binding:"omitempty"`
	Zip            string `json:"zip" binding:"omitempty"`
	PostalCode     string `json:"postal_code" binding:"omitempty"`
	Country        string `json:"country" binding:"omitempty"`
	BillingCountry string `json:"billing_country" binding:"omitempty"`
	Phone          string `json:"phone" binding:"omitempty"`
	Email          string `json:"email" binding:"omitempty,email"`
	Company        string `json:"company" binding:"omitempty"`
	Address2       string `json:"address2" binding:"omitempty"`
	Provider       string `json:"provider" binding:"omitempty"`
	LastFour       string `json:"last_four" binding:"omitempty"`
	CardType       string `json:"card_type" binding:"omitempty"`
	ExpiryDate     string `json:"expiry_date" binding:"omitempty"`

	RawCardNumber        *json.RawMessage `json:"card_number,omitempty"`
	RawNumber            *json.RawMessage `json:"number,omitempty"`
	RawPAN               *json.RawMessage `json:"pan,omitempty"`
	RawPrimaryAccountNum *json.RawMessage `json:"primary_account_number,omitempty"`
	RawCVV               *json.RawMessage `json:"cvv,omitempty"`
	RawCVC               *json.RawMessage `json:"cvc,omitempty"`
	RawCVN               *json.RawMessage `json:"cvn,omitempty"`
	RawSecurityCode      *json.RawMessage `json:"security_code,omitempty"`
	RawVerificationValue *json.RawMessage `json:"verification_value,omitempty"`
}

type updatePaymentMethodRequest struct {
	PaymentToken   string  `json:"payment_token" binding:"required"`
	NameOnCard     *string `json:"name_on_card"`
	FirstName      *string `json:"first_name"`
	LastName       *string `json:"last_name"`
	Address1       *string `json:"address1"`
	City           *string `json:"city"`
	State          *string `json:"state"`
	Zip            *string `json:"zip"`
	PostalCode     *string `json:"postal_code"`
	Country        *string `json:"country"`
	BillingCountry *string `json:"billing_country"`
	Phone          *string `json:"phone"`
	Email          *string `json:"email" binding:"omitempty,email"`
	Company        *string `json:"company"`
	Address2       *string `json:"address2"`
	Provider       *string `json:"provider"`
	LastFour       *string `json:"last_four" binding:"omitempty"`
	CardType       *string `json:"card_type" binding:"omitempty"`
	ExpiryDate     *string `json:"expiry_date" binding:"omitempty"`

	RawCardNumber        *json.RawMessage `json:"card_number,omitempty"`
	RawNumber            *json.RawMessage `json:"number,omitempty"`
	RawPAN               *json.RawMessage `json:"pan,omitempty"`
	RawPrimaryAccountNum *json.RawMessage `json:"primary_account_number,omitempty"`
	RawCVV               *json.RawMessage `json:"cvv,omitempty"`
	RawCVC               *json.RawMessage `json:"cvc,omitempty"`
	RawCVN               *json.RawMessage `json:"cvn,omitempty"`
	RawSecurityCode      *json.RawMessage `json:"security_code,omitempty"`
	RawVerificationValue *json.RawMessage `json:"verification_value,omitempty"`
}

type rawCardField struct {
	name  string
	value *json.RawMessage
}

func (req *createPaymentMethodRequest) rejectRawCardFields() error {
	return rejectRawCardFieldValues(
		rawCardField{name: "card_number", value: req.RawCardNumber},
		rawCardField{name: "number", value: req.RawNumber},
		rawCardField{name: "pan", value: req.RawPAN},
		rawCardField{name: "primary_account_number", value: req.RawPrimaryAccountNum},
		rawCardField{name: "cvv", value: req.RawCVV},
		rawCardField{name: "cvc", value: req.RawCVC},
		rawCardField{name: "cvn", value: req.RawCVN},
		rawCardField{name: "security_code", value: req.RawSecurityCode},
		rawCardField{name: "verification_value", value: req.RawVerificationValue},
	)
}

func (req *updatePaymentMethodRequest) rejectRawCardFields() error {
	return rejectRawCardFieldValues(
		rawCardField{name: "card_number", value: req.RawCardNumber},
		rawCardField{name: "number", value: req.RawNumber},
		rawCardField{name: "pan", value: req.RawPAN},
		rawCardField{name: "primary_account_number", value: req.RawPrimaryAccountNum},
		rawCardField{name: "cvv", value: req.RawCVV},
		rawCardField{name: "cvc", value: req.RawCVC},
		rawCardField{name: "cvn", value: req.RawCVN},
		rawCardField{name: "security_code", value: req.RawSecurityCode},
		rawCardField{name: "verification_value", value: req.RawVerificationValue},
	)
}

func rejectRawCardFieldValues(fields ...rawCardField) error {
	for _, field := range fields {
		if field.value != nil {
			return fmt.Errorf("%s must be tokenized by the payment provider before calling OpenRails", field.name)
		}
	}
	return nil
}

type subscriptionSummary struct {
	ID          string    `json:"id"`
	DisplayName string    `json:"display_name"`
	Description string    `json:"description"`
	CreatedAt   time.Time `json:"created_at"`
}

type paymentMethodResponse struct {
	ID             string                       `json:"id"`
	Object         string                       `json:"object"`
	Type           string                       `json:"type"`
	Rail           string                       `json:"rail"`
	Customer       *string                      `json:"customer,omitempty"`
	BillingDetails *paymentMethodBillingDetails `json:"billing_details,omitempty"`
	Card           *paymentMethodCardDetails    `json:"card,omitempty"`
	Metadata       map[string]string            `json:"metadata,omitempty"`
	Created        int64                        `json:"created"`
	Health         *paymentMethodHealth         `json:"health,omitempty"`
	Subscriptions  []subscriptionSummary        `json:"subscriptions,omitempty"`
}

// paymentMethodHealth is the #589 DERIVED per-method health, computed at query
// time (never a stored column). last_charge_* come from openrails.payments via the
// subscription link; expiry_status from the card's expiry vs now.
type paymentMethodHealth struct {
	ExpiryStatus      string     `json:"expiry_status,omitempty"`       // card only: valid|expiring_soon|expired
	LastChargedAt     *time.Time `json:"last_charged_at,omitempty"`     // most recent charge time
	LastChargeOutcome string     `json:"last_charge_outcome,omitempty"` // success|failed|refunded|pending
	Active            bool       `json:"active"`                        // usable: not expired and last charge not failed
}

type paymentMethodBillingDetails struct {
	Name    *string               `json:"name,omitempty"`
	Email   *string               `json:"email,omitempty"`
	Phone   *string               `json:"phone,omitempty"`
	Address *paymentMethodAddress `json:"address,omitempty"`
}

type paymentMethodAddress struct {
	Line1      *string `json:"line1,omitempty"`
	Line2      *string `json:"line2,omitempty"`
	City       *string `json:"city,omitempty"`
	State      *string `json:"state,omitempty"`
	PostalCode *string `json:"postal_code,omitempty"`
	Country    *string `json:"country,omitempty"`
}

type paymentMethodCardDetails struct {
	Brand    *string `json:"brand,omitempty"`
	Last4    *string `json:"last4,omitempty"`
	ExpMonth *int    `json:"exp_month,omitempty"`
	ExpYear  *int    `json:"exp_year,omitempty"`
}

func CreatePaymentMethod(r *httprequest.Request) {
	user := r.GetUser()
	if user == nil {
		r.ErrorJSON(http.StatusUnauthorized, "Authentication required")
		return
	}

	req := new(createPaymentMethodRequest)
	if !r.BindJSON(req) {
		return
	}
	if err := req.rejectRawCardFields(); err != nil {
		r.ErrorJSON(http.StatusBadRequest, err.Error())
		return
	}

	if strings.TrimSpace(req.PaymentToken) == "" {
		r.ErrorJSON(http.StatusBadRequest, "payment_token is required")
		return
	}

	// The NMI transport has a 25-second ceiling. Leave enough time for the
	// local write while still completing before the browser's 30-second cap.
	ctx, cancel := context.WithTimeout(r.Request.Context(), createPaymentMethodTimeout)
	defer cancel()

	email := strings.TrimSpace(req.Email)
	if email == "" && user.Email != nil {
		email = strings.TrimSpace(*user.Email)
	}

	createReq := toCreatePaymentMethodRequest(req, email)
	if e2eRunID := strings.TrimSpace(r.Header("X-E2E-Run-ID")); e2eRunID != "" {
		createReq.Metadata["e2e_run_id"] = e2eRunID
	}

	pm, err := r.State.RailPaymentMethodService.CreatePaymentMethod(ctx, user.ID, createReq)
	if err != nil {
		log.WithError(err).WithFields(log.Fields{
			"request_id": r.RequestID(),
			"user_id":    user.ID,
		}).Error("Failed to create payment method")
		// or#896: an unsupported rail is not a misconfiguration — say so.
		if errors.Is(err, paymentmethods.ErrPaymentMethodsUnsupportedOnRail) {
			r.ErrorJSON(http.StatusBadRequest, err.Error())
			return
		}
		if errors.Is(err, merchants.ErrSecretBackendUnavailable) {
			r.ErrorJSON(http.StatusServiceUnavailable, "payment rail credentials are temporarily unavailable")
			return
		}
		if providerErr := createPaymentMethodProviderError(err); providerErr != nil {
			r.APIError(providerErr)
			return
		}
		var pmErr *paymentmethods.PaymentMethodError
		if errors.As(err, &pmErr) {
			writePaymentMethodError(r, pmErr)
			return
		}
		r.ErrorJSON(http.StatusBadRequest, "failed to create payment method")
		return
	}

	r.SuccessJSON(paymentMethodToAPI(pm, nil))
}

func createPaymentMethodProviderError(err error) *api.APIError {
	if !nmi.IsTransportAmbiguous(err) {
		return nil
	}
	return api.NewAPIError(
		http.StatusConflict,
		api.ErrorTypeAPI,
		codePaymentMethodProviderOutcomeUnknown,
		"The payment provider did not confirm whether the card was saved. Refresh your payment methods before trying again.",
	)
}

func toCreatePaymentMethodRequest(req *createPaymentMethodRequest, email string) *paymentmethods.CreatePaymentMethodRequest {
	lastFour := strings.TrimSpace(req.LastFour)
	if len(lastFour) > 4 {
		lastFour = lastFour[len(lastFour)-4:]
	}
	country := strings.TrimSpace(req.BillingCountry)
	if country == "" {
		country = strings.TrimSpace(req.Country)
	}
	postalCode := strings.TrimSpace(req.PostalCode)
	if postalCode == "" {
		postalCode = strings.TrimSpace(req.Zip)
	}

	metadata := map[string]any{}
	setMetadata := func(key, value string) {
		if value := strings.TrimSpace(value); value != "" {
			metadata[key] = value
		}
	}
	setMetadata("name_on_card", req.NameOnCard)
	setMetadata("billing_country", country)
	setMetadata("postal_code", postalCode)
	setMetadata("billing_email", email)
	setMetadata("billing_phone", req.Phone)
	setMetadata("billing_address1", req.Address1)
	setMetadata("billing_address2", req.Address2)
	setMetadata("billing_city", req.City)
	setMetadata("billing_state", req.State)
	setMetadata("billing_company", req.Company)

	return &paymentmethods.CreatePaymentMethodRequest{
		PaymentToken: req.PaymentToken,
		NameOnCard:   req.NameOnCard,
		FirstName:    req.FirstName,
		LastName:     req.LastName,
		Address1:     req.Address1,
		City:         req.City,
		State:        req.State,
		Zip:          postalCode,
		Country:      country,
		Phone:        req.Phone,
		Email:        email,
		Company:      req.Company,
		Address2:     req.Address2,
		Provider:     req.Provider,
		LastFour:     lastFour,
		CardType:     req.CardType,
		ExpiryDate:   req.ExpiryDate,
		Metadata:     metadata,
	}
}

func UpdatePaymentMethod(r *httprequest.Request) {
	path := new(paymentMethodURI)
	if !r.BindURI(path) {
		return
	}
	body := new(updatePaymentMethodRequest)
	if !r.BindJSON(body) {
		return
	}
	if err := body.rejectRawCardFields(); err != nil {
		r.ErrorJSON(http.StatusBadRequest, err.Error())
		return
	}

	user := r.GetUser()
	if user == nil {
		r.ErrorJSON(http.StatusUnauthorized, "Authentication required")
		return
	}

	methodID, err := api.ParsePaymentMethodID(path.ID)
	if err != nil {
		r.ErrorJSON(http.StatusBadRequest, "Invalid payment method ID format")
		return
	}

	trimmedToken := strings.TrimSpace(body.PaymentToken)
	if trimmedToken == "" {
		r.ErrorJSON(http.StatusBadRequest, "payment_token is required")
		return
	}

	pm, err := r.State.PaymentMethodService.ValidatePaymentMethodOperation(r.Request.Context(), methodID, user.ID)
	if err != nil {
		switch {
		case errors.Is(err, paymentmethods.ErrPaymentMethodNotFound):
			r.ErrorJSON(http.StatusNotFound, "Payment method not found")
			return
		case errors.Is(err, paymentmethods.ErrPaymentMethodAccessDenied):
			r.ErrorJSON(http.StatusForbidden, "Access denied - you don't own this payment method")
			return
		default:
			log.WithError(err).WithFields(log.Fields{"payment_method_id": methodID, "user_id": user.ID}).Error("Failed to validate payment method ownership")
			r.ErrorJSON(http.StatusInternalServerError, "Failed to validate payment method")
			return
		}
	}

	if !rails.SupportsPaymentMethodCRUD(pm.Rail) {
		// or#896: name the rail and where the instrument actually lives.
		r.ErrorJSON(http.StatusBadRequest, paymentmethods.RailPaymentMethodsUnsupported(string(pm.Rail)).Error())
		return
	}

	updateReq := &paymentmethods.UpdatePaymentMethodRequest{
		PaymentToken: &trimmedToken,
		Provider:     body.Provider,
		NameOnCard:   body.NameOnCard,
		FirstName:    body.FirstName,
		LastName:     body.LastName,
		Address1:     body.Address1,
		City:         body.City,
		State:        body.State,
		Zip:          firstNonNilString(body.PostalCode, body.Zip),
		Country:      firstNonNilString(body.BillingCountry, body.Country),
		Phone:        body.Phone,
		Email:        body.Email,
		Company:      body.Company,
		Address2:     body.Address2,
		LastFour:     body.LastFour,
		CardType:     body.CardType,
		ExpiryDate:   body.ExpiryDate,
	}

	ctx, cancel := context.WithTimeout(r.Request.Context(), 10*time.Second)
	defer cancel()

	updated, err := r.State.RailPaymentMethodService.UpdatePaymentMethod(ctx, pm, updateReq)
	if err != nil {
		log.WithError(err).WithFields(log.Fields{"payment_method_id": methodID, "user_id": user.ID}).Error("Failed to update payment method")
		if errors.Is(err, paymentmethods.ErrPaymentMethodsUnsupportedOnRail) {
			r.ErrorJSON(http.StatusBadRequest, err.Error())
			return
		}
		if errors.Is(err, merchants.ErrSecretBackendUnavailable) {
			r.ErrorJSON(http.StatusServiceUnavailable, "payment rail credentials are temporarily unavailable")
			return
		}
		r.ErrorJSON(http.StatusBadRequest, "failed to update payment method")
		return
	}

	r.SuccessJSON(paymentMethodToAPI(updated, nil))
}

func firstNonNilString(values ...*string) *string {
	for _, value := range values {
		if value != nil {
			return value
		}
	}
	return nil
}

func ListPaymentMethods(r *httprequest.Request) {
	req := &listPaymentMethodsQuery{Limit: 20, Offset: 0}
	if !r.BindQuery(req) {
		return
	}

	if l := r.Request.URL.Query().Get("limit"); l != "" {
		if v, err := strconv.Atoi(l); err == nil && v > 0 && v <= 100 {
			req.Limit = v
		} else if err != nil {
			log.WithError(err).WithField("limit", l).Error("Invalid limit parameter")
			r.ErrorJSON(http.StatusBadRequest, "Invalid limit parameter - must be a positive integer")
			return
		} else if v > 100 {
			log.WithField("limit", v).Error("Limit too large")
			r.ErrorJSON(http.StatusBadRequest, "Limit cannot exceed 100")
			return
		}
	}
	if o := r.Request.URL.Query().Get("offset"); o != "" {
		if v, err := strconv.Atoi(o); err == nil && v >= 0 {
			req.Offset = v
		} else if err != nil {
			log.WithError(err).WithField("offset", o).Error("Invalid offset parameter")
			r.ErrorJSON(http.StatusBadRequest, "Invalid offset parameter - must be a non-negative integer")
			return
		}
	}

	user := r.GetUser()
	if user == nil {
		log.Error("User not found in request context")
		r.ErrorJSON(http.StatusUnauthorized, "Authentication required")
		return
	}

	if req.Limit < 1 || req.Limit > 100 {
		r.ErrorJSON(http.StatusBadRequest, "Limit must be between 1 and 100")
		return
	}
	if req.Offset < 0 {
		r.ErrorJSON(http.StatusBadRequest, "Offset must be non-negative")
		return
	}

	methods, totalItems, err := r.State.PaymentMethodService.ListByUserID(r.Request.Context(), user.ID, req.Limit, req.Offset)
	if err != nil {
		log.WithError(err).WithFields(log.Fields{"user_id": user.ID, "limit": req.Limit, "offset": req.Offset}).Error("Failed to retrieve payment methods")
		r.ErrorJSON(http.StatusInternalServerError, "Failed to retrieve payment methods")
		return
	}

	charges := paymentMethodCharges(r, methods)
	r.SuccessJSON(api.NewList(paymentMethodsToAPI(methods, charges), totalItems, req.Limit, req.Offset))
}

// paymentMethodCharges loads the derived last-charge health for the listed
// methods. Enrichment is best-effort: a lookup failure degrades to no health
// rather than failing the listing.
func paymentMethodCharges(r *httprequest.Request, methods []*models.PaymentMethod) map[uuid.UUID]models.PaymentMethodCharge {
	charges, err := r.State.PaymentMethodService.LatestCharges(r.Request.Context(), methods)
	if err != nil {
		log.WithError(err).Warn("failed to derive payment-method charge health; returning methods without it")
		return nil
	}
	return charges
}

func DeletePaymentMethod(r *httprequest.Request) {
	path := new(paymentMethodURI)
	if !r.BindURI(path) {
		return
	}

	id, err := api.ParsePaymentMethodID(path.ID)
	if err != nil {
		log.WithError(err).WithField("id", path.ID).Error("Invalid payment method ID format")
		r.ErrorJSON(http.StatusBadRequest, "Invalid payment method ID format")
		return
	}

	user := r.GetUser()
	if user == nil {
		log.Error("User not found in request context")
		r.ErrorJSON(http.StatusUnauthorized, "Authentication required")
		return
	}

	paymentMethod, err := r.State.PaymentMethodService.ValidatePaymentMethodOperation(r.Request.Context(), id, user.ID)
	if err != nil {
		switch {
		case errors.Is(err, paymentmethods.ErrPaymentMethodNotFound):
			log.WithFields(log.Fields{"payment_method_id": id, "user_id": user.ID}).Warn("Payment method not found for deletion")
			r.ErrorJSON(http.StatusNotFound, "Payment method not found")
			return
		case errors.Is(err, paymentmethods.ErrPaymentMethodAccessDenied):
			log.WithFields(log.Fields{"payment_method_id": id, "user_id": user.ID}).Warn("Unauthorized payment method deletion attempt")
			r.ErrorJSON(http.StatusForbidden, "Access denied - you don't own this payment method")
			return
		default:
			log.WithError(err).WithFields(log.Fields{"payment_method_id": id, "user_id": user.ID}).Error("Failed to validate payment method ownership")
			r.ErrorJSON(http.StatusInternalServerError, "Failed to validate payment method")
			return
		}
	}

	err = r.State.RailPaymentMethodService.DeletePaymentMethod(r.Request.Context(), paymentMethod)
	if err != nil {
		respondPaymentMethodDeleteError(r, paymentMethod, user.ID, err)
		return
	}

	log.WithFields(log.Fields{"payment_method_id": id, "user_id": user.ID, "rail": paymentMethod.Rail}).Info("Payment method successfully deleted")
	r.Status(http.StatusNoContent)
}

func respondPaymentMethodDeleteError(r *httprequest.Request, pm *models.PaymentMethod, userID string, err error) {
	fields := log.Fields{"payment_method_id": pm.ID, "user_id": userID, "rail": pm.Rail}
	switch {
	case errors.Is(err, paymentmethods.ErrPaymentMethodDeleteProcessing):
		log.WithError(err).WithFields(fields).Info("Payment method deletion is still converging")
		r.Status(http.StatusAccepted)
	case errors.Is(err, paymentmethods.ErrPaymentMethodInUse):
		log.WithError(err).WithFields(fields).Warn("Payment method deletion blocked because the method is in use")
		r.APIError(api.NewAPIError(http.StatusConflict, api.ErrorTypeInvalidRequest, api.CodeResourceConflict,
			"Cannot delete payment method linked to an active, pending, or past-due subscription"))
	case errors.Is(err, paymentmethods.ErrPaymentMethodDeleteUnsafe):
		log.WithError(err).WithFields(fields).Warn("Payment method deletion refused because it cannot be scoped safely")
		r.APIError(api.NewAPIError(http.StatusConflict, api.ErrorTypeInvalidRequest, api.CodeResourceConflict,
			"Payment method cannot be deleted safely; contact support"))
	case errors.Is(err, paymentmethods.ErrPaymentMethodsUnsupportedOnRail):
		log.WithError(err).WithFields(fields).Info("Payment method deletion is managed by the payment rail")
		r.APIError(api.NewAPIError(http.StatusBadRequest, api.ErrorTypeInvalidRequest, codePaymentMethodDeleteUnsupported, err.Error()).
			WithMetadata(map[string]any{"rail": pm.Rail}))
	case errors.Is(err, intents.ErrRateCeilingTripped):
		// #732: the operator alert is raised by the destructive-operation
		// ceiling; this surface returns a stable refusal and never deletes locally.
		log.WithError(err).WithFields(fields).Warn("Payment method delete refused by destructive-operation rate ceiling")
		r.APIError(api.NewAPIError(http.StatusTooManyRequests, api.ErrorTypeRateLimit, api.CodeRateLimitExceeded,
			"Destructive operation rate limit reached; try again later or contact support"))
	case errors.Is(err, merchants.ErrSecretBackendUnavailable), errors.Is(err, paymentmethods.ErrPaymentMethodProviderUnavailable):
		log.WithError(err).WithFields(fields).Warn("Payment method delete unavailable because provider credentials could not be loaded")
		r.APIError(api.NewAPIError(http.StatusServiceUnavailable, api.ErrorTypeAPI, api.CodeServiceUnavailable,
			"Payment rail credentials are temporarily unavailable"))
	default:
		var terminal *paymentmethods.PaymentMethodDeleteFailedError
		if errors.As(err, &terminal) {
			log.WithError(err).WithFields(fields).Error("Payment method deletion failed permanently at the provider")
			r.APIError(api.NewAPIError(http.StatusBadGateway, api.ErrorTypeAPI, codePaymentMethodDeleteFailed,
				"Payment method could not be deleted at the payment provider"))
			return
		}
		r.InternalError("Failed to delete payment method", err)
	}
}

func paymentMethodToAPI(pm *models.PaymentMethod, charge *models.PaymentMethodCharge) paymentMethodResponse {
	card := &paymentMethodCardDetails{Brand: pm.CardType, Last4: pm.LastFour}
	if pm.ExpiryDate != nil {
		if month, year, ok := sharedformat.ParseExpiry(*pm.ExpiryDate); ok {
			card.ExpMonth = &month
			card.ExpYear = &year
		}
	}

	var subs []subscriptionSummary
	for _, s := range pm.Subscriptions {
		summary := subscriptionSummary{ID: s.ID.String(), CreatedAt: s.CreatedAt}
		if s.Product != nil {
			summary.DisplayName = s.Product.DisplayName
			summary.Description = s.Product.Description
		}
		subs = append(subs, summary)
	}

	metadata := paymentMethodMetadataToAPI(pm.Metadata)
	return paymentMethodResponse{
		ID:             api.FormatPaymentMethodID(pm.ID),
		Object:         "payment_method",
		Type:           "card",
		Rail:           string(pm.Rail),
		BillingDetails: paymentMethodBillingDetailsFromMetadata(metadata),
		Card:           card,
		Created:        api.ToUnix(pm.CreatedAt),
		Metadata:       metadata,
		Health:         paymentMethodHealthFrom(pm, charge),
		Subscriptions:  subs,
	}
}

// paymentMethodHealthFrom derives #589 health: expiry from the card, last-charge
// from the (optional) derived charge record. A method is "active" unless its card
// is expired or its most recent charge hard-failed.
func paymentMethodHealthFrom(pm *models.PaymentMethod, charge *models.PaymentMethodCharge) *paymentMethodHealth {
	h := &paymentMethodHealth{Active: true, ExpiryStatus: cardExpiryStatus(pm.ExpiryDate)}
	if charge != nil {
		t := charge.LastChargedAt
		h.LastChargedAt = &t
		h.LastChargeOutcome = chargeOutcome(charge.Status)
	}
	if h.ExpiryStatus == "expired" || h.LastChargeOutcome == "failed" {
		h.Active = false
	}
	return h
}

// cardExpiryStatus classifies a card's "MM/YY" expiry vs now; "" for non-cards or
// unparseable input. A card is valid through the END of its expiry month.
func cardExpiryStatus(expiry *string) string {
	if expiry == nil {
		return ""
	}
	month, year, ok := sharedformat.ParseExpiry(*expiry)
	if !ok {
		return ""
	}
	firstAfterExpiry := time.Date(year, time.Month(month), 1, 0, 0, 0, 0, time.UTC).AddDate(0, 1, 0)
	now := time.Now().UTC()
	switch {
	case !now.Before(firstAfterExpiry):
		return "expired"
	case now.AddDate(0, 0, 60).After(firstAfterExpiry):
		return "expiring_soon"
	default:
		return "valid"
	}
}

// chargeOutcome maps a raw payment_status to the listing's outcome vocabulary.
func chargeOutcome(status string) string {
	switch status {
	case "completed":
		return "success"
	case "failed":
		return "failed"
	case "refunded":
		return "refunded"
	default:
		return status
	}
}

func paymentMethodMetadataToAPI(metadata map[string]any) map[string]string {
	out := map[string]string{}
	for key, value := range metadata {
		switch v := value.(type) {
		case string:
			if v := strings.TrimSpace(v); v != "" {
				out[key] = v
			}
		}
	}
	return out
}

func paymentMethodBillingDetailsFromMetadata(metadata map[string]string) *paymentMethodBillingDetails {
	if len(metadata) == 0 {
		return nil
	}
	details := &paymentMethodBillingDetails{
		Name:  stringPtrFromMap(metadata, "name_on_card"),
		Email: stringPtrFromMap(metadata, "billing_email"),
		Phone: stringPtrFromMap(metadata, "billing_phone"),
		Address: &paymentMethodAddress{
			Line1:      stringPtrFromMap(metadata, "billing_address1"),
			Line2:      stringPtrFromMap(metadata, "billing_address2"),
			City:       stringPtrFromMap(metadata, "billing_city"),
			State:      stringPtrFromMap(metadata, "billing_state"),
			PostalCode: stringPtrFromMap(metadata, "postal_code"),
			Country:    stringPtrFromMap(metadata, "billing_country"),
		},
	}
	if details.Address.Line1 == nil &&
		details.Address.Line2 == nil &&
		details.Address.City == nil &&
		details.Address.State == nil &&
		details.Address.PostalCode == nil &&
		details.Address.Country == nil {
		details.Address = nil
	}
	if details.Name == nil && details.Email == nil && details.Phone == nil && details.Address == nil {
		return nil
	}
	return details
}

func stringPtrFromMap(metadata map[string]string, key string) *string {
	value := strings.TrimSpace(metadata[key])
	if value == "" {
		return nil
	}
	return &value
}

func paymentMethodsToAPI(methods []*models.PaymentMethod, charges map[uuid.UUID]models.PaymentMethodCharge) []paymentMethodResponse {
	result := make([]paymentMethodResponse, len(methods))
	for i, pm := range methods {
		var charge *models.PaymentMethodCharge
		if c, ok := charges[pm.ID]; ok {
			charge = &c
		}
		result[i] = paymentMethodToAPI(pm, charge)
	}
	return result
}
