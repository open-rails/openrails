package riverjobs

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jonboulle/clockwork"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/modules/credits"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/internal/shared/uuidutil"
	"github.com/riverqueue/river"
	log "github.com/sirupsen/logrus"
	"github.com/uptrace/bun"
)

const KindLowBalanceAlert = "billing.low_balance_alert"

type LowBalanceAlertArgs struct{}

func (LowBalanceAlertArgs) Kind() string { return KindLowBalanceAlert }

// LowBalanceAlertWorker completes the deferred #116 low-balance alert (issue
// #240). Each run scans owner credit balances whose AVAILABLE amount
// (balance - held_balance) has dropped below the credit type's configured
// low_balance_threshold and which have not been alerted within the cooldown,
// emits the reusable credits.LowBalanceEvent SIGNAL to a sink, and stamps
// user_credit_balances.last_low_balance_alert_at so the same owner is not
// re-alerted until the cooldown elapses.
//
// The default sink delivers an in-app + email notification via the
// NotificationService. The SAME signal is the hook #239 (auto-top-up) and #241
// (arrears) consume later: compose additional credits.LowBalanceSink values into
// a credits.MultiLowBalanceSink and pass it as Sink — the detection loop here is
// unchanged.
//
// Owner/tenant scoping (issue #221/#223): the scan runs cross-tenant (like
// CreditExpiryWorker) but every row carries tenant_id + owner_id and each owner's
// alert state is tracked independently, so one owner being alerted never
// suppresses another's alert and no row leaks across owners/tenants.
type LowBalanceAlertWorker struct {
	river.WorkerDefaults[LowBalanceAlertArgs]
	DB                  *db.DB
	Config              *config.Config
	Clock               clockwork.Clock
	NotificationService *subscriptions.NotificationService
	// Sink receives every LowBalanceEvent. When nil the worker falls back to a
	// notification-only sink built from NotificationService. Downstream features
	// (#239/#241) inject a credits.MultiLowBalanceSink here.
	Sink      credits.LowBalanceSink
	BatchSize int
}

func (LowBalanceAlertWorker) Kind() string { return KindLowBalanceAlert }

// lowBalanceRow is the projection of the threshold join the scan reads.
type lowBalanceRow struct {
	BalanceID      uuid.UUID `bun:"balance_id"`
	TenantID       uuid.UUID `bun:"tenant_id"`
	OwnerID        uuid.UUID `bun:"owner_id"`
	UserID         string    `bun:"user_id"`
	CreditTypeID   uuid.UUID `bun:"credit_type_id"`
	CreditTypeName string    `bun:"credit_type_name"`
	Available      int64     `bun:"available"`
	Threshold      int64     `bun:"threshold"`
}

func (w LowBalanceAlertWorker) Work(ctx context.Context, job *river.Job[LowBalanceAlertArgs]) error {
	clock := w.Clock
	if clock == nil {
		clock = clockwork.NewRealClock()
	}
	batchSize := w.BatchSize
	if batchSize <= 0 {
		batchSize = 500
	}
	cooldown := 24 * time.Hour
	if w.Config != nil {
		cooldown = w.Config.LowBalanceAlertCooldown()
	}
	now := clock.Now().UTC()
	cooldownCutoff := now.Add(-cooldown)
	logger := log.WithContext(ctx).WithField("worker", KindLowBalanceAlert)

	sink := w.Sink
	if sink == nil {
		sink = w.notificationSink()
	}

	bunDB := w.DB.GetDB().(*bun.DB)

	// Candidate scan: balances whose available amount is under the per-type
	// threshold AND which are eligible to alert (never alerted, or alerted longer
	// ago than the cooldown). credit_types.low_balance_threshold IS NULL disables
	// alerting for that type. Ordered + limited so a large fleet drains over
	// successive runs rather than in one unbounded pass.
	var rows []lowBalanceRow
	if err := bunDB.NewSelect().
		ColumnExpr("ucb.id AS balance_id").
		ColumnExpr("ucb.tenant_id AS tenant_id").
		ColumnExpr("ucb.owner_id AS owner_id").
		ColumnExpr("ucb.user_id AS user_id").
		ColumnExpr("ucb.credit_type_id AS credit_type_id").
		ColumnExpr("ct.name AS credit_type_name").
		ColumnExpr("(ucb.balance - ucb.held_balance) AS available").
		ColumnExpr("ct.low_balance_threshold AS threshold").
		TableExpr("billing.user_credit_balances AS ucb").
		Join("JOIN billing.credit_types AS ct ON ct.id = ucb.credit_type_id").
		Where("ct.low_balance_threshold IS NOT NULL").
		Where("ct.is_active = TRUE").
		Where("(ucb.balance - ucb.held_balance) < ct.low_balance_threshold").
		Where("(ucb.last_low_balance_alert_at IS NULL OR ucb.last_low_balance_alert_at <= ?)", cooldownCutoff).
		OrderExpr("ucb.last_low_balance_alert_at ASC NULLS FIRST, ucb.id ASC").
		Limit(batchSize).
		Scan(ctx, &rows); err != nil {
		return fmt.Errorf("scan low-balance candidates: %w", err)
	}

	if len(rows) == 0 {
		logger.Debug("no owners under low-balance threshold")
		return nil
	}

	alerted := 0
	for i := range rows {
		r := rows[i]

		// Stamp last_low_balance_alert_at FIRST, conditioned on the cooldown still
		// being satisfied. This claims the alert atomically: if a concurrent run
		// (or this owner having been alerted since the scan) already stamped a
		// recent value, the UPDATE matches zero rows and we skip — guaranteeing at
		// most one alert per cooldown even with overlapping job runs.
		res, err := bunDB.NewUpdate().
			Model((*models.UserCreditBalance)(nil)).
			Set("last_low_balance_alert_at = ?", now).
			Set("updated_at = ?", now).
			Where("id = ?", r.BalanceID).
			Where("last_low_balance_alert_at IS NULL OR last_low_balance_alert_at <= ?", cooldownCutoff).
			Exec(ctx)
		if err != nil {
			logger.WithError(err).WithField("balance_id", r.BalanceID).Error("failed to claim low-balance alert")
			continue
		}
		if n, _ := res.RowsAffected(); n == 0 {
			// Lost the race / already alerted within cooldown — skip without emitting.
			continue
		}

		ev := credits.LowBalanceEvent{
			TenantID:       r.TenantID,
			OwnerID:        r.OwnerID,
			UserID:         r.UserID,
			CreditTypeID:   r.CreditTypeID,
			CreditTypeName: r.CreditTypeName,
			Available:      r.Available,
			Threshold:      r.Threshold,
			DetectedAt:     now,
		}

		// Best-effort emit: a sink failure is logged but does NOT roll back the
		// stamp — otherwise a flaky notifier would re-alert the same owner every
		// run. The stamped timestamp is the source of truth for the cooldown.
		if err := sink.Handle(ctx, ev); err != nil {
			logger.WithError(err).WithFields(log.Fields{
				"owner_id":    r.OwnerID,
				"tenant_id":   r.TenantID,
				"credit_type": r.CreditTypeName,
			}).Error("low-balance sink failed (alert state already stamped)")
		}
		alerted++
	}

	logger.WithFields(log.Fields{
		"candidates": len(rows),
		"alerted":    alerted,
	}).Info("low-balance alert run complete")
	return nil
}

// notificationSink builds the default sink: an in-app + email low-balance
// notification per owner. When NotificationService is nil the sink is a no-op so
// the cooldown bookkeeping still runs (useful in tests / minimal deployments).
func (w LowBalanceAlertWorker) notificationSink() credits.LowBalanceSink {
	return credits.LowBalanceSinkFunc(func(ctx context.Context, ev credits.LowBalanceEvent) error {
		if w.NotificationService == nil {
			return nil
		}
		return w.NotificationService.CreateAndDeliver(ctx, &models.NotificationQueue{
			ID:        uuidutil.NewV7(),
			UserID:    ev.UserID,
			EventType: models.NotificationLowBalance,
			Data: map[string]any{
				"tenant_id":   ev.TenantID.String(),
				"owner_id":    ev.OwnerID.String(),
				"credit_type": ev.CreditTypeName,
				"available":   ev.Available,
				"threshold":   ev.Threshold,
			},
			CreatedAt: ev.DetectedAt,
		})
	})
}
