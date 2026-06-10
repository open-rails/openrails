package riverjobs

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/db/repo"
	"github.com/open-rails/openrails/internal/integrations/nmi"
	"github.com/open-rails/openrails/internal/modules/entitlements"
	"github.com/open-rails/openrails/internal/modules/payments/processors"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"
	log "github.com/sirupsen/logrus"
)

const (
	KindSubscriptionCancel    = "billing.subscription_cancel"
	KindSubscriptionResume    = "billing.subscription_resume"
	KindSubscriptionNMIDelete = "billing.subscription_nmi_delete"
)

// NMIDeleteSubscriptionArgs schedules the deferred NMI delete_subscription for a
// cancellation that retained a future undo window (issue 216).
type NMIDeleteSubscriptionArgs struct {
	UserID         string    `json:"user_id" river:"unique"`
	SubscriptionID uuid.UUID `json:"subscription_id,omitempty" river:"unique"`
}

func (NMIDeleteSubscriptionArgs) Kind() string { return KindSubscriptionNMIDelete }

type CancelSubscriptionArgs struct {
	UserID         string    `json:"user_id" river:"unique"`
	SubscriptionID uuid.UUID `json:"subscription_id,omitempty" river:"unique"`
	Feedback       string    `json:"feedback,omitempty"`
}

func (CancelSubscriptionArgs) Kind() string { return KindSubscriptionCancel }

type ResumeSubscriptionArgs struct {
	UserID         string    `json:"user_id" river:"unique"`
	SubscriptionID uuid.UUID `json:"subscription_id,omitempty" river:"unique"`
}

func (ResumeSubscriptionArgs) Kind() string { return KindSubscriptionResume }

type CancelSubscriptionWorker struct {
	river.WorkerDefaults[CancelSubscriptionArgs]
	DB                           *db.DB
	Config                       *config.Config
	UserSubscriptionService      *subscriptions.UserSubscriptionService
	SubscriptionService          *subscriptions.SubscriptionService
	SubscriptionLifecycleService *subscriptions.SubscriptionLifecycleService
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
		if sub.TenantSubjectID.String() != userID {
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
	DB                           *db.DB
	Config                       *config.Config
	EntitlementService           *entitlements.EntitlementService
	SubscriptionService          *subscriptions.SubscriptionService
	SubscriptionLifecycleService *subscriptions.SubscriptionLifecycleService
	NMIClients                   map[string]*nmi.NMIClient
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
		if sub.TenantSubjectID.String() != userID {
			log.WithContext(ctx).WithFields(log.Fields{
				"user_id":         userID,
				"subscription_id": job.Args.SubscriptionID,
			}).Warn("subscription ownership mismatch")
			return nil
		}
	} else {
		// Fallback to active subscription lookup
		sub = new(models.Subscription)
		tsid, terr := repo.ResolveTenantSubjectID(ctx, w.DB.Qx(ctx), uuid.Nil, userID)
		if terr != nil {
			return terr
		}
		err = w.DB.Q(ctx).NewSelect().
			Model(sub).
			Where("sub.tenant_subject_id = ?", tsid).
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

	// Gate on the single shared resumability predicate (matches the HTTP handler
	// and the DTO). If the precondition no longer holds by the time the job runs
	// (e.g. the paid period elapsed, or an NMI deferred delete already fired),
	// the resume is a no-op rather than a hard failure.
	if !subscriptions.Resumable(sub, now) {
		log.WithContext(ctx).WithFields(log.Fields{
			"user_id":         userID,
			"subscription_id": sub.ID,
			"processor":       sub.Processor,
			"status":          sub.Status,
		}).Info("subscription not resumable; skipping resume")
		return nil
	}

	log.WithContext(ctx).WithFields(log.Fields{
		"user_id":         userID,
		"subscription_id": sub.ID,
		"processor":       sub.Processor,
	}).Info("processing subscription resume")

	if sub.Processor == models.ProcessorStripe {
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

	if processors.IsNMIBackedProcessor(sub.Processor) {
		// NMI in-window resume (issue 216). The NMI subscription was never
		// deleted (the delete was deferred), so no processor-side call is needed.
		// We (1) cancel the pending scheduled-delete job so it cannot fire, then
		// (2) restore the subscription + paid entitlement window via
		// ReactivateMembership, and (3) clear DeletionScheduledAt.
		//
		// The AUTHORITATIVE guard against an erroneous delete is the status flip
		// to active: even if the best-effort job cancel misses, the delete worker
		// re-checks status==cancelled && DeletionScheduledAt!=nil and no-ops once
		// the subscription is active again.
		if w.SubscriptionLifecycleService == nil {
			return fmt.Errorf("subscription lifecycle service unavailable")
		}

		cancelScheduledNMIDelete(ctx, userID, sub.ID)

		periodEnd := sub.CurrentPeriodEndsAt
		reactivated, err := w.SubscriptionLifecycleService.ReactivateMembership(ctx, &subscriptions.ReactivateMembershipParams{
			Processor:               sub.Processor,
			ProcessorSubscriptionID: sub.ProcessorSubscriptionID,
			CurrentPeriodEndsAt:     periodEnd,
			// This is an authorized user-initiated undo within the open window.
			// A user-cancelled subscription is "terminal" per the lifecycle guard,
			// so we explicitly allow the terminal->active transition here. The
			// Resumable predicate (already checked above) is the real gate; the NMI
			// subscription was never deleted, so reactivation is safe.
			AllowTerminalReactivation: true,
		})
		if err != nil {
			return fmt.Errorf("reactivate NMI subscription: %w", err)
		}

		// Clear the now-stale deferred-delete schedule on the reactivated row.
		if reactivated != nil && reactivated.DeletionScheduledAt != nil {
			reactivated.DeletionScheduledAt = nil
			if err := w.SubscriptionService.Update(ctx, reactivated); err != nil {
				return fmt.Errorf("clear deferred delete schedule: %w", err)
			}
		}
		return nil
	}

	log.WithContext(ctx).WithFields(log.Fields{
		"subscription_id": sub.ID,
		"processor":       sub.Processor,
	}).Warn("resumable subscription with unsupported processor path")
	return nil
}

// cancelScheduledNMIDelete best-effort cancels any pending deferred NMI delete
// job for the subscription. It is advisory: the delete worker itself re-checks
// the subscription state and no-ops if the cancellation was resumed, so a missed
// cancel here does not cause an erroneous delete.
func cancelScheduledNMIDelete(ctx context.Context, userID string, subscriptionID uuid.UUID) {
	client := river.ClientFromContext[pgx.Tx](ctx)
	if client == nil {
		return
	}
	jobs, err := client.JobList(ctx, river.NewJobListParams().
		Kinds(KindSubscriptionNMIDelete).
		States(rivertype.JobStateScheduled, rivertype.JobStateAvailable, rivertype.JobStateRetryable, rivertype.JobStatePending).
		First(1000))
	if err != nil {
		log.WithContext(ctx).WithError(err).Warn("failed to list scheduled NMI delete jobs for cancellation")
		return
	}
	for _, j := range jobs.Jobs {
		var args NMIDeleteSubscriptionArgs
		if err := json.Unmarshal(j.EncodedArgs, &args); err != nil {
			continue
		}
		if args.SubscriptionID == subscriptionID && args.UserID == userID {
			if _, err := client.JobCancel(ctx, j.ID); err != nil {
				log.WithContext(ctx).WithError(err).WithField("job_id", j.ID).Warn("failed to cancel scheduled NMI delete job")
			}
		}
	}
}

// NMIDeleteSubscriptionWorker executes the deferred NMI delete_subscription for a
// cancelled subscription whose undo window has elapsed (issue 216). It is
// idempotent and retry-safe: it re-checks the subscription state on every run and
// no-ops if the cancellation was resumed (status no longer cancelled, or
// DeletionScheduledAt cleared) or already finalized.
type NMIDeleteSubscriptionWorker struct {
	river.WorkerDefaults[NMIDeleteSubscriptionArgs]
	DB                  *db.DB
	Config              *config.Config
	SubscriptionService *subscriptions.SubscriptionService
	NMIClients          map[string]*nmi.NMIClient
}

func (NMIDeleteSubscriptionWorker) Kind() string { return KindSubscriptionNMIDelete }

func (w NMIDeleteSubscriptionWorker) Work(ctx context.Context, job *river.Job[NMIDeleteSubscriptionArgs]) error {
	if w.SubscriptionService == nil {
		return fmt.Errorf("subscription service unavailable")
	}
	if job.Args.SubscriptionID == uuid.Nil {
		return fmt.Errorf("subscription_id required")
	}

	sub, err := w.SubscriptionService.GetByID(ctx, job.Args.SubscriptionID)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil
		}
		return err
	}

	// Idempotency / resume guard: only proceed if the subscription is still
	// cancelled with a pending deferred delete. If it was resumed, the status is
	// active or DeletionScheduledAt was cleared -> no-op.
	if sub.Status != models.StatusCancelled || sub.DeletionScheduledAt == nil {
		log.WithContext(ctx).WithFields(log.Fields{
			"subscription_id": sub.ID,
			"status":          sub.Status,
		}).Info("deferred NMI delete skipped (resumed or already finalized)")
		return nil
	}

	provider := strings.ToLower(string(sub.Processor))
	if w.NMIClients != nil {
		if client, ok := w.NMIClients[provider]; ok && sub.ProcessorSubscriptionID != "" {
			if err := client.DeleteRecurringSubscription(sub.ProcessorSubscriptionID); err != nil {
				// Return the error so River retries; the NMI subscription must be
				// deleted before its rebill, so retries matter.
				return fmt.Errorf("deferred delete: failed to delete NMI subscription '%s': %w", provider, err)
			}
		}
	}

	// Finalize: clear the schedule so the cancellation is now destructive
	// (no longer resumable) and the job cannot re-fire meaningfully.
	sub.DeletionScheduledAt = nil
	if err := w.SubscriptionService.Update(ctx, sub); err != nil {
		return err
	}

	log.WithContext(ctx).WithFields(log.Fields{
		"subscription_id": sub.ID,
		"processor":       sub.Processor,
	}).Info("deferred NMI delete executed")
	return nil
}
