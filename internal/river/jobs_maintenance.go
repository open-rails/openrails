package riverjobs

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/repo"
	"github.com/open-rails/openrails/internal/integrations/ccbill"
	"github.com/open-rails/openrails/internal/modules/catalog"
	"github.com/open-rails/openrails/internal/modules/idempotency"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/internal/modules/webhooks"
	"github.com/redis/go-redis/v9"
	"github.com/riverqueue/river"
	log "github.com/sirupsen/logrus"
)

const (
	KindIdempotencyCleanup = "openrails.idempotency_cleanup"
	KindCCBillReconcile    = "openrails.ccbill_reconcile"
)

type IdempotencyCleanupArgs struct{}

func (IdempotencyCleanupArgs) Kind() string { return KindIdempotencyCleanup }

type IdempotencyCleanupWorker struct {
	river.WorkerDefaults[IdempotencyCleanupArgs]
	Config *config.Config
}

func (IdempotencyCleanupWorker) Kind() string { return KindIdempotencyCleanup }

func (w IdempotencyCleanupWorker) Work(ctx context.Context, job *river.Job[IdempotencyCleanupArgs]) error {
	var redisClient *redis.Client
	if w.Config != nil && w.Config.Redis != nil && w.Config.Redis.Addr != "" {
		redisOpts := &redis.Options{
			Addr: w.Config.Redis.Addr,
			DB:   w.Config.Redis.DB,
		}
		if w.Config.Redis.Password != "" {
			redisOpts.Password = w.Config.Redis.Password
		}
		redisClient = redis.NewClient(redisOpts)
		defer func() {
			if err := redisClient.Close(); err != nil {
				log.WithContext(ctx).WithError(err).Warn("IdempotencyCleanup: failed to close redis client")
			}
		}()

		pingCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		if _, err := redisClient.Ping(pingCtx).Result(); err != nil {
			log.WithContext(ctx).WithError(err).Warn("IdempotencyCleanup: Redis ping failed, idempotency will use in-memory fallback")
		}
		cancel()
	}

	const retentionDays = 90

	idempotencyService := idempotency.NewIdempotencyService(redisClient)
	defer idempotencyService.Close()
	dedupeService := webhooks.NewDeduplicationService(idempotencyService)
	deletedRows, err := dedupeService.CleanupOldWebhooks(ctx, retentionDays)
	if err != nil {
		return fmt.Errorf("cleanup durable webhook dedupe records: %w", err)
	}

	log.WithContext(ctx).WithFields(log.Fields{
		"retention_days": retentionDays,
		"deleted_rows":   deletedRows,
	}).Info("IdempotencyCleanup: completed")
	return nil
}

type CCBillReconcileArgs struct{}

func (CCBillReconcileArgs) Kind() string { return KindCCBillReconcile }

type CCBillReconcileWorker struct {
	river.WorkerDefaults[CCBillReconcileArgs]
	DB                  *db.DB
	DataLink            *ccbill.DataLinkClient
	NotificationService *subscriptions.NotificationService
}

func (CCBillReconcileWorker) Kind() string { return KindCCBillReconcile }

func (w CCBillReconcileWorker) Work(ctx context.Context, job *river.Job[CCBillReconcileArgs]) error {
	if w.DataLink == nil {
		log.WithContext(ctx).Info("CCBillReconcile: DataLink not configured; skipping")
		return nil
	}
	if w.DB == nil {
		return fmt.Errorf("ccbill datalink reconcile: db not configured")
	}
	records, err := w.DataLink.FetchActiveMembers(ctx)
	if err != nil {
		return fmt.Errorf("ccbill datalink reconcile: %w", err)
	}
	remoteActive := make(map[string]ccbill.CCBillRecord, len(records))
	for _, record := range records {
		if !ccbill.IsDataLinkActiveStatus(record.Status) {
			continue
		}
		remoteActive[strings.TrimSpace(record.SubscriptionID)] = record
	}
	priceService := catalog.NewPriceService(w.DB)
	productService := catalog.NewProductService(w.DB)
	subscriptionService := subscriptions.NewSubscriptionService(w.DB, priceService, productService, nil, nil, nil)
	lifecycleService := &subscriptions.SubscriptionLifecycleService{DB: w.DB}
	localActive, err := subscriptionService.GetActiveSubscriptionsByProcessor(ctx, "ccbill")
	if err != nil {
		return fmt.Errorf("load local ccbill subscriptions: %w", err)
	}
	if len(remoteActive) == 0 && len(localActive) > 0 {
		return fmt.Errorf("ccbill datalink reconcile refused to repair: remote active set is empty while %d local active subscriptions exist", len(localActive))
	}
	localByProcessorID := make(map[string]struct{}, len(localActive))
	missingRemote := 0
	for _, sub := range localActive {
		processorSubID := strings.TrimSpace(sub.ProcessorSubscriptionID)
		if processorSubID == "" {
			continue
		}
		localByProcessorID[processorSubID] = struct{}{}
		if _, ok := remoteActive[processorSubID]; !ok {
			missingRemote++
			if err := w.recordDataLinkRepairAlert(ctx, "ccbill_datalink_missing_remote", &sub.ID, sub.MerchantSubjectID.String(), processorSubID, nil, nil); err != nil {
				log.WithContext(ctx).WithError(err).Warn("CCBillReconcile: failed to persist missing-remote repair alert")
			}
			log.WithContext(ctx).WithFields(log.Fields{
				"subscription_id":           sub.ID,
				"processor_subscription_id": processorSubID,
				"user_id":                   sub.MerchantSubjectID.String(),
			}).Warn("CCBillReconcile: local active subscription missing from DataLink active members; quarantine for manual review")
		}
	}
	missingLocal := 0
	reactivatedLocal := 0
	for processorSubID, record := range remoteActive {
		if _, ok := localByProcessorID[processorSubID]; ok {
			continue
		}
		missingLocal++
		existing, err := subscriptionService.GetByProcessorSubscriptionID(ctx, "ccbill", processorSubID)
		if err != nil {
			if repo.IsNotFound(err) {
				if alertErr := w.recordDataLinkRepairAlert(ctx, "ccbill_datalink_missing_local", nil, "", processorSubID, &record, nil); alertErr != nil {
					log.WithContext(ctx).WithError(alertErr).Warn("CCBillReconcile: failed to persist missing-local repair alert")
				}
				log.WithContext(ctx).WithFields(log.Fields{
					"processor_subscription_id": processorSubID,
					"username":                  record.Username,
					"email":                     record.Email,
					"rebill_date":               record.RebillDate,
					"expiry_date":               record.ExpiryDate,
				}).Warn("CCBillReconcile: DataLink active member has no local subscription record; manual repair required")
				continue
			}
			return fmt.Errorf("load ccbill subscription %s for reactivation: %w", processorSubID, err)
		}
		if existing.Status == "active" {
			continue
		}
		paidThrough, ok := ccbill.DataLinkPaidThrough(record, time.Now().UTC())
		if !ok {
			if alertErr := w.recordDataLinkRepairAlert(ctx, "ccbill_datalink_missing_paid_through", &existing.ID, existing.MerchantSubjectID.String(), processorSubID, &record, nil); alertErr != nil {
				log.WithContext(ctx).WithError(alertErr).Warn("CCBillReconcile: failed to persist missing-paid-through repair alert")
			}
			log.WithContext(ctx).WithFields(log.Fields{
				"subscription_id":           existing.ID,
				"processor_subscription_id": processorSubID,
				"rebill_date":               record.RebillDate,
				"expiry_date":               record.ExpiryDate,
			}).Warn("CCBillReconcile: cannot reactivate local subscription without future paid-through date")
			continue
		}
		if _, err := lifecycleService.ReactivateMembership(ctx, &subscriptions.ReactivateMembershipParams{
			Processor:               "ccbill",
			ProcessorSubscriptionID: processorSubID,
			CurrentPeriodEndsAt:     &paidThrough,
		}); err != nil {
			if subscriptions.IsTerminalTransitionBlocked(err) {
				if alertErr := w.recordDataLinkRepairAlert(ctx, "ccbill_datalink_terminal_reactivation_blocked", &existing.ID, existing.MerchantSubjectID.String(), processorSubID, &record, err); alertErr != nil {
					log.WithContext(ctx).WithError(alertErr).Warn("CCBillReconcile: failed to persist terminal-blocked repair alert")
				}
				log.WithContext(ctx).WithError(err).WithFields(log.Fields{
					"subscription_id":           existing.ID,
					"processor_subscription_id": processorSubID,
				}).Warn("CCBillReconcile: reactivation blocked by terminal transition policy")
				continue
			}
			return fmt.Errorf("reactivate ccbill subscription %s: %w", existing.ID, err)
		}
		reactivatedLocal++
		log.WithContext(ctx).WithFields(log.Fields{
			"processor_subscription_id": processorSubID,
			"username":                  record.Username,
			"email":                     record.Email,
			"rebill_date":               record.RebillDate,
			"expiry_date":               record.ExpiryDate,
		}).Warn("CCBillReconcile: reactivated local subscription from DataLink active member")
	}
	log.WithContext(ctx).WithFields(log.Fields{
		"record_count":         len(records),
		"remote_active_count":  len(remoteActive),
		"local_active_count":   len(localActive),
		"missing_remote_count": missingRemote,
		"missing_local_count":  missingLocal,
		"reactivated_count":    reactivatedLocal,
		"reconcile_mode":       "guarded_repair_no_destructive_expiry",
	}).Info("CCBillReconcile: completed guarded repair scan")
	return nil
}

func (w CCBillReconcileWorker) recordDataLinkRepairAlert(ctx context.Context, operation string, subscriptionID *uuid.UUID, userID, processorSubID string, record *ccbill.CCBillRecord, err error) error {
	metadata := map[string]any{"processor_subscription_id": strings.TrimSpace(processorSubID)}
	if record != nil {
		metadata["datalink_status"] = record.Status
		metadata["datalink_rebill_date"] = record.RebillDate
		metadata["datalink_expiry_date"] = record.ExpiryDate
		metadata["datalink_username"] = record.Username
		metadata["datalink_email"] = record.Email
	}
	return webhooks.RecordLedgerRepairAlert(ctx, w.NotificationService, w.DB, time.Now().UTC(), webhooks.LedgerRepairAlert{
		Provider:       "ccbill",
		Operation:      operation,
		SubscriptionID: subscriptionID,
		UserID:         userID,
		Err:            err,
		Metadata:       metadata,
	})
}
