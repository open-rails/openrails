package disputes

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/disputejob"
	"github.com/open-rails/openrails/internal/disputerules"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/reconcile"
)

const (
	// FindingThreshold is a PSP's ratio at a program's warning or excessive
	// level in a month.
	FindingThreshold reconcile.FindingType = disputejob.FindingThreshold
	// FindingCustomerReview asks staff to review a customer a chargeback was
	// filed against: only the disputed card is blocked.
	FindingCustomerReview reconcile.FindingType = "life.disputes.customer_review"
	// FindingUnknownCharge is a dispute or fraud report on a charge OpenRails
	// has no record of.
	FindingUnknownCharge reconcile.FindingType = "life.disputes.unknown_charge"
)

// Evaluate settles the findings of psp's programs in the current month, and
// once a month has ended its open findings on the whole of it, then alerts
// on the customer reviews waiting.
func (s *Service) Evaluate(ctx context.Context, psp uuid.UUID) error {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return err
	}
	now := s.now()
	current := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	months := []time.Time{current}
	open, err := s.DB.Gen(ctx).ListOpenThresholdFindings(ctx, gen.ListOpenThresholdFindingsParams{MerchantID: mid.UUID(), PspID: psp})
	if err != nil {
		return fmt.Errorf("list open dispute thresholds: %w", err)
	}
	for _, subject := range open {
		parts := strings.Split(subject, ":")
		month, err := time.Parse("2006-01", parts[len(parts)-1])
		if err == nil && month.Before(current) && !slices.Contains(months, month) {
			months = append(months, month)
		}
	}
	store := &reconcile.PGStore{DB: s.DB}
	for _, month := range months {
		standings, err := s.standings(ctx, mid, &psp, month)
		if err != nil {
			return err
		}
		for _, st := range standings {
			subject := thresholdSubject(psp, st.Program, st.Month)
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

func thresholdSubject(psp uuid.UUID, program billing.DisputeProgram, month time.Time) string {
	return strings.Join([]string{billing.PSPID(psp).String(), string(program), month.Format("2006-01")}, ":")
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
	action := fmt.Sprintf("%s is at %.2f%% in %s, projected over the month, at the %s level (%.2f%%). Find the products, prices and reasons drawing disputes; refund unrecognized charges before they become disputes; check the statement descriptor and the cancellation path.",
		st.Program, st.ProjectedRatio*100, st.Month.Format("January 2006"), st.Level, reached.Ratio*100)
	if !st.FraudReportsVisible && st.Program == billing.ProgramVisaVAMP {
		action += " This rail passes on no fraud reports, so the real ratio is higher."
	}
	psp := st.PSPID.UUID()
	return reconcile.RaisedFinding{Type: FindingThreshold, SubjectKey: subject, PSPID: &psp, Severity: severity, RecommendedAction: action,
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

// standings evaluates the month containing at as it stands now: projected
// while it runs, whole once it has ended.
func (s *Service) standings(ctx context.Context, mid billing.MerchantID, only *uuid.UUID, at time.Time) ([]billing.DisputeStanding, error) {
	q := s.DB.Gen(ctx)
	month := time.Date(at.Year(), at.Month(), 1, 0, 0, 0, 0, time.UTC)
	evaluatedAt := s.now()
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
