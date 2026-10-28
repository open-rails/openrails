package riverjobs

import (
	"context"
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
	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/modules/attempts"
	"github.com/open-rails/openrails/internal/modules/catalog"
	"github.com/open-rails/openrails/internal/modules/catalog/providerobjects"
	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/open-rails/openrails/internal/modules/parking"
	"github.com/open-rails/openrails/internal/modules/payments"
	"github.com/open-rails/openrails/internal/modules/payments/rails"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/internal/modules/webhooks"
	"github.com/open-rails/openrails/internal/pspimport"
	"github.com/open-rails/openrails/internal/reconcile"
	"github.com/open-rails/openrails/internal/reconcile/converge"
	"github.com/open-rails/openrails/internal/wakejobs"
)

// Webhooks are wake-up signals. This worker is the coalesced dirty-flag
// fetch: unique per (merchant, PSP, rail, subscription reference) with a
// short debounce, so a burst of events about one subscription collapses to
// one provider fetch, converged through the decider. Provider outages leave
// the job retrying, access intact.

const (
	KindSubscriptionConverge = wakejobs.KindSubscriptionConverge

	// SubscriptionConvergeDebounce delays the fetch so a burst of events about
	// one subscription (dunning storms, renewal event fans) coalesces into one
	// provider read.
	SubscriptionConvergeDebounce = wakejobs.Debounce

	subscriptionConvergeMissingClient = "converge: rail client not configured"
)

// SubscriptionConvergeArgs identifies one dirty subscription.
type SubscriptionConvergeArgs = wakejobs.SubscriptionConvergeArgs

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
	return wakejobs.Enqueue(ctx, wakejobs.Client{River: e.Client}, SubscriptionConvergeArgs{
		MerchantID:            req.MerchantID,
		PSPID:                 req.PSPID,
		Rail:                  strings.ToLower(strings.TrimSpace(req.Rail)),
		SubscriptionReference: strings.TrimSpace(req.SubscriptionReference),
		EventType:             req.EventType,
		EventCreated:          req.EventCreated,
	}, e.Debounce)
}

// SubscriptionConvergeWorker fetches provider truth for one subscription and
// converges the local row through the decider plus the rail-specific
// signup/mirror legs (webhooks.StripeConvergeService / NMIConvergeService).
type SubscriptionConvergeWorker struct {
	StripeClients *stripeapi.Factory
	WorkerDefaults[SubscriptionConvergeArgs]
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
	// Objects records the provider prices parked subscriptions bill.
	Objects func() *providerobjects.Observer
	// Importer adopts live NMI subscriptions OpenRails does not hold.
	Importer func() *pspimport.Importer
	// Findings notifies an operator of the finding a park raises.
	Findings reconcile.FindingNotifier
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

	if args.After != 0 && predecessorRunning(ctx, args.After) {
		// Converge in order: the older fetch must not commit after this one.
		return river.JobSnooze(SubscriptionConvergeDebounce)
	}

	mctx := db.WithPSPID(merchant.WithID(ctx, billing.MerchantID(args.MerchantID)), args.PSPID)
	var customerID uuid.UUID
	err := w.DB.RunInMerchantConn(mctx, func(cctx context.Context) error {
		var cerr error
		customerID, cerr = w.convergeOne(cctx, args)
		if errors.Is(cerr, parking.ErrCauseFixed) {
			// The cause was fixed while this read was in flight: read again.
			customerID, cerr = w.convergeOne(cctx, args)
		}
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
	// Retryable: provider API down / transient DB failure — the job IS the
	// dirty mark; River backoff re-fetches and converges later. Access intact.
	return err
}

func (w *SubscriptionConvergeWorker) objects() *providerobjects.Observer {
	if w.Objects == nil {
		return nil
	}
	return w.Objects()
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
			Objects:             w.objects(),
			Findings:            w.Findings,
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
			Objects:                      w.objects(),
			Findings:                     w.Findings,
		}
		if w.Importer != nil {
			if im := w.Importer(); im != nil {
				svc.Adopter = im
			}
		}
		return svc.Converge(ctx, args.SubscriptionReference)

	default:
		log.WithContext(ctx).WithField("rail", args.Rail).
			Warn("subscription converge: rail has no fetch-and-converge implementation; dropping")
		return uuid.Nil, nil
	}
}
