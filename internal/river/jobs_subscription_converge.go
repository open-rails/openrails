package riverjobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/open-rails/openrails/internal/integrations/stripeapi"
	"github.com/open-rails/openrails/internal/railresolve"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jonboulle/clockwork"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"
	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/modules/attempts"
	"github.com/open-rails/openrails/internal/modules/catalog"
	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/open-rails/openrails/internal/modules/payments"
	"github.com/open-rails/openrails/internal/modules/payments/rails"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/internal/modules/webhooks"
	"github.com/open-rails/openrails/internal/reconcile/converge"
)

// Webhooks are wake-up signals. This worker is the coalesced dirty-flag
// fetch: unique per (merchant, PSP, rail, subscription reference) with a
// short debounce, so a burst of events about one subscription collapses to
// one provider fetch, converged through the decider. Provider outages leave
// the job retrying, access intact.

const (
	KindSubscriptionConverge = "openrails.subscription_converge"

	// SubscriptionConvergeDebounce delays the fetch so a burst of events about
	// one subscription (dunning storms, renewal event fans) coalesces into one
	// provider read via the unique-job dedup below.
	SubscriptionConvergeDebounce = 5 * time.Second

	// A pending signup whose charge has not appeared yet is read again after
	// a minute, the wait doubling to subscriptionConvergeSnoozeMax, until the
	// row leaves `pending`. Only the converge job can activate it: no pull
	// does. Once the row is converge.PendingStaleAfter old, the stale-pending
	// pass owns it.
	subscriptionConvergeSnoozeFor     = time.Minute
	subscriptionConvergeSnoozeMax     = 30 * time.Minute
	subscriptionConvergeMissingClient = "converge: rail client not configured"
)

// SubscriptionConvergeArgs identifies one dirty subscription. EventType and
// EventCreated are forensics outside uniqueness, so a burst about one
// subscription dedupes to one job.
type SubscriptionConvergeArgs struct {
	PSPID                 uuid.UUID `json:"psp_id" river:"unique"`
	MerchantID            uuid.UUID `json:"merchant_id" river:"unique"`
	Rail                  string    `json:"rail" river:"unique"`
	SubscriptionReference string    `json:"subscription_reference" river:"unique"`
	// After is the converge job already running when this event arrived: it
	// may have fetched before the event, so this job re-fetches once it ends.
	After        int64  `json:"after,omitempty" river:"unique"`
	EventType    string `json:"event_type,omitempty"`
	EventCreated int64  `json:"event_created,omitempty"`
}

func (SubscriptionConvergeArgs) Kind() string { return KindSubscriptionConverge }

// subscriptionConvergeUniqueStates is the default unique set minus completed,
// so a finished converge never blocks the next wake-up. River requires
// running in the set; an event that lands on a running job queues a follow-up
// (SubscriptionConvergeArgs.After) instead of being dropped.
var subscriptionConvergeUniqueStates = []rivertype.JobState{
	rivertype.JobStateAvailable,
	rivertype.JobStatePending,
	rivertype.JobStateRetryable,
	rivertype.JobStateRunning,
	rivertype.JobStateScheduled,
}

// SubscriptionConvergeEnqueuer implements webhooks.SubscriptionConvergeEnqueuer
// on the enqueue-only River client.
type SubscriptionConvergeEnqueuer struct {
	Client *river.Client[pgx.Tx]
	// Debounce overrides SubscriptionConvergeDebounce (tests); zero = default.
	Debounce time.Duration
}

func NewSubscriptionConvergeEnqueuer(client *river.Client[pgx.Tx]) *SubscriptionConvergeEnqueuer {
	return &SubscriptionConvergeEnqueuer{Client: client}
}

func (e *SubscriptionConvergeEnqueuer) EnqueueSubscriptionConverge(ctx context.Context, req webhooks.ConvergeRequest) error {
	if e == nil || e.Client == nil {
		return fmt.Errorf("subscription converge enqueuer: river client unavailable")
	}
	if req.PSPID == uuid.Nil {
		return db.ErrNoPSPInContext
	}
	debounce := e.Debounce
	if debounce <= 0 {
		debounce = SubscriptionConvergeDebounce
	}
	args := SubscriptionConvergeArgs{
		MerchantID:            req.MerchantID,
		PSPID:                 req.PSPID,
		Rail:                  strings.ToLower(strings.TrimSpace(req.Rail)),
		SubscriptionReference: strings.TrimSpace(req.SubscriptionReference),
		EventType:             req.EventType,
		EventCreated:          req.EventCreated,
	}
	opts := &river.InsertOpts{
		Queue:       QueueBilling,
		ScheduledAt: time.Now().Add(debounce),
		UniqueOpts:  river.UniqueOpts{ByArgs: true, ByState: subscriptionConvergeUniqueStates},
	}
	// A job that is not running yet will fetch after this event; one snoozed
	// past the debounce (a pending signup's backoff) is woken now. A running
	// one may have fetched already, so chain a follow-up behind it.
	for range subscriptionConvergeMaxChain {
		res, err := e.Client.Insert(ctx, args, opts)
		if err != nil {
			return err
		}
		if !res.UniqueSkippedAsDuplicate || res.Job == nil {
			return nil
		}
		if res.Job.State == rivertype.JobStateScheduled && res.Job.ScheduledAt.After(opts.ScheduledAt) {
			_, err := e.Client.JobRetry(ctx, res.Job.ID)
			return err
		}
		if res.Job.State != rivertype.JobStateRunning {
			return nil
		}
		args.After = res.Job.ID
	}
	return fmt.Errorf("subscription converge: follow-up chain for %s exceeds %d running jobs", args.SubscriptionReference, subscriptionConvergeMaxChain)
}

// subscriptionConvergeMaxChain bounds the follow-up chain. A follow-up waits
// for its predecessor, so at most two jobs per subscription run at once.
const subscriptionConvergeMaxChain = 4

// SubscriptionConvergeWorker fetches provider truth for one subscription and
// converges the local row through the decider plus the rail-specific
// signup/mirror legs (webhooks.StripeConvergeService / NMIConvergeService).
type SubscriptionConvergeWorker struct {
	StripeClients *stripeapi.Factory
	river.WorkerDefaults[SubscriptionConvergeArgs]
	DB     *db.DB
	Config *config.Config
	Rails  railresolve.Source
	Clock  clockwork.Clock

	// NMIResolver arms the args merchant's NMI client from the armed rail state.
	NMIResolver money.NMIClientResolver
	// StripeProber overrides the config-built prober (tests). Nil = build from Rails.
	StripeProber subscriptions.StripeLivenessProber

	PriceService                 *catalog.PriceService
	ProductService               *catalog.ProductService
	SubscriptionService          *subscriptions.SubscriptionService
	SubscriptionLifecycleService *subscriptions.SubscriptionLifecycleService
	PaymentService               *payments.PaymentService
	MoneyService                 *money.MoneyService
	RailCustomerService          *payments.RailCustomerService
	OrderAttemptService          webhooks.OrderAttemptStore
}

func (SubscriptionConvergeWorker) Kind() string { return KindSubscriptionConverge }

func (w *SubscriptionConvergeWorker) Work(ctx context.Context, job *river.Job[SubscriptionConvergeArgs]) error {
	args := job.Args
	if w.DB == nil {
		return fmt.Errorf("subscription converge: db not configured")
	}
	if args.PSPID == uuid.Nil || args.MerchantID == uuid.Nil || strings.TrimSpace(args.Rail) == "" || strings.TrimSpace(args.SubscriptionReference) == "" {
		log.WithContext(ctx).WithFields(log.Fields{
			"merchant_id": args.MerchantID, "rail": args.Rail, "reference": args.SubscriptionReference,
		}).Warn("subscription converge: incomplete identity; dropping job")
		return nil
	}

	if args.After != 0 && w.predecessorRunning(ctx, args.After) {
		// Converge in order: the older fetch must not commit after this one.
		return river.JobSnooze(SubscriptionConvergeDebounce)
	}

	mctx := db.WithPSPID(merchant.WithID(ctx, billing.MerchantID(args.MerchantID)), args.PSPID)
	var customerID uuid.UUID
	err := w.DB.RunInMerchantConn(mctx, func(cctx context.Context) error {
		var cerr error
		customerID, cerr = w.convergeOne(cctx, args)
		if cerr != nil {
			return cerr
		}
		// Project the entitlement windows and grant effects the transition
		// implies. Best-effort: the sweep backstops.
		if customerID != uuid.Nil {
			if _, aerr := converge.AfterMutation(cctx, w.DB, billing.MerchantID(args.MerchantID), customerID, w.Clock); aerr != nil {
				log.WithContext(cctx).WithError(aerr).WithFields(log.Fields{
					"merchant_id": args.MerchantID, "customer_id": customerID,
				}).Warn("subscription converge: inline converge after transition failed; the sweep will reconcile")
			}
		}
		return nil
	})
	if err == nil {
		return nil
	}
	var pending *webhooks.PendingError
	if errors.As(err, &pending) && !workerNow(w.Clock).Before(pending.Since.Add(converge.PendingStaleAfter)) {
		log.WithContext(ctx).WithFields(log.Fields{"rail": args.Rail, "reference": args.SubscriptionReference, "pending_since": pending.Since.UTC().Format(time.RFC3339)}).
			Warn("subscription converge: the signup stayed pending past the stale threshold; the stale-pending pass owns it")
		return nil
	}
	if errors.Is(err, webhooks.ErrConvergeRetryLater) {
		return river.JobSnooze(pendingSnooze(job))
	}
	// Retryable: provider API down / transient DB failure — the job IS the
	// dirty mark; River backoff re-fetches and converges later. Access intact.
	return err
}

// predecessorRunning reports whether the converge job this follow-up chains
// behind is still running. An unreadable predecessor is treated as finished.
func (w *SubscriptionConvergeWorker) predecessorRunning(ctx context.Context, id int64) bool {
	client, err := river.ClientFromContextSafely[pgx.Tx](ctx)
	if err != nil {
		return false
	}
	job, err := client.JobGet(ctx, id)
	return err == nil && job.State == rivertype.JobStateRunning
}

// pendingSnooze is the wait before the next read of a pending signup: a
// minute, doubling with each snooze up to subscriptionConvergeSnoozeMax.
func pendingSnooze(job *river.Job[SubscriptionConvergeArgs]) time.Duration {
	var meta struct {
		Snoozes int `json:"snoozes"`
	}
	_ = json.Unmarshal(job.Metadata, &meta)
	wait := subscriptionConvergeSnoozeFor
	for range min(max(meta.Snoozes, 0), 5) {
		wait *= 2
	}
	return min(wait, subscriptionConvergeSnoozeMax)
}

func (w *SubscriptionConvergeWorker) convergeOne(ctx context.Context, args SubscriptionConvergeArgs) (uuid.UUID, error) {
	if args.EventType != "" {
		ctx = attempts.ObservedVia(ctx, "webhook") // the provider answers it fetches were announced by a webhook
	}
	switch {
	case args.Rail == string(models.RailStripe):
		prober := w.StripeProber
		if prober == nil {
			p, err := subscriptions.NewStripeLivenessProber(ctx, w.Rails)
			if err != nil {
				return uuid.Nil, fmt.Errorf("%s (stripe): %w", subscriptionConvergeMissingClient, err)
			}
			p.HTTPClient = w.StripeClients.ReadOnlyClient(0)
			prober = p
		}
		svc := &webhooks.StripeConvergeService{
			DB:                           w.DB,
			Clock:                        w.Clock,
			Prober:                       prober,
			PriceService:                 w.PriceService,
			ProductService:               w.ProductService,
			SubscriptionService:          w.SubscriptionService,
			SubscriptionLifecycleService: w.SubscriptionLifecycleService,
			PaymentService:               w.PaymentService,
			MoneyService:                 w.MoneyService,

			RailCustomerService: w.RailCustomerService,
			OrderAttemptService: w.OrderAttemptService,
		}
		return svc.Converge(ctx, args.SubscriptionReference)

	case rails.IsNMI(models.Rail(args.Rail)):
		if w.NMIResolver == nil {
			return uuid.Nil, fmt.Errorf("%s (nmi rail %q)", subscriptionConvergeMissingClient, args.Rail)
		}
		client, ok, err := w.NMIResolver.ResolveNMIClient(ctx, args.MerchantID, &args.PSPID)
		if err != nil {
			return uuid.Nil, fmt.Errorf("%s (nmi rail %q): %w", subscriptionConvergeMissingClient, args.Rail, err)
		}
		if !ok || client == nil {
			return uuid.Nil, fmt.Errorf("%s (nmi rail %q)", subscriptionConvergeMissingClient, args.Rail)
		}
		svc := &webhooks.NMIConvergeService{
			DB:                           w.DB,
			Clock:                        w.Clock,
			Rail:                         args.Rail,
			NMIClient:                    client,
			PriceService:                 w.PriceService,
			SubscriptionService:          w.SubscriptionService,
			SubscriptionLifecycleService: w.SubscriptionLifecycleService,
			PaymentService:               w.PaymentService,
			MoneyService:                 w.MoneyService,
		}
		return svc.Converge(ctx, args.SubscriptionReference)

	default:
		log.WithContext(ctx).WithField("rail", args.Rail).
			Warn("subscription converge: rail has no fetch-and-converge implementation; dropping")
		return uuid.Nil, nil
	}
}
