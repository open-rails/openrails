package riverjobs

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jonboulle/clockwork"
	"github.com/riverqueue/river"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/integrations/nmi"
	"github.com/open-rails/openrails/internal/logx"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/internal/railresolve"
)

const (
	KindNMIHistory = "openrails.nmi_history"
	// QueueNMIHistory keeps provider reporting reads off billing workers.
	QueueNMIHistory    = "nmi_history"
	NMIHistoryInterval = time.Hour

	// A completed read is due again a day later, less the hour a job may
	// start past it; a failed read stays due for the next hourly job.
	nmiHistoryEvery = 24*time.Hour - NMIHistoryInterval
	// NMIHistoryMonths is how far a PSP's first read reaches back, this
	// month included: the attempt retention.
	NMIHistoryMonths = 25

	nmiHistoryPage = 100
	// nmiHistoryReadBudget bounds one PSP's read, so a slow account cannot
	// spend the whole job's budget and starve the rest.
	nmiHistoryReadBudget = 30 * time.Minute
)

// NMIHistoryArgs is one hourly run over the NMI PSPs whose history is due.
type NMIHistoryArgs struct{}

func (NMIHistoryArgs) Kind() string { return KindNMIHistory }

func (NMIHistoryArgs) InsertOpts() river.InsertOpts {
	opts := periodPass(NMIHistoryInterval)
	opts.Queue = QueueNMIHistory
	return opts
}

// NMIHistoryWorker keeps each NMI PSP's authorization history as monthly
// aggregates: the decline report's numbers, through the same read. It reads
// only the PSPs nmi_history_reads holds due, by its due_at index. A PSP's
// first read backfills NMIHistoryMonths; later ones re-read from the month
// before the last read's. A read replaces the months it covers in one
// transaction, so a rerun is idempotent and a failed read leaves the stored
// months as they were. The checkpoint is the PSP, not the month: the Query
// API selects by last change, so a month's count is exact only from one read
// that runs through to now. It only reads NMI. A job that starts while
// another still reads, on any fleet, exits.
type NMIHistoryWorker struct {
	WorkerDefaults[NMIHistoryArgs]
	DB *db.DB
	// Leases renews the job's lease, on connections the reads cannot take;
	// nil is DB.
	Leases      *db.DB
	Clock       clockwork.Clock
	NMIResolver railresolve.NMIClientResolver
	Log         *slog.Logger
}

func (w *NMIHistoryWorker) Work(ctx context.Context, _ *river.Job[NMIHistoryArgs]) error {
	if w.NMIResolver == nil {
		return errors.New("nmi history: nmi resolver not wired")
	}
	leases := w.Leases
	if leases == nil {
		leases = w.DB
	}
	lease, err := leases.TryPassLease(ctx, KindNMIHistory, db.RunLeaseTTL)
	if err != nil {
		return fmt.Errorf("nmi history: lease: %w", err)
	}
	if lease == nil {
		w.Log.WarnContext(ctx, "nmi history: the previous job is still reading; this one exits")
		return nil
	}
	defer lease.Release()
	ctx = lease.Context()
	now := workerNow(w.Clock)
	cursor := gen.ListDueNMIHistoryReadsParams{DueBy: now, RowLimit: nmiHistoryPage}
	var errs error
	for ctx.Err() == nil {
		due, err := w.DB.GenDirectory().ListDueNMIHistoryReads(ctx, cursor)
		if err != nil {
			return errors.Join(errs, fmt.Errorf("nmi history: list due reads: %w", err))
		}
		for _, merchantDue := range byMerchant(due) {
			mid := merchantDue[0].MerchantID
			err := w.DB.RunInMerchantScope(ctx, billing.MerchantID(mid), "nmi history", func(ctx context.Context) error {
				return w.readMerchant(ctx, mid, merchantDue, now)
			})
			if err != nil {
				errs = errors.Join(errs, fmt.Errorf("merchant %s: %w", mid, err))
			}
		}
		if len(due) < nmiHistoryPage {
			return errs
		}
		last := due[len(due)-1]
		cursor.AfterDueAt, cursor.AfterMerchantID, cursor.AfterPspID = last.DueAt, last.MerchantID, last.PspID
	}
	return errors.Join(errs, fmt.Errorf("nmi history: not finished: %w", context.Cause(ctx)))
}

// byMerchant groups a page's due reads by merchant, in the page's order.
func byMerchant(due []gen.ListDueNMIHistoryReadsRow) [][]gen.ListDueNMIHistoryReadsRow {
	var out [][]gen.ListDueNMIHistoryReadsRow
	at := map[uuid.UUID]int{}
	for _, d := range due {
		i, ok := at[d.MerchantID]
		if !ok {
			i = len(out)
			at[d.MerchantID] = i
			out = append(out, nil)
		}
		out[i] = append(out[i], d)
	}
	return out
}

// readMerchant reads the merchant's due PSPs that are live in its
// configuration and parks the rest until it declares them again.
func (w *NMIHistoryWorker) readMerchant(ctx context.Context, mid uuid.UUID, due []gen.ListDueNMIHistoryReadsRow, now time.Time) error {
	var errs error
	park := func(psp uuid.UUID) {
		if err := w.DB.Gen(ctx).ParkNMIHistoryRead(ctx, gen.ParkNMIHistoryReadParams{MerchantID: mid, PspID: psp}); err != nil {
			errs = errors.Join(errs, fmt.Errorf("psp %s: park: %w", psp, err))
		}
	}
	var current []gen.ListDueNMIHistoryReadsRow
	for _, psp := range due {
		if psp.Live {
			current = append(current, psp)
		} else {
			park(psp.PspID)
		}
	}
	if len(current) == 0 {
		return errs
	}
	configuration := merchants.Of(w.DB)
	if configuration == nil {
		return errors.Join(errs, errors.New("merchant configuration not wired"))
	}
	live, err := configuration.ActivePSPScopesForRail(ctx, billing.MerchantID(mid), string(models.RailNMI))
	if err != nil {
		return errors.Join(errs, err)
	}
	active := map[uuid.UUID]bool{}
	for _, p := range live {
		active[p.ID] = true
	}
	for _, psp := range current {
		if !active[psp.PspID] {
			park(psp.PspID)
			continue
		}
		rctx, cancel := context.WithTimeout(ctx, nmiHistoryReadBudget)
		err := w.read(rctx, mid, psp.PspID, psp.ReadAt, now)
		cancel()
		if err != nil {
			w.Log.WarnContext(ctx, "nmi history: read failed; the stored months stand and the next job tries again", logx.Err(err), "merchant_id", mid, "psp_id", psp.PspID)
			errs = errors.Join(errs, fmt.Errorf("psp %s: %w", psp.PspID, err))
		}
	}
	return errs
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
		w.Log.WarnContext(ctx, "nmi history: authorizations with no readable date are in no month", "merchant_id", mid, "psp_id", psp, "undated", history.Undated)
	}
	n := len(history.Counts)
	months, kinds, reasons, counts, outcomes, causes := make([]time.Time, n), make([]string, n), make([]string, n), make([]int64, n), make([]string, n), make([]string, n)
	for i, c := range history.Counts {
		months[i], kinds[i], reasons[i], counts[i], outcomes[i], causes[i] = c.Month, c.Kind, c.Reason, int64(c.Count), string(c.Outcome), string(c.Cause)
	}
	return w.DB.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		q := gen.New(tx)
		if err := q.DeleteNMIHistoryMonthsFrom(ctx, gen.DeleteNMIHistoryMonthsFromParams{MerchantID: mid, PspID: psp, Since: since}); err != nil {
			return err
		}
		if err := q.InsertNMIHistoryMonths(ctx, gen.InsertNMIHistoryMonthsParams{
			MerchantID: mid, PspID: psp, Months: months, Kinds: kinds, Reasons: reasons, Counts: counts, Outcomes: outcomes, DeclineCauses: causes,
		}); err != nil {
			return err
		}
		return q.RecordNMIHistoryRead(ctx, gen.RecordNMIHistoryReadParams{MerchantID: mid, PspID: psp, ReadAt: now, DueAt: now.Add(nmiHistoryEvery)})
	})
}
