package riverjobs

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"runtime/debug"
	"time"

	"github.com/jonboulle/clockwork"
	"github.com/riverqueue/river"

	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/logx"
	"github.com/open-rails/openrails/internal/modules/money"
)

const (
	KindBillingPass                   = "openrails.billing_pass"
	KindSubscriptionVerificationRetry = "openrails.subscription_verification_retry"
	// BillingPassInterval is the pass's cadence: what falls due waits at most
	// this long, and a renewal's access grace covers two of them.
	BillingPassInterval = time.Hour
	// CriticalOverdue is how long past its due day a row critical work left due
	// fails job progress.
	CriticalOverdue = 24 * time.Hour
)

// BillingPassArgs is one run of every due scan.
type BillingPassArgs struct{}

func (BillingPassArgs) Kind() string { return KindBillingPass }

// InsertOpts: one attempt and one pass per hour; a pass that crashed does not
// hold up the next hour's, which takes what it left due.
func (BillingPassArgs) InsertOpts() river.InsertOpts { return periodPass(BillingPassInterval) }

// Scan is one due scan of the billing pass: everything due by now.
type Scan struct {
	Kind    string
	Timeout time.Duration
	Run     func(context.Context) error
	// DueBy reports a row critical work would still select at an instant;
	// nil for a scan whose work is best effort.
	DueBy func(ctx context.Context, at time.Time) (bool, error)
}

// PassReport is what a billing pass records as its River output.
type PassReport struct {
	// Overdue are the scans holding a critical row still due CriticalOverdue
	// past its due day.
	Overdue []string `json:"overdue"`
}

// BillingPassWorker runs each scan in turn under its own budget. One scan's
// failure never stops the next; the pass fails with all of them. It then
// records which critical scans hold rows a day overdue. A pass that starts
// while another still runs, on any fleet, exits: the running one holds the
// pass's lease and takes what is due. A pass whose lease is lost stops.
type BillingPassWorker struct {
	WorkerDefaults[BillingPassArgs]
	DB *db.DB
	// Leases renews the pass's lease, on connections the scans cannot take;
	// nil is DB.
	Leases *db.DB
	Scans  []Scan
	Clock  clockwork.Clock
	Log    *slog.Logger
}

func (*BillingPassWorker) Kind() string { return KindBillingPass }

func (w *BillingPassWorker) Work(ctx context.Context, _ *river.Job[BillingPassArgs]) error {
	leases := w.Leases
	if leases == nil {
		leases = w.DB
	}
	lease, err := leases.TryPassLease(ctx, KindBillingPass, db.RunLeaseTTL)
	if err != nil {
		return fmt.Errorf("billing pass: lease: %w", err)
	}
	if lease == nil {
		w.Log.WarnContext(ctx, "billing pass: the previous pass is still running; this one exits")
		return nil
	}
	defer lease.Release()
	ctx = lease.Context()
	var errs error
	for _, s := range w.Scans {
		if ctx.Err() != nil {
			errs = errors.Join(errs, fmt.Errorf("%s: not run: %w", s.Kind, context.Cause(ctx)))
			continue
		}
		if err := runScan(ctx, s); err != nil {
			w.Log.ErrorContext(ctx, "billing pass: scan failed; the next pass takes what it left due", logx.Err(err), "scan", s.Kind)
			errs = errors.Join(errs, fmt.Errorf("%s: %w", s.Kind, err))
		}
	}
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
	defer cancel()
	report, err := w.report(rctx)
	if err != nil {
		return errors.Join(errs, fmt.Errorf("billing pass: overdue check: %w", err))
	}
	if len(report.Overdue) > 0 {
		w.Log.ErrorContext(ctx, "billing pass: critical work is a day overdue", "overdue", report.Overdue)
	}
	return errors.Join(errs, river.RecordOutput(ctx, report))
}

func runScan(ctx context.Context, s Scan) (err error) {
	ctx, cancel := context.WithTimeout(ctx, s.Timeout)
	defer cancel()
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("panic: %v\n%s", p, debug.Stack())
		}
	}()
	return s.Run(ctx)
}

// report names each critical scan with a row still due CriticalOverdue past
// its due day, a UTC day.
func (w *BillingPassWorker) report(ctx context.Context) (PassReport, error) {
	cutoff := workerNow(w.Clock).Truncate(24 * time.Hour).Add(-CriticalOverdue)
	report := PassReport{Overdue: []string{}}
	for _, s := range w.Scans {
		if s.DueBy == nil {
			continue
		}
		due, err := s.DueBy(ctx, cutoff)
		if err != nil {
			return report, fmt.Errorf("%s: %w", s.Kind, err)
		}
		if due {
			report.Overdue = append(report.Overdue, s.Kind)
		}
	}
	return report, nil
}

// BillingPassTimeout is the pass's budget: every scan's.
func BillingPassTimeout() time.Duration {
	var total time.Duration
	for _, budget := range billingScanBudgets {
		total += budget
	}
	return total
}

// billingScanBudgets are the scans' budgets.
var billingScanBudgets = map[string]time.Duration{
	KindUsagePosting:                  DefaultTimeout,
	KindDunning:                       DefaultTimeout,
	KindSubscriptionVerificationRetry: DefaultTimeout,
	KindBalanceWatch:                  DefaultTimeout,
	KindDelinquency:                   DefaultTimeout,
	KindInvoiceExpiry:                 DefaultTimeout,
	KindRevenueRecognition:            DefaultTimeout,
	money.KindUsageInvoices:           DefaultTimeout,
	KindRebillWatch:                   time.Hour,
	KindConfigRealerts:                DefaultTimeout,
	KindStoredCardRefreshDue:          time.Hour,
	KindOperationWakes:                DefaultTimeout,
	KindMRRDue:                        DefaultTimeout,
	KindStripeFees:                    DefaultTimeout,
}

// KindStripeFees is the scan that reads the processor fees of the Stripe
// charges, refunds and disputes queued in stripe_fee_reads.
const KindStripeFees = "openrails.stripe_fees"

// ScanBudget is a scan's budget inside the pass.
func ScanBudget(kind string) time.Duration { return billingScanBudgets[kind] }
