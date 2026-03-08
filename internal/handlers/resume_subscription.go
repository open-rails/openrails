package handlers

import (
	"database/sql"
	"errors"
	"net/http"

	"github.com/open-rails/openrails/internal/db/models"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	riverjobs "github.com/open-rails/openrails/internal/river"
	"github.com/open-rails/openrails/pkg/api"
	"github.com/open-rails/openrails/pkg/authprovider"
	"github.com/riverqueue/river"
)

// ResumeSubscription resumes a cancelled Stripe subscription (cancel_at_period_end=false).
// POST /v1/me/subscriptions/:id/resume
func ResumeSubscription(r *httprequest.Request) {
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

	if r.State == nil || r.State.DB == nil {
		r.ErrorJSON(http.StatusInternalServerError, "database unavailable")
		return
	}
	if r.State.RiverProducer == nil {
		r.ErrorJSON(http.StatusInternalServerError, "job queue unavailable")
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
		r.ErrorJSON(http.StatusInternalServerError, "failed to load subscription")
		return
	}

	// Verify ownership
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
