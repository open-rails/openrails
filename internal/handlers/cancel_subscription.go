package handlers

import (
	"net/http"

	authgin "github.com/PaulFidika/authkit/adapters/gin"
	riverjobs "github.com/doujins-org/doujins-billing/internal/river"
	"github.com/doujins-org/doujins-billing/internal/services"
	"github.com/riverqueue/river"
)

// CancelSubscription cancels the current user's subscription
func CancelSubscription(r *Request) {
	req := new(CancelSubscriptionRequest)
	if !r.BindJSON(req) {
		return
	}

	cl, ok := authgin.ClaimsFromGin(r.GinCtx)
	if !ok || cl.UserID == "" {
		r.ErrorJSON(http.StatusUnauthorized, "User authentication required")
		return
	}

	if r.State.SubscriptionService == nil {
		r.ErrorJSON(http.StatusInternalServerError, "subscription service unavailable")
		return
	}
	sub, err := r.State.SubscriptionService.GetActiveSubscription(r.Request.Context(), cl.UserID)
	if err != nil {
		r.ErrorJSON(http.StatusNotFound, "active subscription not found")
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
		UserID:   cl.UserID,
		Feedback: req.Feedback,
	}, &river.InsertOpts{Queue: riverjobs.QueueBilling})
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, "failed to enqueue cancellation")
		return
	}

	r.GinCtx.JSON(http.StatusAccepted, map[string]any{"status": "queued"})
}
