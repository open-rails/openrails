package handlers

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/doujins-org/ginapi/response"
	"github.com/open-rails/openrails/internal/db/models"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/modules/payments/processors"
	"github.com/open-rails/openrails/internal/modules/vault"
	sharedformat "github.com/open-rails/openrails/internal/shared/format"
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
	PaymentToken string `json:"payment_token" binding:"required"`
	FirstName    string `json:"first_name" binding:"required"`
	LastName     string `json:"last_name" binding:"required"`
	Address1     string `json:"address1" binding:"required"`
	City         string `json:"city" binding:"required"`
	State        string `json:"state" binding:"omitempty"`
	Zip          string `json:"zip" binding:"required"`
	Country      string `json:"country" binding:"required"`
	Phone        string `json:"phone" binding:"omitempty"`
	Email        string `json:"email" binding:"omitempty,email"`
	Company      string `json:"company" binding:"omitempty"`
	Address2     string `json:"address2" binding:"omitempty"`
	Provider     string `json:"provider" binding:"omitempty"`
	LastFour     string `json:"last_four" binding:"omitempty"`
	CardType     string `json:"card_type" binding:"omitempty"`
	ExpiryDate   string `json:"expiry_date" binding:"omitempty"`
}

type updatePaymentMethodRequest struct {
	PaymentToken string  `json:"payment_token" binding:"required"`
	FirstName    *string `json:"first_name"`
	LastName     *string `json:"last_name"`
	Address1     *string `json:"address1"`
	City         *string `json:"city"`
	State        *string `json:"state"`
	Zip          *string `json:"zip"`
	Country      *string `json:"country"`
	Phone        *string `json:"phone"`
	Email        *string `json:"email" binding:"omitempty,email"`
	Company      *string `json:"company"`
	Address2     *string `json:"address2"`
	Provider     *string `json:"provider"`
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

	if strings.TrimSpace(req.PaymentToken) == "" {
		r.ErrorJSON(http.StatusBadRequest, "payment_token is required")
		return
	}

	ctx, cancel := context.WithTimeout(r.Request.Context(), 10*time.Second)
	defer cancel()

	lastFour := strings.TrimSpace(req.LastFour)
	if len(lastFour) > 4 {
		lastFour = lastFour[len(lastFour)-4:]
	}
	createReq := &vault.CreateVaultRequest{
		PaymentToken: req.PaymentToken,
		FirstName:    req.FirstName,
		LastName:     req.LastName,
		Address1:     req.Address1,
		City:         req.City,
		State:        req.State,
		Zip:          req.Zip,
		Country:      req.Country,
		Phone:        req.Phone,
		Company:      req.Company,
		Address2:     req.Address2,
		Provider:     req.Provider,
		LastFour:     lastFour,
		CardType:     req.CardType,
		ExpiryDate:   req.ExpiryDate,
	}
	if e2eRunID := strings.TrimSpace(r.GinCtx.GetHeader("X-E2E-Run-ID")); e2eRunID != "" {
		createReq.Metadata = map[string]any{"e2e_run_id": e2eRunID}
	}

	if req.Email != "" {
		createReq.Email = req.Email
	} else if user.Email != nil {
		createReq.Email = strings.TrimSpace(*user.Email)
	}

	pm, err := r.State.VaultService.CreateVault(ctx, user.ID, createReq)
	if err != nil {
		log.WithError(err).WithField("user_id", user.ID).Error("Failed to create payment method")
		var vaultErr *vault.VaultError
		if errors.As(err, &vaultErr) {
			code := api.CodePaymentFailed
			if strings.TrimSpace(vaultErr.LocalizationID) != "" {
				code = vaultErr.LocalizationID
			}
			r.APIError(api.NewAPIError(http.StatusBadRequest, api.ErrorTypeCard, code, vaultErr.Error()))
			return
		}
		r.ErrorJSON(http.StatusBadRequest, err.Error())
		return
	}

	r.SuccessJSON(paymentMethodToAPI(pm))
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
		FirstName:    body.FirstName,
		LastName:     body.LastName,
		Address1:     body.Address1,
		City:         body.City,
		State:        body.State,
		Zip:          body.Zip,
		Country:      body.Country,
		Phone:        body.Phone,
		Email:        body.Email,
		Company:      body.Company,
		Address2:     body.Address2,
	}

	ctx, cancel := context.WithTimeout(r.Request.Context(), 10*time.Second)
	defer cancel()

	updated, err := r.State.VaultService.UpdateVault(ctx, pm, updateReq)
	if err != nil {
		log.WithError(err).WithFields(log.Fields{"payment_method_id": methodID, "user_id": user.ID}).Error("Failed to update payment method")
		r.ErrorJSON(http.StatusBadRequest, err.Error())
		return
	}

	r.SuccessJSON(paymentMethodToAPI(updated))
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

	r.SuccessJSON(response.NewList(paymentMethodsToAPI(methods), totalItems, req.Limit, req.Offset))
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

	return paymentMethodResponse{
		ID:            api.FormatPaymentMethodID(pm.ID),
		Object:        "payment_method",
		Type:          "card",
		Processor:     string(pm.Processor),
		Card:          card,
		Created:       api.ToUnix(pm.CreatedAt),
		Metadata:      map[string]string{},
		FailureReason: pm.FailureReason,
		Subscriptions: subs,
	}
}

func paymentMethodsToAPI(methods []*models.PaymentMethod) []paymentMethodResponse {
	result := make([]paymentMethodResponse, len(methods))
	for i, pm := range methods {
		result[i] = paymentMethodToAPI(pm)
	}
	return result
}
