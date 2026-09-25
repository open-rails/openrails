package riverjobs

import (
	"context"
	"errors"
	"fmt"
	"github.com/open-rails/openrails/internal/integrations/stripeapi"
	"github.com/open-rails/openrails/internal/railresolve"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jonboulle/clockwork"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"
	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/modules/catalog"
	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/open-rails/openrails/internal/modules/payments"
	"github.com/open-rails/openrails/internal/modules/payments/rails"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/internal/modules/webhooks"
	"github.com/open-rails/openrails/internal/reconcile/converge"
	"github.com/open-rails/openrails/pkg/merchant"
)

// #684: webhooks are wake-up signals. This worker is the coalesced dirty-flag
// fetch: unique per (merchant, PSP, rail, subscription reference) with a short
// debounce, so a burst of events about one subscription collapses to ONE
// provider fetch, converged through the #665 decider. Provider outages leave
// the job retrying (the dirty mark parks; access intact — #664 posture).

const (
	KindSubscriptionConverge = "openrails.subscription_converge"

	// SubscriptionConvergeDebounce delays the fetch so a burst of events about
	// one subscription (dunning storms, renewal event fans) coalesces into one
	// provider read via the unique-job dedup below.
	SubscriptionConvergeDebounce = 5 * time.Second

	// subscriptionConvergeSnoozeFor paces the settlement-lag snooze loop
	// (pending NMI signups whose charge hasn't appeared yet): one provider
	// read per minute per pending signup. The loop ENDS on observation, not
	// on a clock (xs-007 row 38): the subscription leaving `pending` (the
	// fetched charge or decline, or checkout expiry — the converge then
	// returns something other than ErrConvergeRetryLater), or the provider
	// refresh pull having covered this rail since the job was born
	// (rail_refresh_watermarks, scoped to the captured PSP) —
	// from then on the pull re-reads the same provider evidence on its own
	// cadence, and this job's snooze would only duplicate it. It used to give
	// up after 24 h whether or not anything else had looked.
	subscriptionConvergeSnoozeFor     = time.Minute
	subscriptionConvergeMissingClient = "converge: rail client not configured"
)

// SubscriptionConvergeArgs identifies one dirty subscription. Only the four
// identity fields participate in uniqueness — EventType/EventCreated are
// forensics, so a burst about one subscription dedupes to one job.
type SubscriptionConvergeArgs struct {
	PSPID                 uuid.UUID `json:"psp_id" river:"unique"`
	MerchantID            uuid.UUID `json:"merchant_id" river:"unique"`
	Rail                  string    `json:"rail" river:"unique"`
	SubscriptionReference string    `json:"subscription_reference" river:"unique"`
	// After is the converge job that was already running when this event
	// arrived: it may have fetched before the event, so this job re-fetches
	// once that one finishes (audit 19).
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
	// A job that is not running yet will fetch after this event. A running one
	// may have fetched already, so chain a follow-up behind it.
	for range subscriptionConvergeMaxChain {
		res, err := e.Client.Insert(ctx, args, opts)
		if err != nil {
			return err
		}
		if !res.UniqueSkippedAsDuplicate || res.Job == nil || res.Job.State != rivertype.JobStateRunning {
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
// converges the local row through the decider (#665) plus the rail-specific
// signup/mirror legs (webhooks.StripeConvergeService / NMIConvergeService).
type SubscriptionConvergeWorker struct {
	StripeClients *stripeapi.Factory
	river.WorkerDefaults[SubscriptionConvergeArgs]
	DB     *db.DB
	Config *config.Config
	Rails  railresolve.Source
	Clock  clockwork.Clock

	// NMIResolver arms the args merchant's NMI client from the armed rail
	// state (#788).
	NMIResolver money.NMIClientResolver
	// StripeProber overrides the config-built prober (tests). Nil = build from Rails.
	StripeProber subscriptions.StripeLivenessProber

	PriceService                 *catalog.PriceService
	ProductService               *catalog.ProductService
	SubscriptionService          *subscriptions.SubscriptionService
	SubscriptionLifecycleService *subscriptions.SubscriptionLifecycleService
	PaymentService               *payments.PaymentService
	MoneyService                 *money.MoneyService
	NotificationService          *subscriptions.NotificationService
	RailCustomerService          *payments.RailCustomerService
	CheckoutSessionService       webhooks.CheckoutSessionStore
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

	mctx := db.WithPSPID(merchant.WithID(ctx, merchant.ID(args.MerchantID)), args.PSPID)
	var customerID uuid.UUID
	err := w.DB.RunInMerchantConn(mctx, func(cctx context.Context) error {
		var cerr error
		customerID, cerr = w.convergeOne(cctx, args)
		if cerr != nil {
			return cerr
		}
		// Inline convergence pass (#511 Phase E): project entitlement windows /
		// grant effects the transition implies. Best-effort — the sweep backstops.
		if customerID != uuid.Nil {
			if _, aerr := converge.AfterMutation(cctx, w.DB, merchant.ID(args.MerchantID), customerID, w.Clock); aerr != nil {
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
	if errors.Is(err, webhooks.ErrConvergeRetryLater) {
		if covered, pulledAt := w.pullCoveredSince(mctx, args, job.CreatedAt); covered {
			log.WithContext(ctx).WithFields(log.Fields{
				"rail": args.Rail, "reference": args.SubscriptionReference,
				"job_created_at": job.CreatedAt.UTC().Format(time.RFC3339),
				"last_pull_at":   pulledAt.UTC().Format(time.RFC3339),
			}).Info("subscription converge: provider evidence not settled and the refresh pull has covered this rail since; handing off to the pull")
			return nil
		}
		return river.JobSnooze(subscriptionConvergeSnoozeFor)
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

// pullCoveredSince reports whether a provider-refresh pull for the rail has
// completed after `since` — the observed hand-off signal for the snooze loop.
// A read failure is "not covered": the job keeps snoozing, which costs one
// provider read a minute, whereas a wrong hand-off could leave a pending
// signup to a pull that never runs.
func (w *SubscriptionConvergeWorker) pullCoveredSince(mctx context.Context, args SubscriptionConvergeArgs, since time.Time) (bool, time.Time) {
	var pulledAt time.Time
	err := w.DB.RunInMerchantConn(mctx, func(cctx context.Context) error {
		var qerr error
		pulledAt, qerr = w.DB.Gen(cctx).GetPSPRefreshWatermark(cctx, gen.GetPSPRefreshWatermarkParams{
			MerchantID: args.MerchantID, Rail: args.Rail, PspID: args.PSPID,
		})
		if qerr != nil && db.IsNotFound(qerr) {
			pulledAt, qerr = time.Time{}, nil
		}
		return qerr
	})
	if err != nil {
		log.WithContext(mctx).WithError(err).WithField("rail", args.Rail).Warn("subscription converge: pull watermark read failed; keeping the snooze")
		return false, time.Time{}
	}
	if !pulledAt.After(since) {
		return false, time.Time{}
	}
	return true, pulledAt
}

func (w *SubscriptionConvergeWorker) convergeOne(ctx context.Context, args SubscriptionConvergeArgs) (uuid.UUID, error) {
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
			NotificationService:          w.NotificationService,
			RailCustomerService:          w.RailCustomerService,
			CheckoutSessionService:       w.CheckoutSessionService,
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
			NotificationService:          w.NotificationService,
		}
		return svc.Converge(ctx, args.SubscriptionReference)

	default:
		log.WithContext(ctx).WithField("rail", args.Rail).
			Warn("subscription converge: rail has no fetch-and-converge implementation; dropping")
		return uuid.Nil, nil
	}
}
