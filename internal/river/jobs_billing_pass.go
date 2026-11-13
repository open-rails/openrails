package riverjobs

import (
	"context"
	"errors"
	"fmt"
	"runtime/debug"
	"time"

	"github.com/jonboulle/clockwork"
	"github.com/riverqueue/river"
	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/internal/modules/money"
)

const (
	KindBillingPass = "openrails.billing_pass"
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
// records which critical scans hold rows a day overdue.
type BillingPassWorker struct {
	WorkerDefaults[BillingPassArgs]
	Scans []Scan
	Clock clockwork.Clock
}

func (*BillingPassWorker) Kind() string { return KindBillingPass }

func (w *BillingPassWorker) Work(ctx context.Context, _ *river.Job[BillingPassArgs]) error {
	var errs error
	for _, s := range w.Scans {
		if ctx.Err() != nil {
			errs = errors.Join(errs, fmt.Errorf("%s: not run: %w", s.Kind, ctx.Err()))
			continue
		}
		if err := runScan(ctx, s); err != nil {
			log.WithContext(ctx).WithError(err).WithField("scan", s.Kind).Error("billing pass: scan failed; the next pass takes what it left due")
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
		log.WithContext(ctx).WithField("overdue", report.Overdue).Error("billing pass: critical work is a day overdue")
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
	KindDunning:              DefaultTimeout,
	KindBalanceWatch:         DefaultTimeout,
	KindDelinquency:          DefaultTimeout,
	KindOrderExpiry:          DefaultTimeout,
	money.KindUsageOrders:    DefaultTimeout,
	KindRebillWatch:          time.Hour,
	KindNMIHistory:           time.Hour,
	KindConfigRealerts:       DefaultTimeout,
	KindStoredCardRefreshDue: time.Hour,
}

// ScanBudget is a scan's budget inside the pass.
func ScanBudget(kind string) time.Duration { return billingScanBudgets[kind] }
