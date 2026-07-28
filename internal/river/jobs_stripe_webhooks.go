package riverjobs

import (
	"context"
	"fmt"

	"github.com/riverqueue/river"
	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/internal/modules/catalog"
	"github.com/open-rails/openrails/internal/modules/webhooks"
	"github.com/open-rails/openrails/pkg/merchant"
)

const KindStripeWebhookReconcile = "openrails.stripe_webhook_reconcile"

type StripeWebhookReconcileArgs struct{}

func (StripeWebhookReconcileArgs) Kind() string { return KindStripeWebhookReconcile }

type StripeWebhookReconcileWorker struct {
	river.WorkerDefaults[StripeWebhookReconcileArgs]
	DB        *db.DB
	Config    *config.Config
	Merchants *merchants.Service
}

func (StripeWebhookReconcileWorker) Kind() string { return KindStripeWebhookReconcile }

func (w StripeWebhookReconcileWorker) Work(ctx context.Context, job *river.Job[StripeWebhookReconcileArgs]) error {
	_ = job
	if w.DB == nil {
		return fmt.Errorf("stripe webhook reconcile: db not configured")
	}
	if w.Config == nil {
		return fmt.Errorf("stripe webhook reconcile: config not configured")
	}
	if w.Merchants == nil || w.Merchants.Secrets() == nil {
		// #788: managed webhook registration resolves ONLY from the armed
		// rail state (psps + secret store); without an
		// armed merchants service there is nothing to reconcile.
		log.WithContext(ctx).Info("StripeWebhookReconcile: merchants service not armed; skipping")
		return nil
	}

	// Register managed endpoints for Stripe accounts that are still eligible for
	// new work. Archived accounts keep their existing webhook secrets/routes for
	// draining, but this worker does not mutate retired Stripe accounts.
	rows, err := w.DB.Qx(ctx).Query(ctx, `
		SELECT m.id::text, m.slug, pa.environment, pa.account_id
		  FROM openrails.merchants m
		  JOIN openrails.psps pa ON pa.merchant_id = m.id
		 WHERE m.deleted_at IS NULL
		   AND pa.rail = 'stripe'
		   AND pa.archived = false
		 ORDER BY m.slug, pa.account_id
	`)
	if err != nil {
		return fmt.Errorf("stripe webhook reconcile: list merchants: %w", err)
	}
	defer rows.Close()

	count, skipped, failed := 0, 0, 0
	for rows.Next() {
		var idStr, slug, environment, accountID string
		if err := rows.Scan(&idStr, &slug, &environment, &accountID); err != nil {
			return fmt.Errorf("stripe webhook reconcile: scan merchant: %w", err)
		}
		id, err := merchant.ParseID(idStr)
		if err != nil {
			return fmt.Errorf("stripe webhook reconcile: parse merchant id %q: %w", idStr, err)
		}
		res, err := catalog.ReconcileManagedStripeWebhook(ctx, catalog.ManagedStripeWebhookParams{
			Config:              w.Config,
			SecretStore:         w.Merchants.Secrets(),
			MerchantID:          id,
			MerchantSlug:        slug,
			ProviderEnvironment: environment,
			PspID:               accountID,
			EnabledEvents:       webhooks.HandledStripeEventTypes,
		})
		fields := log.Fields{"merchant": slug, "stripe_account_id": accountID}
		if err != nil {
			failed++
			log.WithContext(ctx).WithError(err).WithFields(fields).Warn("StripeWebhookReconcile: merchant reconcile failed")
			continue
		}
		if res.Skipped {
			skipped++
			fields["reason"] = res.SkipReason
			log.WithContext(ctx).WithFields(fields).Info("StripeWebhookReconcile: merchant skipped")
			continue
		}
		count++
		fields["action"] = res.Result.Action
		fields["endpoint_id"] = res.Result.EndpointID
		log.WithContext(ctx).WithFields(fields).Info("StripeWebhookReconcile: merchant reconciled")
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("stripe webhook reconcile: rows: %w", err)
	}
	log.WithContext(ctx).WithFields(log.Fields{
		"reconciled": count,
		"skipped":    skipped,
		"failed":     failed,
	}).Info("StripeWebhookReconcile: completed")
	return nil
}
