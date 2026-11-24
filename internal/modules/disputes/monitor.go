package disputes

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/disputerules"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/reconcile"
)

const (
	// MonitorKind evaluates one PSP's monitoring programs after a chargeback
	// or a fraud report: event-driven, never a sweep.
	MonitorKind = "openrails.dispute_monitor"
	// FindingThreshold is a PSP's projected ratio at a program's warning or
	// excessive level in a month.
	FindingThreshold reconcile.FindingType = "life.disputes.threshold"
	// FindingCustomerReview asks staff to review a customer a chargeback was
	// filed against: only the disputed card is blocked.
	FindingCustomerReview reconcile.FindingType = "life.disputes.customer_review"
)

// MonitorArgs names one PSP. After is the job already running when the
// dispute committed: it may have read before it, so this one reads again.
type MonitorArgs struct {
	MerchantID uuid.UUID `json:"merchant_id" river:"unique"`
	PSPID      uuid.UUID `json:"psp_id" river:"unique"`
	After      int64     `json:"after,omitempty" river:"unique"`
}

func (MonitorArgs) Kind() string { return MonitorKind }

var uniqueStates = []rivertype.JobState{
	rivertype.JobStateAvailable, rivertype.JobStatePending, rivertype.JobStateRetryable,
	rivertype.JobStateRunning, rivertype.JobStateScheduled,
}

// InsertOpts coalesce a batch of disputes on one PSP into one evaluation.
func (MonitorArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{Queue: "billing", Priority: 3, UniqueOpts: river.UniqueOpts{ByArgs: true, ByState: uniqueStates}}
}

// QueueMonitor commits the PSP's evaluation with d's transaction. A handle
// outside a transaction, or a process with no River producer, queues none.
func QueueMonitor(ctx context.Context, d *db.DB, mid billing.MerchantID, psp uuid.UUID) error {
	tx := d.Tx()
	if tx == nil || mid.IsZero() || psp == uuid.Nil {
		return nil
	}
	args := MonitorArgs{MerchantID: mid.UUID(), PSPID: psp}
	opts := args.InsertOpts()
	for range 4 {
		res, err := d.InsertRiverJobTx(ctx, tx, args, &opts)
		if errors.Is(err, db.ErrNoRiverProducer) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("queue dispute monitor: %w", err)
		}
		if res == nil || !res.UniqueSkippedAsDuplicate || res.Job == nil || res.Job.State != rivertype.JobStateRunning {
			return nil
		}
		args.After = res.Job.ID
	}
	return fmt.Errorf("queue dispute monitor: jobs already running for PSP %s", psp)
}

// Evaluate settles the findings of psp's programs in the current month and
// alerts on the customer reviews waiting.
func (s *Service) Evaluate(ctx context.Context, psp uuid.UUID) error {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return err
	}
	now := s.now()
	standings, err := s.standings(ctx, mid, &psp, now)
	if err != nil {
		return err
	}
	store := &reconcile.PGStore{DB: s.DB}
	for _, st := range standings {
		subject := strings.Join([]string{billing.PSPID(psp).String(), string(st.Program), st.Month.Format("2006-01")}, ":")
		if st.Level == billing.DisputeLevelNone {
			if err := store.ResolveRaisedFinding(ctx, FindingThreshold, subject); err != nil {
				return fmt.Errorf("resolve dispute threshold %s: %w", subject, err)
			}
			continue
		}
		rec, err := store.RaiseFinding(ctx, thresholdFinding(subject, st))
		if err != nil {
			return fmt.Errorf("raise dispute threshold %s: %w", subject, err)
		}
		if err := s.notify(ctx, rec); err != nil {
			return err
		}
	}
	reviews, err := s.DB.Gen(ctx).ListUnnotifiedFindings(ctx, gen.ListUnnotifiedFindingsParams{MerchantID: mid.UUID(), FindingType: string(FindingCustomerReview)})
	if err != nil {
		return fmt.Errorf("list customer reviews: %w", err)
	}
	for _, row := range reviews {
		if err := s.notify(ctx, reconcile.FindingRecordFromRow(row)); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) notify(ctx context.Context, rec reconcile.FindingRecord) error {
	if s.Notifier == nil {
		return nil
	}
	if err := s.Notifier.NotifyFinding(ctx, rec); err != nil {
		return fmt.Errorf("alert finding %s: %w", rec.SubjectKey, err)
	}
	return nil
}

func thresholdFinding(subject string, st billing.DisputeStanding) reconcile.RaisedFinding {
	severity := reconcile.SeverityHigh
	if st.Level != billing.DisputeLevelWarning {
		severity = reconcile.SeverityCritical
	}
	var reached billing.DisputeThreshold
	for _, t := range st.Thresholds {
		if t.Level == st.Level {
			reached = t
		}
	}
	action := fmt.Sprintf("%s is projected at %.2f%% in %s, at the %s level (%.2f%%). Find the products, prices and reasons drawing disputes; refund unrecognized charges before they become disputes; check the statement descriptor and the cancellation path.",
		st.Program, st.ProjectedRatio*100, st.Month.Format("January 2006"), st.Level, reached.Ratio*100)
	if !st.FraudReportsVisible && st.Program == billing.ProgramVisaVAMP {
		action += " This rail passes on no fraud reports, so the real ratio is higher."
	}
	return reconcile.RaisedFinding{Type: FindingThreshold, SubjectKey: subject, Severity: severity, RecommendedAction: action,
		Evidence: map[string]any{
			"psp_id": st.PSPID.String(), "rail": string(st.Rail), "program": string(st.Program), "month": st.Month.Format("2006-01"),
			"count": st.Count, "denominator": st.Denominator, "ratio": st.Ratio, "projected_ratio": st.ProjectedRatio,
			"projected_count": st.ProjectedCount, "level": string(st.Level), "threshold": reached.Ratio,
			"min_count": reached.MinCount, "min_amount": reached.MinAmount, "min_prior_sales": reached.MinPriorSales,
			"source": reached.Source, "fraud_reports_visible": st.FraudReportsVisible,
		}}
}

// ListPrograms is every card PSP's standing in each program for the month
// containing at.
func (s *Service) ListPrograms(ctx context.Context, params billing.DisputeProgramListParams) ([]billing.DisputeStanding, error) {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	at := s.now()
	if params.At != nil {
		at = params.At.UTC()
	}
	var psp *uuid.UUID
	if !params.PSPID.IsZero() {
		id := params.PSPID.UUID()
		psp = &id
	}
	return s.standings(ctx, mid, psp, at)
}

// standings evaluates the month containing at: as it stands at at, or
// whole once the month has ended.
func (s *Service) standings(ctx context.Context, mid billing.MerchantID, only *uuid.UUID, at time.Time) ([]billing.DisputeStanding, error) {
	q := s.DB.Gen(ctx)
	month := time.Date(at.Year(), at.Month(), 1, 0, 0, 0, 0, time.UTC)
	evaluatedAt := at
	if now := s.now(); evaluatedAt.After(now) {
		evaluatedAt = now
	}
	psps, err := q.ListPSPsForMerchant(ctx, mid.UUID())
	if err != nil {
		return nil, fmt.Errorf("list PSPs: %w", err)
	}
	reported, err := q.ListPSPsWithDisputes(ctx, mid.UUID())
	if err != nil {
		return nil, fmt.Errorf("list PSPs with disputes: %w", err)
	}
	rows, err := q.ListNetworkActivity(ctx, gen.ListNetworkActivityParams{MerchantID: mid.UUID(), PspID: only,
		FromAt: month.AddDate(0, -2, 0), ToAt: month.AddDate(0, 1, 0)})
	if err != nil {
		return nil, fmt.Errorf("read network activity: %w", err)
	}
	active := map[uuid.UUID]bool{}
	for _, r := range rows {
		active[r.PspID] = true
	}
	var out []billing.DisputeStanding
	for _, p := range psps {
		// A superseded PSP is evaluated while its charges are still disputed.
		if (only != nil && p.ID != *only) || (p.SupersededAt != nil && !active[p.ID]) {
			continue
		}
		rail := billing.Rail(p.Rail)
		programs := append([]disputerules.Program(nil), disputerules.Programs...)
		if s.Limits != nil {
			if limit, ok := s.Limits(ctx, p.ID); ok {
				programs = append(programs, disputerules.AcquirerProgram(limit))
			}
		}
		for _, program := range programs {
			if !program.Applies(rail) {
				continue
			}
			w := disputerules.WindowFor(month, evaluatedAt, days(rows, p.ID, program.Network))
			st := program.Evaluate(w)
			standing := billing.DisputeStanding{PSPID: billing.PSPID(p.ID), Rail: rail, Program: program.ID, Month: month,
				Count: st.Count, Denominator: st.Denominator, Ratio: st.Ratio, ProjectedRatio: st.Projected, ProjectedCount: st.ProjectedCount,
				Level: st.Level, Thresholds: []billing.DisputeThreshold{}, FraudReportsVisible: rail == billing.RailStripe}
			for _, id := range reported {
				standing.DisputesReported = standing.DisputesReported || id == p.ID
			}
			for _, l := range program.InForce(month) {
				standing.Thresholds = append(standing.Thresholds, billing.DisputeThreshold{Level: l.Level, Ratio: float64(l.RatioBPS) / 10_000,
					MinCount: l.MinCount, MinAmount: l.MinAmountUSD, MinPriorSales: l.MinPriorSales, Source: l.Source})
			}
			out = append(out, standing)
		}
	}
	return out, nil
}

// days are one PSP's daily activity on network ("" every network), USD
// amounts only.
func days(rows []gen.ListNetworkActivityRow, psp uuid.UUID, network billing.CardNetwork) map[time.Time]disputerules.Activity {
	out := map[time.Time]disputerules.Activity{}
	for _, r := range rows {
		if r.PspID != psp || (network != "" && r.Network != string(network)) {
			continue
		}
		a := disputerules.Activity{Sales: r.Sales, Chargebacks: r.Chargebacks, FraudChargebacks: r.FraudChargebacks, FraudReports: r.FraudReports}
		if r.Currency == "USD" {
			a.SalesAmountUSD, a.FraudChargebackAmountUSD = r.SalesAmount, r.FraudChargebackAmount
		}
		day := r.DayAt.UTC()
		out[day] = out[day].Add(a)
	}
	return out
}
