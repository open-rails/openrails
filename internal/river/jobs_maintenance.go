package riverjobs

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/integrations/ccbill"
	"github.com/open-rails/openrails/internal/modules/catalog"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/internal/modules/webhooks"
	log "github.com/sirupsen/logrus"
)

// CCBillReconciler is the CCBill DataLink guarded-repair scan. It is NOT a River
// job: ProviderRefreshWorker's CCBill lane calls Run directly.
type CCBillReconciler struct {
	DB                  *db.DB
	DataLink            *ccbill.DataLinkClient
	NotificationService *subscriptions.NotificationService
}

func (w CCBillReconciler) Run(ctx context.Context) error {
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
	subscriptionService := subscriptions.NewSubscriptionService(w.DB, priceService, productService, nil)
	lifecycleService := &subscriptions.SubscriptionLifecycleService{DB: w.DB}
	localActive, err := subscriptionService.GetActiveSubscriptionsByRail(ctx, "ccbill")
	if err != nil {
		return fmt.Errorf("load local ccbill subscriptions: %w", err)
	}
	if len(remoteActive) == 0 && len(localActive) > 0 {
		return fmt.Errorf("ccbill datalink reconcile refused to repair: remote active set is empty while %d local active subscriptions exist", len(localActive))
	}
	localByRailID := make(map[string]struct{}, len(localActive))
	missingRemote := 0
	for _, sub := range localActive {
		railSubID := strings.TrimSpace(sub.RailSubscriptionID)
		if railSubID == "" {
			continue
		}
		localByRailID[railSubID] = struct{}{}
		if _, ok := remoteActive[railSubID]; !ok {
			missingRemote++
			if err := w.recordDataLinkRepairAlert(ctx, "ccbill_datalink_missing_remote", &sub.ID, sub.CustomerID.String(), railSubID, nil, nil); err != nil {
				log.WithContext(ctx).WithError(err).Warn("CCBillReconcile: failed to persist missing-remote repair alert")
			}
			log.WithContext(ctx).WithFields(log.Fields{
				"subscription_id":      sub.ID,
				"rail_subscription_id": railSubID,
				"user_id":              sub.CustomerID.String(),
			}).Warn("CCBillReconcile: local active subscription missing from DataLink active members; quarantine for manual review")
		}
	}
	missingLocal := 0
	reactivatedLocal := 0
	for railSubID, record := range remoteActive {
		if _, ok := localByRailID[railSubID]; ok {
			continue
		}
		missingLocal++
		existing, err := subscriptionService.GetByRailSubscriptionID(ctx, "ccbill", railSubID)
		if err != nil {
			if db.IsNotFound(err) {
				if alertErr := w.recordDataLinkRepairAlert(ctx, "ccbill_datalink_missing_local", nil, "", railSubID, &record, nil); alertErr != nil {
					log.WithContext(ctx).WithError(alertErr).Warn("CCBillReconcile: failed to persist missing-local repair alert")
				}
				log.WithContext(ctx).WithFields(log.Fields{
					"rail_subscription_id": railSubID,
					"username":             record.Username,
					"email":                record.Email,
					"rebill_date":          record.RebillDate,
					"expiry_date":          record.ExpiryDate,
				}).Warn("CCBillReconcile: DataLink active member has no local subscription record; manual repair required")
				continue
			}
			return fmt.Errorf("load ccbill subscription %s for reactivation: %w", railSubID, err)
		}
		if existing.Status == "active" {
			continue
		}
		paidThrough, ok := ccbill.DataLinkPaidThrough(record, time.Now().UTC())
		if !ok {
			if alertErr := w.recordDataLinkRepairAlert(ctx, "ccbill_datalink_missing_paid_through", &existing.ID, existing.CustomerID.String(), railSubID, &record, nil); alertErr != nil {
				log.WithContext(ctx).WithError(alertErr).Warn("CCBillReconcile: failed to persist missing-paid-through repair alert")
			}
			log.WithContext(ctx).WithFields(log.Fields{
				"subscription_id":      existing.ID,
				"rail_subscription_id": railSubID,
				"rebill_date":          record.RebillDate,
				"expiry_date":          record.ExpiryDate,
			}).Warn("CCBillReconcile: cannot reactivate local subscription without future paid-through date")
			continue
		}
		if _, err := lifecycleService.ReactivateMembership(ctx, &subscriptions.ReactivateMembershipParams{
			Rail:                "ccbill",
			RailSubscriptionID:  railSubID,
			CurrentPeriodEndsAt: &paidThrough,
		}); err != nil {
			if subscriptions.IsTerminalTransitionBlocked(err) {
				if alertErr := w.recordDataLinkRepairAlert(ctx, "ccbill_datalink_terminal_reactivation_blocked", &existing.ID, existing.CustomerID.String(), railSubID, &record, err); alertErr != nil {
					log.WithContext(ctx).WithError(alertErr).Warn("CCBillReconcile: failed to persist terminal-blocked repair alert")
				}
				log.WithContext(ctx).WithError(err).WithFields(log.Fields{
					"subscription_id":      existing.ID,
					"rail_subscription_id": railSubID,
				}).Warn("CCBillReconcile: reactivation blocked by terminal transition policy")
				continue
			}
			return fmt.Errorf("reactivate ccbill subscription %s: %w", existing.ID, err)
		}
		reactivatedLocal++
		log.WithContext(ctx).WithFields(log.Fields{
			"rail_subscription_id": railSubID,
			"username":             record.Username,
			"email":                record.Email,
			"rebill_date":          record.RebillDate,
			"expiry_date":          record.ExpiryDate,
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

func (w CCBillReconciler) recordDataLinkRepairAlert(ctx context.Context, operation string, subscriptionID *uuid.UUID, userID, railSubID string, record *ccbill.CCBillRecord, err error) error {
	metadata := map[string]any{"rail_subscription_id": strings.TrimSpace(railSubID)}
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
