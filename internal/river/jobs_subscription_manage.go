package riverjobs

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/modules/entitlements"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/internal/services"
	"github.com/riverqueue/river"
	log "github.com/sirupsen/logrus"
)

const (
	KindSubscriptionCancel = "billing.subscription_cancel"
	KindSubscriptionResume = "billing.subscription_resume"
)

type CancelSubscriptionArgs struct {
	UserID         string    `json:"user_id"`
	SubscriptionID uuid.UUID `json:"subscription_id,omitempty"`
	Feedback       string    `json:"feedback,omitempty"`
}

func (CancelSubscriptionArgs) Kind() string { return KindSubscriptionCancel }

type ResumeSubscriptionArgs struct {
	UserID         string    `json:"user_id"`
	SubscriptionID uuid.UUID `json:"subscription_id,omitempty"`
}

func (ResumeSubscriptionArgs) Kind() string { return KindSubscriptionResume }

type CancelSubscriptionWorker struct {
	river.WorkerDefaults[CancelSubscriptionArgs]
	DB                           *db.DB
	Config                       *config.Config
	UserSubscriptionService      *subscriptions.UserSubscriptionService
	SubscriptionService          *subscriptions.SubscriptionService
	SubscriptionLifecycleService *services.SubscriptionLifecycleService
}

func (CancelSubscriptionWorker) Kind() string { return KindSubscriptionCancel }

func (w CancelSubscriptionWorker) Work(ctx context.Context, job *river.Job[CancelSubscriptionArgs]) error {
	if w.SubscriptionService == nil {
		return fmt.Errorf("subscription service unavailable")
	}
	userID := job.Args.UserID
	if userID == "" {
		return fmt.Errorf("user_id required")
	}

	var sub *models.Subscription
	var err error

	// If subscription ID is provided, use it directly
	if job.Args.SubscriptionID != uuid.Nil {
		sub, err = w.SubscriptionService.GetByID(ctx, job.Args.SubscriptionID)
		if err != nil {
			log.WithContext(ctx).WithFields(log.Fields{
				"user_id":         userID,
				"subscription_id": job.Args.SubscriptionID,
			}).Info("subscription not found")
			return nil
		}
		// Verify ownership
		if sub.UserID != userID {
			log.WithContext(ctx).WithFields(log.Fields{
				"user_id":         userID,
				"subscription_id": job.Args.SubscriptionID,
			}).Warn("subscription ownership mismatch")
			return nil
		}
	} else {
		// Fallback to active subscription lookup
		sub, err = w.SubscriptionService.GetActiveSubscription(ctx, userID)
		if err != nil {
			log.WithContext(ctx).WithField("user_id", userID).Info("no active subscription to cancel")
			return nil
		}
	}
	log.WithContext(ctx).WithFields(log.Fields{
		"user_id":         userID,
		"subscription_id": sub.ID,
		"processor":       sub.Processor,
	}).Info("processing subscription cancellation")

	switch sub.Processor {
	case models.ProcessorStripe:
		if w.SubscriptionLifecycleService == nil {
			return fmt.Errorf("subscription lifecycle service unavailable")
		}
		stripeSvc := &subscriptions.StripeService{Config: w.Config}
		if err := stripeSvc.CancelSubscription(ctx, sub.ProcessorSubscriptionID); err != nil {
			return err
		}
		var feedback *string
		if job.Args.Feedback != "" {
			feedback = &job.Args.Feedback
		}
		return w.SubscriptionLifecycleService.CancelMembership(ctx, &subscriptions.CancelMembershipParams{
			SubscriptionID: &sub.ID,
			CancelType:     models.CancelTypeUser,
			CancelFeedback: feedback,
			RevokeAccess:   false,
		})
	default:
		if w.UserSubscriptionService == nil {
			return fmt.Errorf("user subscription service unavailable")
		}
		return w.UserSubscriptionService.CancelUserSubscription(ctx, userID, job.Args.Feedback)
	}
}

type ResumeSubscriptionWorker struct {
	river.WorkerDefaults[ResumeSubscriptionArgs]
	DB                  *db.DB
	Config              *config.Config
	EntitlementService  *entitlements.EntitlementService
	SubscriptionService *subscriptions.SubscriptionService
}

func (ResumeSubscriptionWorker) Kind() string { return KindSubscriptionResume }

func (w ResumeSubscriptionWorker) Work(ctx context.Context, job *river.Job[ResumeSubscriptionArgs]) error {
	if w.SubscriptionService == nil {
		return fmt.Errorf("subscription service unavailable")
	}
	userID := job.Args.UserID
	if userID == "" {
		return fmt.Errorf("user_id required")
	}

	var sub *models.Subscription
	var err error
	now := time.Now().UTC()

	// If subscription ID is provided, use it directly
	if job.Args.SubscriptionID != uuid.Nil {
		sub, err = w.SubscriptionService.GetByID(ctx, job.Args.SubscriptionID)
		if err != nil {
			log.WithContext(ctx).WithFields(log.Fields{
				"user_id":         userID,
				"subscription_id": job.Args.SubscriptionID,
			}).Info("subscription not found")
			return nil
		}
		// Verify ownership
		if sub.UserID != userID {
			log.WithContext(ctx).WithFields(log.Fields{
				"user_id":         userID,
				"subscription_id": job.Args.SubscriptionID,
			}).Warn("subscription ownership mismatch")
			return nil
		}
	} else {
		// Fallback to active subscription lookup
		sub = new(models.Subscription)
		err = w.DB.GetDB().NewSelect().
			Model(sub).
			Where("sub.user_id = ?", userID).
			Where("sub.status = ?", models.StatusCancelled).
			Where("(sub.current_period_ends_at IS NULL OR sub.current_period_ends_at > ?)", now).
			OrderExpr("sub.created_at DESC").
			Limit(1).
			Scan(ctx)
		if err != nil {
			if err == sql.ErrNoRows {
				log.WithContext(ctx).WithField("user_id", userID).Info("no cancellable subscription to resume")
				return nil
			}
			return err
		}
	}

	if sub.Processor != models.ProcessorStripe {
		return fmt.Errorf("resume unsupported for processor %s", sub.Processor)
	}
	log.WithContext(ctx).WithFields(log.Fields{
		"user_id":         userID,
		"subscription_id": sub.ID,
		"processor":       sub.Processor,
	}).Info("processing subscription resume")
	stripeSvc := &subscriptions.StripeService{Config: w.Config}
	if err := stripeSvc.ResumeSubscription(ctx, sub.ProcessorSubscriptionID); err != nil {
		return err
	}

	sub.Status = models.StatusActive
	sub.CancelledAt = nil
	sub.CancelType = nil
	sub.CancelFeedback = nil
	sub.EndedAt = nil
	if err := w.SubscriptionService.Update(ctx, sub); err != nil {
		return err
	}
	return nil
}
