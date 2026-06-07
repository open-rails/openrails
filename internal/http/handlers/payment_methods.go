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

	"github.com/open-rails/openrails/internal/db/models"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/modules/payments/processors"
	"github.com/open-rails/openrails/internal/modules/vault"
	sharedformat "github.com/open-rails/openrails/internal/shared/format"
	"github.com/open-rails/openrails/internal/tenancy"
	"github.com/open-rails/openrails/pkg/api"
	log "github.com/sirupsen/logrus"
)

type listPaymentMethodsQuery struct {
	Limit           int  `form:"limit"`
	Offset          int  `form:"offset"`
	IncludeInactive bool `form:"include_inactive"`
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
	Processor      string                       `json:"processor"`
	Customer       *string                      `json:"customer,omitempty"`
	BillingDetails *paymentMethodBillingDetails `json:"billing_details,omitempty"`
	Card           *paymentMethodCardDetails    `json:"card,omitempty"`
	Metadata       map[string]string            `json:"metadata,omitempty"`
	Livemode       bool                         `json:"livemode"`
	Created        int64                        `json:"created"`
	FailureReason  *string                      `json:"failure_reason,omitempty"`
	Subscriptions  []subscriptionSummary        `json:"subscriptions,omitempty"`
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

	ctx, cancel := context.WithTimeout(r.Request.Context(), 10*time.Second)
	defer cancel()

	email := strings.TrimSpace(req.Email)
	if email == "" && user.Email != nil {
		email = strings.TrimSpace(*user.Email)
	}

	createReq := createVaultRequestFromPaymentMethodRequest(req, email)
	if e2eRunID := strings.TrimSpace(r.Header("X-E2E-Run-ID")); e2eRunID != "" {
		createReq.Metadata["e2e_run_id"] = e2eRunID
	}

	pm, err := r.State.VaultService.CreateVault(ctx, user.ID, createReq)
	if err != nil {
		log.WithError(err).WithField("user_id", user.ID).Error("Failed to create payment method")
		if errors.Is(err, tenancy.ErrSecretBackendUnavailable) {
			r.ErrorJSON(http.StatusServiceUnavailable, "payment processor credentials are temporarily unavailable")
			return
		}
		var vaultErr *vault.VaultError
		if errors.As(err, &vaultErr) {
			code := api.CodePaymentFailed
			if strings.TrimSpace(vaultErr.LocalizationID) != "" {
				code = vaultErr.LocalizationID
			}
			r.APIError(api.NewAPIError(http.StatusBadRequest, api.ErrorTypeCard, code, vaultErr.Error()))
			return
		}
		r.ErrorJSON(http.StatusBadRequest, "failed to create payment method")
		return
	}

	r.SuccessJSON(paymentMethodToAPI(pm))
}

func createVaultRequestFromPaymentMethodRequest(req *createPaymentMethodRequest, email string) *vault.CreateVaultRequest {
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

	return &vault.CreateVaultRequest{
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
		case errors.Is(err, vault.ErrPaymentMethodNotFound):
			r.ErrorJSON(http.StatusNotFound, "Payment method not found")
			return
		case errors.Is(err, vault.ErrPaymentMethodAccessDenied):
			r.ErrorJSON(http.StatusForbidden, "Access denied - you don't own this payment method")
			return
		default:
			log.WithError(err).WithFields(log.Fields{"payment_method_id": methodID, "user_id": user.ID}).Error("Failed to validate payment method ownership")
			r.ErrorJSON(http.StatusInternalServerError, "Failed to validate payment method")
			return
		}
	}

	if !processors.IsNMIBackedProcessor(pm.Processor) {
		r.ErrorJSON(http.StatusBadRequest, "Only NMI-backed payment methods can be updated")
		return
	}

	updateReq := &vault.UpdateVaultRequest{
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

	updated, err := r.State.VaultService.UpdateVault(ctx, pm, updateReq)
	if err != nil {
		log.WithError(err).WithFields(log.Fields{"payment_method_id": methodID, "user_id": user.ID}).Error("Failed to update payment method")
		if errors.Is(err, tenancy.ErrSecretBackendUnavailable) {
			r.ErrorJSON(http.StatusServiceUnavailable, "payment processor credentials are temporarily unavailable")
			return
		}
		r.ErrorJSON(http.StatusBadRequest, "failed to update payment method")
		return
	}

	r.SuccessJSON(paymentMethodToAPI(updated))
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

	r.SuccessJSON(api.NewList(paymentMethodsToAPI(methods), totalItems, req.Limit, req.Offset))
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
		case errors.Is(err, vault.ErrPaymentMethodNotFound):
			log.WithFields(log.Fields{"payment_method_id": id, "user_id": user.ID}).Warn("Payment method not found for deletion")
			r.ErrorJSON(http.StatusNotFound, "Payment method not found")
			return
		case errors.Is(err, vault.ErrPaymentMethodAccessDenied):
			log.WithFields(log.Fields{"payment_method_id": id, "user_id": user.ID}).Warn("Unauthorized payment method deletion attempt")
			r.ErrorJSON(http.StatusForbidden, "Access denied - you don't own this payment method")
			return
		default:
			log.WithError(err).WithFields(log.Fields{"payment_method_id": id, "user_id": user.ID}).Error("Failed to validate payment method ownership")
			r.ErrorJSON(http.StatusInternalServerError, "Failed to validate payment method")
			return
		}
	}

	for _, s := range paymentMethod.Subscriptions {
		if s.Status == "active" || s.Status == "pending" || s.Status == "past_due" {
			log.WithFields(log.Fields{"payment_method_id": id, "user_id": user.ID, "subscription_id": s.ID, "subscription_status": s.Status}).Warn("Cannot delete payment method linked to active, past_due or pending subscription")
			r.ErrorJSON(http.StatusConflict, "Cannot delete payment method linked to active, past_due or pending subscription")
			return
		}
	}

	if err := r.State.PaymentMethodService.Delete(r.Request.Context(), id); err != nil {
		if errors.Is(err, vault.ErrPaymentMethodNotFound) {
			log.WithFields(log.Fields{"payment_method_id": id, "user_id": user.ID}).Warn("Payment method not found during deletion")
			r.ErrorJSON(http.StatusNotFound, "Payment method not found")
			return
		}
		log.WithError(err).WithFields(log.Fields{"payment_method_id": id, "user_id": user.ID}).Error("Failed to delete payment method")
		r.ErrorJSON(http.StatusInternalServerError, "Failed to delete payment method")
		return
	}

	log.WithFields(log.Fields{"payment_method_id": id, "user_id": user.ID, "processor": paymentMethod.Processor}).Info("Payment method successfully deleted")

	r.SuccessJSON(map[string]any{"success": true, "message": "Payment method deleted successfully"})
}

func paymentMethodToAPI(pm *models.PaymentMethod) paymentMethodResponse {
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
		Processor:      string(pm.Processor),
		BillingDetails: paymentMethodBillingDetailsFromMetadata(metadata),
		Card:           card,
		Created:        api.ToUnix(pm.CreatedAt),
		Metadata:       metadata,
		FailureReason:  pm.FailureReason,
		Subscriptions:  subs,
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

func paymentMethodsToAPI(methods []*models.PaymentMethod) []paymentMethodResponse {
	result := make([]paymentMethodResponse, len(methods))
	for i, pm := range methods {
		result[i] = paymentMethodToAPI(pm)
	}
	return result
}
