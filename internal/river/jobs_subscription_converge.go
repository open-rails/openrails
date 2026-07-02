package riverjobs

import (
	"context"
	"errors"
	"fmt"
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
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/integrations/nmi"
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
// fetch: unique per (merchant, rail, subscription reference) with a short
// debounce, so a burst of events about one subscription collapses to ONE
// provider fetch, converged through the #665 decider. Provider outages leave
// the job retrying (the dirty mark parks; access intact — #664 posture).

const (
	KindSubscriptionConverge = "openrails.subscription_converge"

	// SubscriptionConvergeDebounce delays the fetch so a burst of events about
	// one subscription (dunning storms, renewal event fans) coalesces into one
	// provider read via the unique-job dedup below.
	SubscriptionConvergeDebounce = 5 * time.Second

	// subscriptionConvergeSnoozeFor / GiveUpAfter bound the settlement-lag
	// snooze loop (pending NMI signups whose charge hasn't appeared yet).
	subscriptionConvergeSnoozeFor     = time.Minute
	subscriptionConvergeGiveUpAfter   = 24 * time.Hour
	subscriptionConvergeMissingClient = "converge: rail client not configured"
)

// SubscriptionConvergeArgs identifies one dirty subscription. Only the three
// identity fields participate in uniqueness — EventType/EventCreated are
// forensics, so a burst about one subscription dedupes to one job.
type SubscriptionConvergeArgs struct {
	MerchantID            uuid.UUID `json:"merchant_id" river:"unique"`
	Rail                  string    `json:"rail" river:"unique"`
	SubscriptionReference string    `json:"subscription_reference" river:"unique"`
	EventType             string    `json:"event_type,omitempty"`
	EventCreated          int64     `json:"event_created,omitempty"`
}

func (SubscriptionConvergeArgs) Kind() string { return KindSubscriptionConverge }

// subscriptionConvergeUniqueStates is the default unique set MINUS completed:
// a finished converge must never block the next wake-up for the same
// subscription. (Dropping completed forces river's slower advisory-lock insert
// path — fine at webhook volumes.) `running` stays required, so an event
// landing mid-fetch dedupes away; the next event or the pull sweep re-converges
// (slightly-old truth is self-healing, never corrupting).
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
	debounce := e.Debounce
	if debounce <= 0 {
		debounce = SubscriptionConvergeDebounce
	}
	_, err := e.Client.Insert(ctx, SubscriptionConvergeArgs{
		MerchantID:            req.MerchantID,
		Rail:                  strings.ToLower(strings.TrimSpace(req.Rail)),
		SubscriptionReference: strings.TrimSpace(req.SubscriptionReference),
		EventType:             req.EventType,
		EventCreated:          req.EventCreated,
	}, &river.InsertOpts{
		Queue:       QueueBilling,
		ScheduledAt: time.Now().Add(debounce),
		UniqueOpts: river.UniqueOpts{
			ByArgs:  true,
			ByState: subscriptionConvergeUniqueStates,
		},
	})
	return err
}

// SubscriptionConvergeWorker fetches provider truth for one subscription and
// converges the local row through the decider (#665) plus the rail-specific
// signup/mirror legs (webhooks.StripeConvergeService / NMIConvergeService).
type SubscriptionConvergeWorker struct {
	river.WorkerDefaults[SubscriptionConvergeArgs]
	DB     *db.DB
	Config *config.Config
	Rails  config.RailMerchantAccountSet
	Clock  clockwork.Clock

	NMIClients map[string]*nmi.NMIClient
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

func (w *SubscriptionConvergeWorker) now() time.Time {
	if w.Clock != nil {
		return w.Clock.Now().UTC()
	}
	return time.Now().UTC()
}

func (w *SubscriptionConvergeWorker) Work(ctx context.Context, job *river.Job[SubscriptionConvergeArgs]) error {
	args := job.Args
	if w.DB == nil {
		return fmt.Errorf("subscription converge: db not configured")
	}
	if args.MerchantID == uuid.Nil || strings.TrimSpace(args.Rail) == "" || strings.TrimSpace(args.SubscriptionReference) == "" {
		log.WithContext(ctx).WithFields(log.Fields{
			"merchant_id": args.MerchantID, "rail": args.Rail, "reference": args.SubscriptionReference,
		}).Warn("subscription converge: incomplete identity; dropping job")
		return nil
	}

	mctx := merchant.WithID(ctx, merchant.ID(args.MerchantID))
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
			if _, aerr := converge.AfterMutation(cctx, w.DB, merchant.ID(args.MerchantID), customerID); aerr != nil {
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
		if time.Since(job.CreatedAt) > subscriptionConvergeGiveUpAfter {
			log.WithContext(ctx).WithFields(log.Fields{
				"rail": args.Rail, "reference": args.SubscriptionReference,
			}).Warn("subscription converge: provider evidence never settled; giving up (pull sweep owns it now)")
			return nil
		}
		return river.JobSnooze(subscriptionConvergeSnoozeFor)
	}
	// Retryable: provider API down / transient DB failure — the job IS the
	// dirty mark; River backoff re-fetches and converges later. Access intact.
	return err
}

func (w *SubscriptionConvergeWorker) convergeOne(ctx context.Context, args SubscriptionConvergeArgs) (uuid.UUID, error) {
	switch {
	case args.Rail == string(models.RailStripe):
		prober := w.StripeProber
		if prober == nil {
			p, err := subscriptions.NewStripeLivenessProber(w.Rails)
			if err != nil {
				return uuid.Nil, fmt.Errorf("%s (stripe): %w", subscriptionConvergeMissingClient, err)
			}
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
		client := w.NMIClients[args.Rail]
		if client == nil {
			client = w.NMIClients[string(models.RailNMI)]
		}
		if client == nil {
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
