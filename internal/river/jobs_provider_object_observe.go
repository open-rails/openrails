package riverjobs

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"
	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/modules/catalog/providerobjects"
	"github.com/open-rails/openrails/internal/modules/parking"
	"github.com/open-rails/openrails/internal/modules/webhooks"
	"github.com/open-rails/openrails/internal/wakejobs"
)

const KindProviderObjectObserve = wakejobs.KindProviderObjectObserve

// ProviderObjectObserveEnqueuer implements webhooks.ProviderObjectObserveEnqueuer
// on the enqueue-only River client.
type ProviderObjectObserveEnqueuer struct {
	Client *river.Client[pgx.Tx]
}

func (e ProviderObjectObserveEnqueuer) EnqueueProviderObjectObserve(ctx context.Context, req webhooks.ObserveRequest) error {
	if req.MerchantID == uuid.Nil || req.PSPID == uuid.Nil || strings.TrimSpace(req.Ref) == "" {
		return fmt.Errorf("observe %s: incomplete identity", req.Kind)
	}
	return wakejobs.Enqueue(ctx, wakejobs.Client{River: e.Client}, wakejobs.ProviderObjectObserveArgs{
		MerchantID: req.MerchantID, PSPID: req.PSPID, ObjectKind: req.Kind, Ref: strings.TrimSpace(req.Ref), EventType: req.EventType,
	}, 0)
}

// ProviderObjectObserveWorker reads one catalog object a webhook announced,
// read-only, records what the provider says now and keeps the price's
// unmapped finding true. Provider errors retry; a lost read is caught by the
// next event or a manual reconcile.
type ProviderObjectObserveWorker struct {
	river.WorkerDefaults[wakejobs.ProviderObjectObserveArgs]
	Objects func() *providerobjects.Observer
}

func (ProviderObjectObserveWorker) Kind() string { return KindProviderObjectObserve }

func (w *ProviderObjectObserveWorker) Work(ctx context.Context, job *river.Job[wakejobs.ProviderObjectObserveArgs]) error {
	args := job.Args
	kind := providerobjects.Kind(args.ObjectKind)
	if args.MerchantID == uuid.Nil || args.PSPID == uuid.Nil || args.Ref == "" || (kind != providerobjects.Product && kind != providerobjects.Price) {
		log.WithContext(ctx).WithFields(log.Fields{"kind": args.ObjectKind, "ref": args.Ref}).Warn("provider object observe: incomplete identity; dropping job")
		return nil
	}
	if args.After != 0 && predecessorRunning(ctx, args.After) {
		return river.JobSnooze(wakejobs.Debounce)
	}
	objects := w.Objects()
	if objects == nil || objects.Merchants == nil {
		return errors.New("provider object observe: observer not wired")
	}
	mid := billing.MerchantID(args.MerchantID)
	return objects.DB.RunInMerchantConn(merchant.WithID(ctx, mid), func(ctx context.Context) error {
		psp, found, err := objects.Merchants.PSPScopeByID(ctx, mid, args.PSPID)
		if err != nil {
			return err
		}
		if !found {
			log.WithContext(ctx).WithField("psp_id", args.PSPID).Info("provider object observe: PSP no longer declared; dropping job")
			return nil
		}
		before, err := objects.Get(ctx, psp, kind, args.Ref)
		if err != nil {
			return err
		}
		obj, err := objects.ObserveByID(ctx, psp, kind, args.Ref, nil, providerobjects.ViaWebhook)
		if err != nil {
			return fmt.Errorf("observe %s %s: %w", kind, args.Ref, err)
		}
		if kind != providerobjects.Price {
			return nil
		}
		// A price an event first shows us, live and unmapped, is new since
		// onboarding: raise it. A pulled one is the import's to list.
		raise := before == nil && obj != nil && !obj.Retired
		return objects.DB.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
			_, err := parking.SyncPriceFinding(ctx, gen.New(tx), args.MerchantID, args.PSPID, args.Ref, raise)
			return err
		})
	})
}

// predecessorRunning reports whether the job a follow-up chains behind is
// still running. An unreadable predecessor is treated as finished.
func predecessorRunning(ctx context.Context, id int64) bool {
	client, err := river.ClientFromContextSafely[pgx.Tx](ctx)
	if err != nil {
		return false
	}
	job, err := client.JobGet(ctx, id)
	return err == nil && job.State == rivertype.JobStateRunning
}
