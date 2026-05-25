package handlers

import (
	"database/sql"
	"errors"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/open-rails/openrails/internal/db/models"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	riverjobs "github.com/open-rails/openrails/internal/river"
	"github.com/open-rails/openrails/pkg/api"
	"github.com/open-rails/openrails/pkg/authprovider"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"
)

// User-initiated cancellations must include a typed explanation. The bound
// gin validator only honors `binding` tags, so the length rule is enforced
// explicitly below (after trimming) rather than via struct tags.
const (
	minCancelFeedbackChars = 4
	maxCancelFeedbackChars = 500
)

type cancelSubscriptionRequest struct {
	Feedback string `json:"feedback"`
}

func subscriptionLifecycleUniqueOpts() river.UniqueOpts {
	return river.UniqueOpts{
		ByArgs:  true,
		ByQueue: true,
		ByState: []rivertype.JobState{
			rivertype.JobStateAvailable,
			rivertype.JobStatePending,
			rivertype.JobStateRunning,
			rivertype.JobStateRetryable,
			rivertype.JobStateScheduled,
		},
	}
}

func CancelSubscription(r *httprequest.Request) {
	req := new(cancelSubscriptionRequest)
	if !r.BindJSON(req) {
		return
	}

	feedback := strings.TrimSpace(req.Feedback)
	switch n := utf8.RuneCountInString(feedback); {
	case n < minCancelFeedbackChars:
		r.ErrorJSON(http.StatusBadRequest, "Please tell us why you're cancelling (at least 4 characters).")
		return
	case n > maxCancelFeedbackChars:
		r.ErrorJSON(http.StatusBadRequest, "Cancellation feedback must be 500 characters or fewer.")
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

	if sub.Processor == subscriptions.ProcessorCCBill {
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
		Feedback:       feedback,
	}, &river.InsertOpts{
		Queue:      riverjobs.QueueBilling,
		UniqueOpts: subscriptionLifecycleUniqueOpts(),
	})
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
	}, &river.InsertOpts{
		Queue:      riverjobs.QueueBilling,
		UniqueOpts: subscriptionLifecycleUniqueOpts(),
	}); err != nil {
		r.ErrorJSON(http.StatusInternalServerError, "failed to enqueue resume")
		return
	}

	r.GinCtx.JSON(http.StatusAccepted, map[string]any{"status": "queued"})
}
