package handlers

import (
	"database/sql"
	"net/http"
	"time"

	authgin "github.com/PaulFidika/authkit/adapters/gin"
	"github.com/doujins-org/doujins-billing/internal/db/models"
	riverjobs "github.com/doujins-org/doujins-billing/internal/river"
	"github.com/riverqueue/river"
)

// ResumeSubscription resumes a cancelled Stripe subscription (cancel_at_period_end=false).
func ResumeSubscription(r *Request) {
	cl, ok := authgin.ClaimsFromGin(r.GinCtx)
	if !ok || cl.UserID == "" {
		r.ErrorJSON(http.StatusUnauthorized, "User authentication required")
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

	now := time.Now().UTC()
	sub := new(models.Subscription)
	err := r.State.DB.GetDB().NewSelect().
		Model(sub).
		Where("sub.user_id = ?", cl.UserID).
		Where("sub.status = ?", models.StatusCancelled).
		Where("(sub.current_period_ends_at IS NULL OR sub.current_period_ends_at > ?)", now).
		OrderExpr("sub.created_at DESC").
		Limit(1).
		Scan(r.Request.Context())
	if err != nil {
		if err == sql.ErrNoRows {
			r.ErrorJSON(http.StatusNotFound, "no resumable subscription found")
			return
		}
		r.ErrorJSON(http.StatusInternalServerError, "failed to load subscription")
		return
	}
	if sub.Processor != models.ProcessorStripe {
		r.ErrorJSON(http.StatusBadRequest, "resume unsupported for processor")
		return
	}

	if _, err := r.State.RiverProducer.Insert(r.Request.Context(), riverjobs.ResumeSubscriptionArgs{
		UserID: cl.UserID,
	}, &river.InsertOpts{Queue: riverjobs.QueueBilling}); err != nil {
		r.ErrorJSON(http.StatusInternalServerError, "failed to enqueue resume")
		return
	}

	r.GinCtx.JSON(http.StatusAccepted, map[string]any{"status": "queued"})
}
