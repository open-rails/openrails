package riverjobs

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/db/repo"
	"github.com/open-rails/openrails/internal/integrations/nmi"
	"github.com/open-rails/openrails/internal/intents"
	"github.com/open-rails/openrails/internal/modules/entitlements"
	"github.com/open-rails/openrails/internal/modules/payments/rails"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/pkg/merchant"
	"github.com/riverqueue/river"
	log "github.com/sirupsen/logrus"
)

const (
	KindSubscriptionCancel = "openrails.subscription_cancel"
	KindSubscriptionResume = "openrails.subscription_resume"
)

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
	Rails                        config.RailMerchantAccountSet
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
		if sub.CustomerID.String() != userID {
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
	// Pin the owning merchant on the job context (#336): the cancel persists a
	// deferred-delete intent and re-converges the customer, both of which require
	// merchant resolution. River job contexts carry no ambient merchant.
	ctx = merchant.WithID(ctx, merchant.ID(sub.MerchantID))

	log.WithContext(ctx).WithFields(log.Fields{
		"user_id":         userID,
		"subscription_id": sub.ID,
		"rail":            sub.Rail,
	}).Info("processing subscription cancellation")

	switch sub.Rail {
	case models.RailStripe:
		if w.SubscriptionLifecycleService == nil {
			return fmt.Errorf("subscription lifecycle service unavailable")
		}
		stripeSvc := &subscriptions.StripeService{Config: w.Config, Rails: w.Rails}
		if err := stripeSvc.CancelSubscription(ctx, sub.RailSubscriptionID); err != nil {
			return err
		}
		var feedback *string
		if job.Args.Feedback != "" {
			feedback = &job.Args.Feedback
		}
		if err := w.SubscriptionLifecycleService.CancelMembership(ctx, &subscriptions.CancelMembershipParams{
			SubscriptionID: &sub.ID,
			CancelType:     models.CancelTypeUser,
			CancelFeedback: feedback,
			RevokeAccess:   false,
		}); err != nil {
			return err
		}
	default:
		if w.UserSubscriptionService == nil {
			return fmt.Errorf("user subscription service unavailable")
		}
		if err := w.UserSubscriptionService.CancelUserSubscription(ctx, userID, job.Args.Feedback); err != nil {
			return err
		}
	}

	// #511 Phase E: re-converge the customer inline after the cancel commits, so
	// any drift this lifecycle transition implies (lapsed grace windows, a stalled
	// dunning schedule, grant effects to retract) is reconciled immediately rather
	// than waiting for the next sweep. Best-effort — the sweep is the backstop.
	convergeCustomerInline(ctx, w.DB, sub.MerchantID, sub.CustomerID, KindSubscriptionCancel)
	return nil
}

type ResumeSubscriptionWorker struct {
	river.WorkerDefaults[ResumeSubscriptionArgs]
	DB                           *db.DB
	Config                       *config.Config
	Rails                        config.RailMerchantAccountSet
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
		if sub.CustomerID.String() != userID {
			log.WithContext(ctx).WithFields(log.Fields{
				"user_id":         userID,
				"subscription_id": job.Args.SubscriptionID,
			}).Warn("subscription ownership mismatch")
			return nil
		}
	} else {
		// Fallback to active subscription lookup
		tsid, terr := repo.ResolveCustomerID(userID)
		if terr != nil {
			return terr
		}
		sub, err = repo.NewSubscriptionRepo(w.DB).GetLatestResumableCancelled(ctx, tsid, now)
		if err != nil {
			if repo.IsNotFound(err) {
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
			"rail":            sub.Rail,
			"status":          sub.Status,
		}).Info("subscription not resumable; skipping resume")
		return nil
	}

	log.WithContext(ctx).WithFields(log.Fields{
		"user_id":         userID,
		"subscription_id": sub.ID,
		"rail":            sub.Rail,
	}).Info("processing subscription resume")

	if sub.Rail == models.RailStripe {
		stripeSvc := &subscriptions.StripeService{Config: w.Config, Rails: w.Rails}
		if err := stripeSvc.ResumeSubscription(ctx, sub.RailSubscriptionID); err != nil {
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

	if rails.IsNMI(sub.Rail) {
		// NMI in-window resume (issue 216). The NMI subscription was never
		// deleted (the delete was deferred), so no rail-side call is needed.
		// We (1) supersede the pending deferred-delete intent on the ledger
		// (#358) so it cannot fire, then (2) restore the subscription + paid
		// entitlement window via ReactivateMembership, and (3) clear
		// DeletionScheduledAt.
		//
		// The AUTHORITATIVE guard against an erroneous delete is the status flip
		// to active: even if the best-effort supersede misses, the intent
		// executor's relevance check re-reads status==cancelled &&
		// DeletionScheduledAt!=nil and supersedes on its own once the
		// subscription is active again.
		if w.SubscriptionLifecycleService == nil {
			return fmt.Errorf("subscription lifecycle service unavailable")
		}

		supersedeScheduledNMIDeleteIntent(ctx, w.DB, userID, sub.ID)

		periodEnd := sub.CurrentPeriodEndsAt
		reactivated, err := w.SubscriptionLifecycleService.ReactivateMembership(ctx, &subscriptions.ReactivateMembershipParams{
			Rail:                sub.Rail,
			RailSubscriptionID:  sub.RailSubscriptionID,
			CurrentPeriodEndsAt: periodEnd,
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
		"rail":            sub.Rail,
	}).Warn("resumable subscription with unsupported rail path")
	return nil
}

// supersedeScheduledNMIDeleteIntent best-effort supersedes any live deferred
// NMI delete intent for the subscription on the provider intent ledger
// (#358). It is advisory: the intent executor's relevance check re-reads the
// subscription state and supersedes on its own if the cancellation was
// resumed, so a missed supersede here does not cause an erroneous delete.
//
// (The deferred delete itself executes via the provider intent executor —
// see internal/intents.NMIDeleteHandler — which replaced the
// NMIDeleteSubscription River worker in #358 phase A.)
func supersedeScheduledNMIDeleteIntent(ctx context.Context, dbi *db.DB, userID string, subscriptionID uuid.UUID) {
	if dbi == nil {
		return
	}
	n, err := intents.NewStore(dbi).SupersedeBySubject(ctx, intents.TypeNMIDeleteSubscription, subscriptionID,
		"cancellation undone (resume) for user "+userID)
	if err != nil {
		log.WithContext(ctx).WithError(err).WithField("subscription_id", subscriptionID).
			Warn("failed to supersede scheduled NMI delete intent; executor relevance check remains the guard")
		return
	}
	if n > 0 {
		log.WithContext(ctx).WithFields(log.Fields{
			"subscription_id": subscriptionID,
			"superseded":      n,
		}).Info("superseded pending deferred NMI delete intent on resume")
	}
}
