package riverjobs

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/riverqueue/river"
	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/destructive"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/internal/modules/catalog"
	"github.com/open-rails/openrails/internal/modules/webhooks"
	"github.com/open-rails/openrails/pkg/merchant"
)

const KindStripeWebhookReconcile = "openrails.stripe_webhook_reconcile"

// FindingStripeWebhookEndpoint is the operator-facing record of a managed
// Stripe webhook endpoint that needs a human: a rollover in flight with
// superseded endpoints still live, or an exhausted endpoint budget.
const FindingStripeWebhookEndpoint = "consistency.stripe_webhook_endpoint"

type StripeWebhookReconcileArgs struct{}

func (StripeWebhookReconcileArgs) Kind() string { return KindStripeWebhookReconcile }

type StripeWebhookReconcileWorker struct {
	river.WorkerDefaults[StripeWebhookReconcileArgs]
	DB        *db.DB
	Config    *config.Config
	Merchants *merchants.Service
	// Now overrides the rollover clock (tests).
	Now func() time.Time
	// RetireOverlap overrides catalog.WebhookRolloverOverlap (tests).
	RetireOverlap time.Duration
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

	type target struct {
		id                           merchant.ID
		slug, environment, accountID string
	}
	var targets []target
	for rows.Next() {
		var idStr, slug, environment, accountID string
		if err := rows.Scan(&idStr, &slug, &environment, &accountID); err != nil {
			return fmt.Errorf("stripe webhook reconcile: scan merchant: %w", err)
		}
		id, err := merchant.ParseID(idStr)
		if err != nil {
			return fmt.Errorf("stripe webhook reconcile: parse merchant id %q: %w", idStr, err)
		}
		targets = append(targets, target{id: id, slug: slug, environment: environment, accountID: accountID})
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("stripe webhook reconcile: rows: %w", err)
	}
	rows.Close()

	// #836/#856: the operator kill switch. Registering and patching an endpoint
	// is ADDITIVE and must run regardless — the whole point of this worker is
	// that Stripe can always reach us. What the gate governs is the one
	// destructive act left: deleting an endpoint a successor has already
	// replaced. Fail-closed default means retirement waits for a human.
	gate := destructive.New(w.DB)
	now := time.Now
	if w.Now != nil {
		now = w.Now
	}

	count, skipped, failed, retired, gated, needsOperator := 0, 0, 0, 0, 0, 0
	for _, t := range targets {
		verdict := gate.CheckMerchant(ctx, t.id.UUID())
		fields := log.Fields{"merchant": t.slug, "stripe_account_id": t.accountID}
		res, err := catalog.ReconcileManagedStripeWebhook(ctx, catalog.ManagedStripeWebhookParams{
			Config:              w.Config,
			SecretStore:         w.Merchants.Secrets(),
			MerchantID:          t.id,
			MerchantSlug:        t.slug,
			ProviderEnvironment: t.environment,
			PspID:               t.accountID,
			EnabledEvents:       webhooks.HandledStripeEventTypes,
			Now:                 now(),
			AllowRetire:         verdict.Allowed,
			RetireOverlap:       w.RetireOverlap,
		})
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
		if !verdict.Allowed && len(res.RetirePending) > 0 {
			gated++
			fields["gated_reason"] = verdict.Reason
		}
		if res.RepairedFrom != "" {
			// A local record was repaired instead of a remote endpoint replaced.
			fields["repaired_from"] = res.RepairedFrom
		}
		retired += len(res.Retired)
		fields["action"] = res.Result.Action
		fields["endpoint_id"] = res.Result.EndpointID
		if len(res.Retired) > 0 {
			fields["retired"] = res.Retired
		}
		log.WithContext(ctx).WithFields(fields).Info("StripeWebhookReconcile: merchant reconciled")

		if res.OperatorAction != "" {
			needsOperator++
			log.WithContext(ctx).WithFields(fields).Warn("StripeWebhookReconcile: " + res.OperatorAction)
		}
		w.recordEndpointFinding(ctx, t.id, t.accountID, res, verdict)
	}
	log.WithContext(ctx).WithFields(log.Fields{
		"reconciled":     count,
		"skipped":        skipped,
		"failed":         failed,
		"retired":        retired,
		"gated":          gated,
		"needs_operator": needsOperator,
	}).Info("StripeWebhookReconcile: completed")
	return nil
}

// recordEndpointFinding opens (or closes) the operator finding for one PSP's
// managed endpoint. Best-effort: the endpoint is already correct in Stripe, and
// failing to file the paperwork must never fail the pass.
func (w StripeWebhookReconcileWorker) recordEndpointFinding(
	ctx context.Context, mid merchant.ID, accountID string,
	res catalog.ManagedStripeWebhookResult, verdict destructive.Verdict,
) {
	subject := "stripe:" + accountID
	err := w.DB.RunInMerchantConn(merchant.WithID(ctx, mid), func(sctx context.Context) error {
		q := w.DB.Gen(sctx)
		if res.OperatorAction == "" {
			return resolveStripeWebhookFinding(sctx, w.DB, mid, subject)
		}
		evidence, _ := json.Marshal(map[string]any{
			"provider":       "stripe",
			"account_id":     accountID,
			"webhook_url":    res.WebhookURL,
			"action":         string(res.Result.Action),
			"endpoint_id":    res.Result.EndpointID,
			"superseded":     res.RetirePending,
			"retired":        res.Retired,
			"retire_allowed": verdict.Allowed,
			"gate_reason":    verdict.Reason,
		})
		action := res.OperatorAction
		_, uerr := q.UpsertReconciliationFinding(sctx, gen.UpsertReconciliationFindingParams{
			MerchantID:        mid.UUID(),
			FindingType:       FindingStripeWebhookEndpoint,
			SubjectKey:        subject,
			Severity:          "high",
			Status:            "requires_review",
			RecommendedAction: &action,
			Evidence:          evidence,
		})
		return uerr
	})
	if err != nil {
		log.WithContext(ctx).WithError(err).WithFields(log.Fields{
			"merchant_id": mid.String(), "stripe_account_id": accountID,
		}).Warn("StripeWebhookReconcile: could not persist the endpoint finding")
	}
}

// resolveStripeWebhookFinding closes a standing finding once the rollover has
// fully drained. Written as raw SQL on the merchant-scoped connection because
// the generated resolve statements key on a finding id, not on the stable
// (merchant, type, subject) identity this worker knows.
func resolveStripeWebhookFinding(ctx context.Context, database *db.DB, mid merchant.ID, subject string) error {
	_, err := database.Qx(ctx).Exec(ctx, `
		UPDATE openrails.reconciliation_findings
		   SET status = 'fixed', resolution = 'auto_vanished', resolved_at = now(),
		       notified_at = NULL, notified_severity = NULL, updated_at = now()
		 WHERE merchant_id = $1::uuid
		   AND finding_type = $2
		   AND subject_key = $3
		   AND status IN ('reconcile_required', 'requires_review')
	`, mid.String(), FindingStripeWebhookEndpoint, subject)
	return err
}
