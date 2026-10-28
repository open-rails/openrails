package riverjobs

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jonboulle/clockwork"

	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"
	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/destructive"
	"github.com/open-rails/openrails/internal/integrations/stripeapi"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/internal/modules/catalog"
	"github.com/open-rails/openrails/internal/modules/webhooks"
)

const KindStripeWebhookReconcile = "openrails.stripe_webhook_reconcile"

// StripeWebhookReconcileInterval is the backstop's cadence: Stripe disables
// or deletes an endpoint without telling OpenRails.
const StripeWebhookReconcileInterval = 24 * time.Hour

// stripeWebhookMerchantBatch bounds each page of the backstop. The work queue
// is the armed-PSP set, so it scales with merchants ON STRIPE, not the
// directory.
const stripeWebhookMerchantBatch = 1000

// FindingStripeWebhookEndpoint is the operator-facing record of a managed
// Stripe webhook endpoint that needs a human: a rollover in flight with
// superseded endpoints still live, or an exhausted endpoint budget.
const FindingStripeWebhookEndpoint = "consistency.stripe_webhook_endpoint"

// StripeWebhookReconcileArgs is one Stripe PSP's endpoint pass, or with
// neither set the backstop that queues every armed PSP's.
type StripeWebhookReconcileArgs struct {
	MerchantID uuid.UUID `json:"merchant_id,omitempty"`
	AccountID  string    `json:"account_id,omitempty"`
}

func (StripeWebhookReconcileArgs) Kind() string { return KindStripeWebhookReconcile }

// InsertOpts keeps one pass of a PSP at a time: a pass queued while one waits
// coalesces with it. A failed one retries a few times; the daily backstop
// covers it after that.
func (a StripeWebhookReconcileArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{Queue: QueueBilling, MaxAttempts: 5, UniqueOpts: river.UniqueOpts{ByArgs: true, ByState: []rivertype.JobState{
		rivertype.JobStateAvailable, rivertype.JobStatePending, rivertype.JobStateRunning, rivertype.JobStateScheduled, rivertype.JobStateRetryable,
	}}}
}

// QueueStripeWebhook queues the PSP's endpoint pass now, in tx. Its pass
// waiting for a superseded endpoint's retirement is brought forward; it waits
// again after.
func QueueStripeWebhook(ctx context.Context, d *db.DB, tx pgx.Tx, mid billing.MerchantID, accountID string) error {
	args := StripeWebhookReconcileArgs{MerchantID: mid.UUID(), AccountID: accountID}
	opts := args.InsertOpts()
	return d.QueueRiverJobNowTx(ctx, tx, args, &opts)
}

// StripeWebhookDigest is what a PSP's managed endpoint is settled at: the
// pinned API version, the handled events and its URL. A boot whose digest
// differs from the one a PSP was settled at reconciles it.
func StripeWebhookDigest(cfg *config.Config, accountID string) string {
	url, _, _ := catalog.PublicStripeWebhookURL(cfg, accountID)
	events := slices.Sorted(slices.Values(webhooks.HandledStripeEventTypes))
	sum := sha256.Sum256([]byte(stripeapi.APIVersion + "\n" + strings.Join(events, ",") + "\n" + url))
	return hex.EncodeToString(sum[:])
}

type StripeWebhookReconcileWorker struct {
	StripeClients *stripeapi.Factory
	WorkerDefaults[StripeWebhookReconcileArgs]
	DB        *db.DB
	Config    *config.Config
	Merchants *merchants.Service
	Clock     clockwork.Clock
}

func (StripeWebhookReconcileWorker) Kind() string { return KindStripeWebhookReconcile }

func (w StripeWebhookReconcileWorker) Work(ctx context.Context, job *river.Job[StripeWebhookReconcileArgs]) error {
	if w.DB == nil {
		return fmt.Errorf("stripe webhook reconcile: db not configured")
	}
	if w.Config == nil {
		return fmt.Errorf("stripe webhook reconcile: config not configured")
	}
	if w.Merchants == nil || w.Merchants.Secrets() == nil {
		// Managed webhook registration resolves only from the armed rail state
		// (psps + secret store): without it there is nothing to reconcile.
		log.WithContext(ctx).Info("StripeWebhookReconcile: merchants service not armed; skipping")
		return nil
	}
	if job.Args.MerchantID == uuid.Nil {
		return w.backstop(ctx)
	}
	if job.Args.AccountID == "" {
		return river.JobCancel(errors.New("stripe webhook reconcile: a merchant's pass names its Stripe account"))
	}
	return w.reconcile(ctx, billing.MerchantID(job.Args.MerchantID), job.Args.AccountID)
}

// backstop queues the endpoint pass of each active Stripe PSP of each armed
// merchant.
func (w StripeWebhookReconcileWorker) backstop(ctx context.Context) error {
	var after *uuid.UUID
	var errs error
	for {
		merchantIDs, err := w.DB.GenDirectory().ListRailArmedMerchants(ctx, gen.ListRailArmedMerchantsParams{
			Rails: []string{string(models.RailStripe)}, MerchantLimit: stripeWebhookMerchantBatch, AfterMerchantID: after,
		})
		if err != nil {
			return errors.Join(errs, fmt.Errorf("stripe webhook backstop: list armed merchants: %w", err))
		}
		for _, id := range merchantIDs {
			after = &id
			mid := billing.MerchantID(id)
			psps, err := w.Merchants.ActivePSPScopesForRail(merchant.WithID(ctx, mid), mid, string(models.RailStripe))
			if err == nil && len(psps) > 0 {
				err = w.DB.MerchantTx(merchant.WithID(ctx, mid), func(ctx context.Context, tx pgx.Tx) error {
					for _, psp := range psps {
						if err := QueueStripeWebhook(ctx, w.DB, tx, mid, psp.AccountID); err != nil {
							return err
						}
					}
					return nil
				})
			}
			if err != nil {
				errs = errors.Join(errs, fmt.Errorf("stripe webhook backstop: merchant %s: %w", mid, err))
			}
		}
		if len(merchantIDs) < stripeWebhookMerchantBatch {
			return errs
		}
	}
}

// reconcile registers, patches or rolls over one PSP's managed endpoint and
// retires the superseded ones whose overlap ended. Settled at the current
// configuration, it records its digest; with a retirement still ahead, the
// pass waits for it.
func (w StripeWebhookReconcileWorker) reconcile(ctx context.Context, mid billing.MerchantID, accountID string) error {
	// The operator kill switch governs the one destructive act: deleting an
	// endpoint a successor replaced. Registering and patching is additive and
	// always runs, so Stripe can always reach us.
	gate := destructive.New(w.DB)
	now := w.Clock.Now().UTC()
	var wait time.Duration
	err := w.DB.RunInMerchantScope(ctx, mid, "stripe webhook reconcile", func(ctx context.Context) error {
		psps, err := w.Merchants.ActivePSPScopesForRail(ctx, mid, string(models.RailStripe))
		if err != nil {
			return fmt.Errorf("list stripe psps: %w", err)
		}
		i := slices.IndexFunc(psps, func(p merchants.PSPScope) bool { return p.AccountID == accountID })
		fields := log.Fields{"merchant_id": mid.String(), "stripe_account_id": accountID}
		if i < 0 {
			// Archived or no longer declared: its endpoint drains untouched.
			log.WithContext(ctx).WithFields(fields).Info("StripeWebhookReconcile: no active PSP for the account; skipped")
			return nil
		}
		psp := psps[i]
		verdict := gate.CheckMerchant(ctx, mid.UUID())
		res, err := catalog.ReconcileManagedStripeWebhook(ctx, catalog.ManagedStripeWebhookParams{
			Publication:         w.Merchants.StripeWebhookPublication(mid, psp.AccountID),
			StripeClients:       w.StripeClients,
			Config:              w.Config,
			SecretStore:         w.Merchants.Secrets(),
			MerchantID:          mid,
			ProviderEnvironment: psp.Environment,
			PspID:               psp.AccountID,
			EnabledEvents:       webhooks.HandledStripeEventTypes,
			Now:                 now,
			AllowRetire:         verdict.Allowed,
		})
		if err != nil {
			return err
		}
		if res.Skipped {
			fields["reason"] = res.SkipReason
			log.WithContext(ctx).WithFields(fields).Info("StripeWebhookReconcile: skipped")
			return nil
		}
		fields["action"], fields["endpoint_id"] = res.Result.Action, res.Result.EndpointID
		if len(res.Retired) > 0 {
			fields["retired"] = res.Retired
		}
		if !verdict.Allowed && len(res.RetirePending) > 0 {
			fields["gated_reason"] = verdict.Reason
		}
		log.WithContext(ctx).WithFields(fields).Info("StripeWebhookReconcile: reconciled")
		if res.OperatorAction != "" {
			log.WithContext(ctx).WithFields(fields).Warn("StripeWebhookReconcile: " + res.OperatorAction)
		}
		w.recordEndpointFinding(ctx, mid, psp.AccountID, res, verdict)
		if res.OperatorAction == "" {
			if err := w.DB.Gen(ctx).SetPSPWebhookDigest(ctx, gen.SetPSPWebhookDigestParams{MerchantID: mid.UUID(), ID: psp.ID, Digest: StripeWebhookDigest(w.Config, psp.AccountID)}); err != nil {
				return fmt.Errorf("record webhook digest: %w", err)
			}
		}
		if verdict.Allowed {
			for _, pending := range res.RetirePending {
				if d := pending.RetireAfter.Sub(now); d > 0 && (wait == 0 || d < wait) {
					wait = d
				}
			}
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("stripe webhook merchant %s account %s: %w", mid, accountID, err)
	}
	if wait > 0 {
		// The superseded endpoint's retirement is this pass's next run.
		return river.JobSnooze(wait)
	}
	return nil
}

// recordEndpointFinding opens (or closes) the operator finding for one PSP's
// managed endpoint. Best-effort: the endpoint is already correct in Stripe, and
// failing to file the paperwork must never fail the pass.
func (w StripeWebhookReconcileWorker) recordEndpointFinding(
	ctx context.Context, mid billing.MerchantID, accountID string,
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
// fully drained.
func resolveStripeWebhookFinding(ctx context.Context, database *db.DB, mid billing.MerchantID, subject string) error {
	return database.Gen(ctx).AutoResolveFindingBySubject(ctx, gen.AutoResolveFindingBySubjectParams{
		MerchantID: mid.UUID(), FindingType: FindingStripeWebhookEndpoint, SubjectKey: subject,
	})
}
