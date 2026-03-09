package handlers

import (
	"database/sql"
	"errors"
	"net/http"

	"github.com/open-rails/openrails/internal/db/models"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	riverjobs "github.com/open-rails/openrails/internal/river"
	"github.com/open-rails/openrails/internal/services"
	"github.com/open-rails/openrails/pkg/api"
	"github.com/open-rails/openrails/pkg/authprovider"
	"github.com/riverqueue/river"
)

type cancelSubscriptionRequest struct {
	Feedback string `json:"feedback" validate:"max=500"`
}

func CancelSubscription(r *httprequest.Request) {
	req := new(cancelSubscriptionRequest)
	if !r.BindJSON(req) {
		return
	}

	uc, ok := authprovider.UserContextFromGin(r.GinCtx)
	if !ok || uc.UserID == "" {
		r.ErrorJSON(http.StatusUnauthorized, "User authentication required")
		return
	}

	subscriptionIDStr := r.GinCtx.Param("id")
	if subscriptionIDStr == "" {
		r.ErrorJSON(http.StatusBadRequest, "subscription ID required")
		return
	}

	subscriptionID, err := api.ParseSubscriptionID(subscriptionIDStr)
	if err != nil {
		r.ErrorJSON(http.StatusBadRequest, "Invalid subscription ID format")
		return
	}

	sub, err := r.State.SubscriptionService.GetByID(r.Request.Context(), subscriptionID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			r.ErrorJSON(http.StatusNotFound, "subscription not found")
			return
		}
		r.ErrorJSON(http.StatusInternalServerError, "failed to retrieve subscription")
		return
	}

	if sub.UserID != uc.UserID {
		r.ErrorJSON(http.StatusNotFound, "subscription not found")
		return
	}

	if sub.Processor == services.ProcessorCCBill {
		r.GinCtx.JSON(http.StatusUnprocessableEntity, map[string]any{
			"error":       "CCBill subscriptions cannot be cancelled through our system. Please visit the CCBill consumer support portal to manage your subscription. You will need the email address you used when subscribing.",
			"support_url": "https://support.ccbill.com",
			"code":        "ccbill_cancel_required",
		})
		return
	}

	_, err = r.State.RiverProducer.Insert(r.Request.Context(), riverjobs.CancelSubscriptionArgs{
		UserID:         uc.UserID,
		SubscriptionID: subscriptionID,
		Feedback:       req.Feedback,
	}, &river.InsertOpts{Queue: riverjobs.QueueBilling})
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, "failed to enqueue cancellation")
		return
	}

	r.GinCtx.JSON(http.StatusAccepted, map[string]any{"status": "queued"})
}

func ResumeSubscription(r *httprequest.Request) {
	uc, ok := authprovider.UserContextFromGin(r.GinCtx)
	if !ok || uc.UserID == "" {
		r.ErrorJSON(http.StatusUnauthorized, "User authentication required")
		return
	}

	subscriptionIDStr := r.GinCtx.Param("id")
	if subscriptionIDStr == "" {
		r.ErrorJSON(http.StatusBadRequest, "subscription ID required")
		return
	}

	subscriptionID, err := api.ParseSubscriptionID(subscriptionIDStr)
	if err != nil {
		r.ErrorJSON(http.StatusBadRequest, "Invalid subscription ID format")
		return
	}

	sub, err := r.State.SubscriptionService.GetByID(r.Request.Context(), subscriptionID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			r.ErrorJSON(http.StatusNotFound, "subscription not found")
			return
		}
		r.ErrorJSON(http.StatusInternalServerError, "failed to load subscription")
		return
	}

	if sub.UserID != uc.UserID {
		r.ErrorJSON(http.StatusNotFound, "subscription not found")
		return
	}

	if sub.Processor != models.ProcessorStripe {
		r.ErrorJSON(http.StatusBadRequest, "resume unsupported for processor")
		return
	}

	if sub.Status != models.StatusCancelled {
		r.ErrorJSON(http.StatusBadRequest, "subscription is not cancelled")
		return
	}

	if _, err := r.State.RiverProducer.Insert(r.Request.Context(), riverjobs.ResumeSubscriptionArgs{
		UserID:         uc.UserID,
		SubscriptionID: subscriptionID,
	}, &river.InsertOpts{Queue: riverjobs.QueueBilling}); err != nil {
		r.ErrorJSON(http.StatusInternalServerError, "failed to enqueue resume")
		return
	}

	r.GinCtx.JSON(http.StatusAccepted, map[string]any{"status": "queued"})
}
