package handlers

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/doujins-org/ginapi/response"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/processors"
	"github.com/open-rails/openrails/internal/services"
	"github.com/open-rails/openrails/pkg/api"
	log "github.com/sirupsen/logrus"
)

// ListPaymentMethods returns the user's payment methods (optionally including inactive)
func CreatePaymentMethod(r *httprequest.Request) {
	if r.State == nil || r.State.VaultService == nil {
		r.ErrorJSON(http.StatusServiceUnavailable, "payment vault unavailable")
		return
	}

	user := r.GetUser()
	if user == nil {
		r.ErrorJSON(http.StatusUnauthorized, "Authentication required")
		return
	}

	req := new(CreatePaymentMethodRequest)
	if !r.BindJSON(req) {
		return
	}

	if strings.TrimSpace(req.PaymentToken) == "" {
		r.ErrorJSON(http.StatusBadRequest, "payment_token is required")
		return
	}

	ctx, cancel := context.WithTimeout(r.Request.Context(), 10*time.Second)
	defer cancel()

	// Ensure LastFour is only the last 4 digits
	lastFour := strings.TrimSpace(req.LastFour)
	if len(lastFour) > 4 {
		lastFour = lastFour[len(lastFour)-4:]
	}
	createReq := &services.CreateVaultRequest{
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

	pm, err := r.State.VaultService.CreateVault(ctx, user, createReq)
	if err != nil {
		log.WithError(err).WithField("user_id", user.ID).Error("Failed to create payment method")
		var vaultErr *services.VaultError
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

	r.SuccessJSON(PaymentMethodToAPI(pm))
}

// UpdatePaymentMethod replaces the stored payment method using a tokenized payload
func UpdatePaymentMethod(r *httprequest.Request) {
	if r.State == nil || r.State.VaultService == nil {
		r.ErrorJSON(http.StatusServiceUnavailable, "payment vault unavailable")
		return
	}

	req := new(UpdatePaymentMethodRequest)
	if !r.BindURI(req.Path()) {
		return
	}
	if !r.BindJSON(req.Body()) {
		return
	}

	user := r.GetUser()
	if user == nil {
		r.ErrorJSON(http.StatusUnauthorized, "Authentication required")
		return
	}

	methodID, err := api.ParsePaymentMethodID(req.ID)
	if err != nil {
		r.ErrorJSON(http.StatusBadRequest, "Invalid payment method ID format")
		return
	}

	trimmedToken := strings.TrimSpace(req.PaymentToken)
	if trimmedToken == "" {
		r.ErrorJSON(http.StatusBadRequest, "payment_token is required")
		return
	}

	pm, err := r.State.PaymentMethodService.ValidatePaymentMethodOperation(r.Request.Context(), methodID, user.ID)
	if err != nil {
		switch {
		case errors.Is(err, services.ErrPaymentMethodNotFound):
			r.ErrorJSON(http.StatusNotFound, "Payment method not found")
			return
		case errors.Is(err, services.ErrPaymentMethodAccessDenied):
			r.ErrorJSON(http.StatusForbidden, "Access denied - you don't own this payment method")
			return
		default:
			log.WithError(err).WithFields(log.Fields{
				"payment_method_id": methodID,
				"user_id":           user.ID,
			}).Error("Failed to validate payment method ownership")
			r.ErrorJSON(http.StatusInternalServerError, "Failed to validate payment method")
			return
		}
	}

	if !processors.IsNMIBackedProcessor(pm.Processor) {
		r.ErrorJSON(http.StatusBadRequest, "Only NMI-backed payment methods can be updated")
		return
	}

	updateReq := &services.UpdateVaultRequest{
		PaymentToken: &trimmedToken,
		Provider:     req.Provider,
		FirstName:    req.FirstName,
		LastName:     req.LastName,
		Address1:     req.Address1,
		City:         req.City,
		State:        req.State,
		Zip:          req.Zip,
		Country:      req.Country,
		Phone:        req.Phone,
		Email:        req.Email,
		Company:      req.Company,
		Address2:     req.Address2,
	}

	ctx, cancel := context.WithTimeout(r.Request.Context(), 10*time.Second)
	defer cancel()

	updated, err := r.State.VaultService.UpdateVault(ctx, pm, updateReq)
	if err != nil {
		log.WithError(err).WithFields(log.Fields{
			"payment_method_id": methodID,
			"user_id":           user.ID,
		}).Error("Failed to update payment method")
		r.ErrorJSON(http.StatusBadRequest, err.Error())
		return
	}

	r.SuccessJSON(PaymentMethodToAPI(updated))
}

func ListPaymentMethods(r *httprequest.Request) {
	req := new(ListPaymentMethodsRequest)
	req.SetDefaults()
	if !r.BindQuery(req.Query()) {
		return
	}

	// Fallback if Bind doesn't populate query embedded struct
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

	// Validate pagination parameters
	if req.Limit < 1 || req.Limit > 100 {
		r.ErrorJSON(http.StatusBadRequest, "Limit must be between 1 and 100")
		return
	}
	if req.Offset < 0 {
		r.ErrorJSON(http.StatusBadRequest, "Offset must be non-negative")
		return
	}

	methods, totalItems, err := r.State.PaymentMethodService.ListByUserID(
		r.Request.Context(),
		user.ID,
		req.Limit,
		req.Offset,
	)
	if err != nil {
		log.WithError(err).WithFields(log.Fields{
			"user_id": user.ID,
			"limit":   req.Limit,
			"offset":  req.Offset,
		}).Error("Failed to retrieve payment methods")
		r.ErrorJSON(http.StatusInternalServerError, "Failed to retrieve payment methods")
		return
	}

	r.SuccessJSON(response.NewList(PaymentMethodsToAPI(methods), totalItems, req.Limit, req.Offset))
}

// DeletePaymentMethod removes a payment method by ID
func DeletePaymentMethod(r *httprequest.Request) {
	req := new(DeletePaymentMethodRequest)
	if !r.BindURI(req.Path()) {
		return
	}

	// Validate UUID format
	id, err := api.ParsePaymentMethodID(req.ID)
	if err != nil {
		log.WithError(err).WithField("id", req.ID).Error("Invalid payment method ID format")
		r.ErrorJSON(http.StatusBadRequest, "Invalid payment method ID format")
		return
	}

	user := r.GetUser()
	if user == nil {
		log.Error("User not found in request context")
		r.ErrorJSON(http.StatusUnauthorized, "Authentication required")
		return
	}

	// Validate ownership and get payment method details
	paymentMethod, err := r.State.PaymentMethodService.ValidatePaymentMethodOperation(r.Request.Context(), id, user.ID)
	if err != nil {
		switch {
		case errors.Is(err, services.ErrPaymentMethodNotFound):
			log.WithFields(log.Fields{
				"payment_method_id": id,
				"user_id":           user.ID,
			}).Warn("Payment method not found for deletion")
			r.ErrorJSON(http.StatusNotFound, "Payment method not found")
			return
		case errors.Is(err, services.ErrPaymentMethodAccessDenied):
			log.WithFields(log.Fields{
				"payment_method_id": id,
				"user_id":           user.ID,
			}).Warn("Unauthorized payment method deletion attempt")
			r.ErrorJSON(http.StatusForbidden, "Access denied - you don't own this payment method")
			return
		default:
			log.WithError(err).WithFields(log.Fields{
				"payment_method_id": id,
				"user_id":           user.ID,
			}).Error("Failed to validate payment method ownership")
			r.ErrorJSON(http.StatusInternalServerError, "Failed to validate payment method")
			return
		}
	}

	for _, s := range paymentMethod.Subscriptions {

		if s.Status == "active" || s.Status == "pending" || s.Status == "past_due" {
			log.WithFields(log.Fields{
				"payment_method_id":   id,
				"user_id":             user.ID,
				"subscription_id":     s.ID,
				"subscription_status": s.Status,
			}).Warn("Cannot delete payment method linked to active, past_due or pending subscription")
			r.ErrorJSON(http.StatusConflict, "Cannot delete payment method linked to active, past_due or pending subscription")
			return
		}
	}

	// Perform the deletion
	if err := r.State.PaymentMethodService.Delete(r.Request.Context(), id); err != nil {
		if errors.Is(err, services.ErrPaymentMethodNotFound) {
			log.WithFields(log.Fields{
				"payment_method_id": id,
				"user_id":           user.ID,
			}).Warn("Payment method not found during deletion")
			r.ErrorJSON(http.StatusNotFound, "Payment method not found")
			return
		}
		log.WithError(err).WithFields(log.Fields{
			"payment_method_id": id,
			"user_id":           user.ID,
		}).Error("Failed to delete payment method")
		r.ErrorJSON(http.StatusInternalServerError, "Failed to delete payment method")
		return
	}

	// Log successful deletion for audit purposes
	log.WithFields(log.Fields{
		"payment_method_id": id,
		"user_id":           user.ID,
		"processor":         paymentMethod.Processor,
	}).Info("Payment method successfully deleted")

	r.SuccessJSON(map[string]any{
		"success": true,
		"message": "Payment method deleted successfully",
	})
}
