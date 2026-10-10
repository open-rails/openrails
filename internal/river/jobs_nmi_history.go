package riverjobs

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jonboulle/clockwork"
	"github.com/riverqueue/river"
	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/integrations/nmi"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/internal/railresolve"
	"github.com/open-rails/openrails/internal/shared/progress"
)

const (
	KindNMIHistory = "openrails.nmi_history"

	// NMIHistoryInterval is how often PSPs due a read are looked for. A PSP
	// is read once a day; a failed read is tried again at the next tick.
	NMIHistoryInterval = time.Hour
	nmiHistoryEvery    = 24*time.Hour - NMIHistoryInterval
	// NMIHistoryMonths is how far a PSP's first read reaches back, this
	// month included: the attempt retention.
	NMIHistoryMonths = 25

	nmiHistoryMerchantBatch = 200
	nmiHistoryPSPBatch      = 100
)

// NMIHistoryArgs runs one pass of the NMI history read.
type NMIHistoryArgs struct{}

func (NMIHistoryArgs) Kind() string { return KindNMIHistory }

// NMIHistoryWorker keeps each NMI PSP's authorization history as monthly
// aggregates: the decline report's numbers, through the same read. A PSP's
// first read backfills NMIHistoryMonths; later ones, daily, re-read from the
// month before the last read's. A read replaces the months it covers in one
// transaction, so a rerun is idempotent and a failed read leaves the stored
// months as they were. It only reads NMI.
type NMIHistoryWorker struct {
	river.WorkerDefaults[NMIHistoryArgs]
	DB          *db.DB
	Clock       clockwork.Clock
	NMIResolver railresolve.NMIClientResolver
}

func (NMIHistoryWorker) Kind() string { return KindNMIHistory }

func (w *NMIHistoryWorker) Work(ctx context.Context, _ *river.Job[NMIHistoryArgs]) error {
	if w.NMIResolver == nil {
		return errors.New("nmi history: nmi resolver not wired")
	}
	now := w.Clock.Now().UTC()
	var after *uuid.UUID
	var workErr error
	for {
		merchantIDs, err := w.DB.GenDirectory().ListRailArmedMerchants(ctx, gen.ListRailArmedMerchantsParams{
			Rails: []string{string(models.RailNMI)}, MerchantLimit: nmiHistoryMerchantBatch, AfterMerchantID: after,
		})
		if err != nil {
			return errors.Join(workErr, fmt.Errorf("nmi history: list merchants: %w", err))
		}
		for _, mid := range merchantIDs {
			after = &mid
			progress.Mark(ctx, "nmi history merchant "+mid.String())
			err := w.DB.RunInMerchantScope(ctx, billing.MerchantID(mid), "nmi history", func(ctx context.Context) error {
				return w.readMerchant(ctx, mid, now)
			})
			if err != nil {
				workErr = errors.Join(workErr, fmt.Errorf("merchant %s: %w", mid, err))
			}
		}
		if len(merchantIDs) < nmiHistoryMerchantBatch {
			return workErr
		}
	}
}

func (w *NMIHistoryWorker) readMerchant(ctx context.Context, mid uuid.UUID, now time.Time) error {
	configuration := merchants.Of(w.DB)
	if configuration == nil {
		return errors.New("merchant configuration not wired")
	}
	live, err := configuration.ActivePSPScopesForRail(ctx, billing.MerchantID(mid), string(models.RailNMI), configuration.Environment())
	if err != nil || len(live) == 0 {
		return err
	}
	ids := make([]uuid.UUID, len(live))
	for i, p := range live {
		ids[i] = p.ID
	}
	due, err := w.DB.Gen(ctx).ListNMIHistoryDuePSPs(ctx, gen.ListNMIHistoryDuePSPsParams{
		MerchantID: mid, PspIds: ids, DueBefore: now.Add(-nmiHistoryEvery), RowLimit: nmiHistoryPSPBatch,
	})
	if err != nil {
		return err
	}
	for _, psp := range due {
		if err := w.read(ctx, mid, psp.ID, psp.ReadAt, now); err != nil {
			log.WithContext(ctx).WithError(err).WithFields(log.Fields{"merchant_id": mid, "psp_id": psp.ID}).
				Warn("nmi history: read failed; the stored months stand and the next pass tries again")
		}
	}
	return nil
}

// read reads one PSP's history and replaces the months it covers.
func (w *NMIHistoryWorker) read(ctx context.Context, mid, psp uuid.UUID, last *time.Time, now time.Time) error {
	since := nmi.MonthStart(now).AddDate(0, 1-NMIHistoryMonths, 0)
	if last != nil {
		if from := nmi.MonthStart(*last).AddDate(0, -1, 0); from.After(since) {
			since = from
		}
	}
	client, ok, err := w.NMIResolver.ResolveNMIClient(ctx, mid, &psp)
	if err != nil || !ok || client == nil {
		return fmt.Errorf("nmi client unavailable: %v", err)
	}
	history, err := client.DeclineHistory(ctx, since, now)
	if err != nil {
		return err
	}
	if history.Undated > 0 {
		log.WithContext(ctx).WithFields(log.Fields{"merchant_id": mid, "psp_id": psp, "undated": history.Undated}).
			Warn("nmi history: authorizations with no readable date are in no month")
	}
	n := len(history.Counts)
	months, kinds, categories, reasons, counts := make([]time.Time, n), make([]string, n), make([]string, n), make([]string, n), make([]int64, n)
	for i, c := range history.Counts {
		months[i], kinds[i], categories[i], reasons[i], counts[i] = c.Month, c.Kind, c.Category, c.Reason, int64(c.Count)
	}
	return w.DB.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		q := gen.New(tx)
		if err := q.DeleteNMIHistoryMonthsFrom(ctx, gen.DeleteNMIHistoryMonthsFromParams{MerchantID: mid, PspID: psp, Since: since}); err != nil {
			return err
		}
		if err := q.InsertNMIHistoryMonths(ctx, gen.InsertNMIHistoryMonthsParams{
			MerchantID: mid, PspID: psp, Months: months, Kinds: kinds, Categories: categories, Reasons: reasons, Counts: counts,
		}); err != nil {
			return err
		}
		return q.RecordNMIHistoryRead(ctx, gen.RecordNMIHistoryReadParams{MerchantID: mid, PspID: psp, ReadAt: now})
	})
}
