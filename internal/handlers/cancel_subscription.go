package handlers

import (
	"database/sql"
	"errors"
	"net/http"

	httprequest "github.com/open-rails/openrails/internal/http/request"
	riverjobs "github.com/open-rails/openrails/internal/river"
	"github.com/open-rails/openrails/internal/services"
	"github.com/open-rails/openrails/pkg/api"
	"github.com/open-rails/openrails/pkg/authprovider"
	"github.com/riverqueue/river"
)

// CancelSubscription cancels a user's subscription by ID
// POST /v1/me/subscriptions/:id/cancel
func CancelSubscription(r *httprequest.Request) {
	req := new(CancelSubscriptionRequest)
	if !r.BindJSON(req) {
		return
	}

	uc, ok := authprovider.UserContextFromGin(r.GinCtx)
	if !ok || uc.UserID == "" {
		r.ErrorJSON(http.StatusUnauthorized, "User authentication required")
		return
	}

	// Parse subscription ID from path
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

	if r.State.SubscriptionService == nil {
		r.ErrorJSON(http.StatusInternalServerError, "subscription service unavailable")
		return
	}

	// Get subscription by ID and verify ownership
	sub, err := r.State.SubscriptionService.GetByID(r.Request.Context(), subscriptionID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			r.ErrorJSON(http.StatusNotFound, "subscription not found")
			return
		}
		r.ErrorJSON(http.StatusInternalServerError, "failed to retrieve subscription")
		return
	}

	// Verify ownership
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

	if r.State.RiverProducer == nil {
		r.ErrorJSON(http.StatusInternalServerError, "job queue unavailable")
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
