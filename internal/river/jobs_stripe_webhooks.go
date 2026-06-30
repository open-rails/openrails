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
	Rails     config.ProviderAccountSet
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
		res, err := catalog.ReconcileManagedStripeWebhook(ctx, catalog.ManagedStripeWebhookParams{
			Config:        w.Config,
			Rails:         w.Rails,
			EnabledEvents: webhooks.HandledStripeEventTypes,
		})
		if err != nil {
			log.WithContext(ctx).WithError(err).Warn("StripeWebhookReconcile: config-rail reconcile failed")
			return nil
		}
		if res.Skipped {
			log.WithContext(ctx).WithField("reason", res.SkipReason).Info("StripeWebhookReconcile: config-rail reconcile skipped")
			return nil
		}
		log.WithContext(ctx).WithFields(log.Fields{
			"action":      res.Result.Action,
			"endpoint_id": res.Result.EndpointID,
		}).Info("StripeWebhookReconcile: config-rail reconciled")
		return nil
	}

	// #641: register a managed endpoint for EVERY enabled Stripe account, not just
	// the primary — secondary (failover) and legacy (old subs/refunds) accounts
	// also receive events, each at its own per-account URL.
	rows, err := w.DB.Qx(ctx).Query(ctx, `
		SELECT m.id::text, m.slug, pa.environment, pa.account_id
		  FROM openrails.merchants m
		  JOIN openrails.provider_accounts pa ON pa.merchant_id = m.id
		 WHERE m.deleted_at IS NULL
		   AND pa.provider_type = 'stripe'
		   AND pa.status = 'enabled'
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
			Rails:               w.Rails,
			SecretStore:         w.Merchants.Secrets(),
			MerchantID:          id,
			MerchantSlug:        slug,
			ProviderEnvironment: environment,
			ProviderAccountID:   accountID,
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
