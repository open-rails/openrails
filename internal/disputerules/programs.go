package disputerules

import (
	"fmt"
	"maps"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/open-rails/openrails/billing"
)

// Basis is what a program's ratio divides.
type Basis string

const (
	// SameMonth divides the month's chargebacks (and fraud reports, for VAMP)
	// by the month's settled sales, by count.
	SameMonth Basis = "same_month"
	// PreviousMonth divides the month's chargebacks by the previous month's
	// settled sales, by count.
	PreviousMonth Basis = "previous_month"
	// FraudAmount divides the month's fraud chargebacks by the previous
	// month's settled sales, by amount in USD.
	FraudAmount Basis = "fraud_amount"
)

// Level is one threshold of a program. A level is reached when the projected
// ratio is at least RatioBPS and every minimum is met.
type Level struct {
	Level billing.DisputeLevel
	// RatioBPS is the ratio in basis points (150 is 1.5%).
	RatioBPS int64
	// MinCount is the month's least count of what the ratio counts.
	MinCount int64
	// MinAmountUSD is the month's least amount, in USD native units.
	MinAmountUSD int64
	// MinPriorSales is the previous month's least settled sales.
	MinPriorSales int64
	// From and Until bound the months the level is in force; zero is open.
	From, Until time.Time
	// Source is where the level is published; OpenRails' own levels cite the
	// rule they follow.
	Source string
}

// Program is a card network's or a processor's dispute monitoring program.
type Program struct {
	ID billing.DisputeProgram
	// Network is the one network the program counts; "" counts every one.
	Network billing.CardNetwork
	// Rail limits the program to one rail's PSPs; "" applies to every card PSP.
	Rail  billing.Rail
	Basis Basis
	// FraudReports adds fraud reports to the count.
	FraudReports bool
	Levels       []Level
}

const usd = 1_000_000 // USD native units

var (
	vampStart    = time.Date(2025, time.June, 1, 0, 0, 0, 0, time.UTC)
	vampStricter = time.Date(2026, time.April, 1, 0, 0, 0, 0, time.UTC)
)

const (
	sourceVAMP          = "https://corporate.visa.com/content/dam/VCOM/corporate/visa-perspectives/security-and-trust/documents/visa-acquirer-monitoring-program-fact-sheet-2025.pdf"
	sourceVAMPFlagged   = "https://docs.stripe.com/disputes/monitoring-programs"
	sourceMastercardECP = "https://developer.paypal.com/braintree/articles/risk-and-security/card-brand-monitoring-programs/mastercard-programs/excessive-chargeback-program"
	sourceMastercardEFM = "https://merchantservices.chase.com/content/dam/jpm/merchant-services/documents/payment-network-updates/MC_ExcessiveFraud_FAQ_02092022.pdf"
	sourceStripe        = "https://docs.stripe.com/disputes/measuring"
	// sourcePolicy is OpenRails' early warning where a program has none: two
	// thirds of the excessive ratio and half its minimum.
	sourcePolicy = "openrails: two thirds of the excessive ratio, half its minimum"
)

// WarningFloor is the least count any warning needs: Visa's own fined level
// under a flagged acquirer, and below it a ratio is noise.
const WarningFloor = 5

// Programs are the monitoring programs OpenRails evaluates. VAMP uses the US,
// Canada, EU and Asia-Pacific levels. Where sources disagree the stricter
// reading is kept: a charge with both a fraud report and a dispute counts
// twice in VAMP. Mastercard's EFM is evaluated without its 3-D Secure share,
// which OpenRails cannot see.
var Programs = []Program{
	{ID: billing.ProgramVisaVAMP, Network: billing.NetworkVisa, Basis: SameMonth, FraudReports: true, Levels: []Level{
		{Level: billing.DisputeLevelWarning, RatioBPS: 50, MinCount: WarningFloor, From: vampStart, Source: sourceVAMPFlagged},
		{Level: billing.DisputeLevelExcessive, RatioBPS: 220, MinCount: 1500, From: vampStart, Until: vampStricter, Source: sourceVAMP},
		{Level: billing.DisputeLevelExcessive, RatioBPS: 150, MinCount: 1500, From: vampStricter, Source: sourceVAMP},
	}},
	{ID: billing.ProgramMastercardECP, Network: billing.NetworkMastercard, Basis: PreviousMonth, Levels: []Level{
		{Level: billing.DisputeLevelWarning, RatioBPS: 100, MinCount: 50, Source: sourcePolicy},
		{Level: billing.DisputeLevelExcessive, RatioBPS: 150, MinCount: 100, Source: sourceMastercardECP},
		{Level: billing.DisputeLevelHighExcessive, RatioBPS: 300, MinCount: 300, Source: sourceMastercardECP},
	}},
	{ID: billing.ProgramMastercardEFM, Network: billing.NetworkMastercard, Basis: FraudAmount, Levels: []Level{
		{Level: billing.DisputeLevelWarning, RatioBPS: 33, MinAmountUSD: 25_000 * usd, MinPriorSales: 1000, Source: sourcePolicy},
		{Level: billing.DisputeLevelExcessive, RatioBPS: 50, MinAmountUSD: 50_000 * usd, MinPriorSales: 1000, Source: sourceMastercardEFM},
	}},
	{ID: billing.ProgramStripe, Rail: billing.RailStripe, Basis: SameMonth, Levels: []Level{
		{Level: billing.DisputeLevelWarning, RatioBPS: 50, MinCount: WarningFloor, Source: sourcePolicy},
		{Level: billing.DisputeLevelExcessive, RatioBPS: 75, MinCount: WarningFloor, Source: sourceStripe},
	}},
}

// AcquirerProgram is the program of a PSP whose acquirer limits its dispute
// ratio: chargebacks over settled sales in the month, every network.
func AcquirerProgram(limitBPS int64) Program {
	return Program{ID: billing.ProgramAcquirer, Basis: SameMonth, Levels: []Level{
		{Level: billing.DisputeLevelWarning, RatioBPS: limitBPS * 2 / 3, MinCount: WarningFloor, Source: sourcePolicy},
		{Level: billing.DisputeLevelExcessive, RatioBPS: limitBPS, MinCount: WarningFloor, Source: "the PSP's dispute_ratio_limit"},
	}}
}

// Applies reports whether p monitors a PSP on rail.
func (p Program) Applies(rail billing.Rail) bool {
	if rail != billing.RailNMI && rail != billing.RailStripe && rail != billing.RailCCBill {
		return false
	}
	return p.Rail == "" || p.Rail == rail
}

// InForce are the program's levels in force in the month starting at month.
func (p Program) InForce(month time.Time) []Level {
	var out []Level
	for _, l := range p.Levels {
		if (l.From.IsZero() || !month.Before(l.From)) && (l.Until.IsZero() || month.Before(l.Until)) {
			out = append(out, l)
		}
	}
	return out
}

// Activity is what a window of a PSP's network activity counted.
type Activity struct {
	Sales, Chargebacks, FraudChargebacks, FraudReports int64
	// SalesAmountUSD and FraudChargebackAmountUSD are USD charges only.
	SalesAmountUSD, FraudChargebackAmountUSD int64
}

// Add is a + b.
func (a Activity) Add(b Activity) Activity {
	return Activity{Sales: a.Sales + b.Sales, Chargebacks: a.Chargebacks + b.Chargebacks, FraudChargebacks: a.FraudChargebacks + b.FraudChargebacks,
		FraudReports: a.FraudReports + b.FraudReports, SalesAmountUSD: a.SalesAmountUSD + b.SalesAmountUSD,
		FraudChargebackAmountUSD: a.FraudChargebackAmountUSD + b.FraudChargebackAmountUSD}
}

// Window is one evaluation of a month: what was counted so far, the factor
// that scales it to the whole month, and the previous month.
type Window struct {
	Month time.Time
	// SoFar is the month so far, or the last 30 days early in a month.
	SoFar Activity
	// Factor scales SoFar to the whole month; 1 once the month has ended.
	Factor float64
	// Prior is the previous month, or the 30 days before SoFar's.
	Prior Activity
}

// WindowFor is the evaluation window of the month containing now (or of an
// ended month): the month so far from the 7th day, and before it the last 30
// days, scaled to the month. days are the PSP's daily activity of one network
// from the start of the month before last.
func WindowFor(month, now time.Time, days map[time.Time]Activity) Window {
	month = time.Date(month.Year(), month.Month(), 1, 0, 0, 0, 0, time.UTC)
	next := month.AddDate(0, 1, 0)
	sum := func(from, to time.Time) Activity {
		var a Activity
		for day, act := range days {
			if !day.Before(from) && day.Before(to) {
				a = a.Add(act)
			}
		}
		return a
	}
	w := Window{Month: month}
	monthDays := next.Sub(month).Hours() / 24
	switch elapsed := now.Sub(month).Hours() / 24; {
	case !now.Before(next):
		w.SoFar, w.Factor, w.Prior = sum(month, next), 1, sum(month.AddDate(0, -1, 0), month)
	case elapsed >= 7:
		w.SoFar, w.Factor, w.Prior = sum(month, now), monthDays/elapsed, sum(month.AddDate(0, -1, 0), month)
	default:
		start := now.AddDate(0, 0, -30)
		w.SoFar, w.Factor, w.Prior = sum(start, now), monthDays/30, sum(start.AddDate(0, 0, -30), start)
	}
	return w
}

// Standing is a program's figures for one PSP in one window.
type Standing struct {
	// Count is what the ratio counts so far; Denominator what it divides by
	// (sales, or USD for the amount basis).
	Count, Denominator int64
	// Ratio is so far; Projected is the whole month.
	Ratio, Projected float64
	ProjectedCount   int64
	Level            billing.DisputeLevel
	Reached          *Level
}

// Evaluate is p's standing in w.
func (p Program) Evaluate(w Window) Standing {
	var count, denominator int64
	switch p.Basis {
	case SameMonth:
		count, denominator = w.SoFar.Chargebacks, w.SoFar.Sales
		if p.FraudReports {
			count += w.SoFar.FraudReports
		}
	case PreviousMonth:
		count, denominator = w.SoFar.Chargebacks, w.Prior.Sales
	case FraudAmount:
		count, denominator = w.SoFar.FraudChargebackAmountUSD, w.Prior.SalesAmountUSD
	}
	s := Standing{Count: count, Denominator: denominator, Ratio: ratio(count, denominator), Level: billing.DisputeLevelNone}
	projected := int64(math.Round(float64(count) * w.Factor))
	s.ProjectedCount = projected
	if p.Basis == SameMonth {
		s.Projected = s.Ratio // both sides scale alike
	} else {
		s.Projected = ratio(projected, denominator)
	}
	// Only the ratio is projected: a minimum is met by what was counted.
	for _, l := range p.InForce(w.Month) {
		if s.Projected*10_000 < float64(l.RatioBPS) {
			continue
		}
		if p.Basis == FraudAmount {
			if count < l.MinAmountUSD || w.Prior.Sales < l.MinPriorSales {
				continue
			}
		} else if count < l.MinCount {
			continue
		}
		if s.Reached == nil || rank(l.Level) > rank(s.Reached.Level) {
			reached := l
			s.Level, s.Reached = l.Level, &reached
		}
	}
	return s
}

func ratio(count, denominator int64) float64 {
	if count <= 0 {
		return 0
	}
	return float64(count) / float64(max(denominator, 1))
}

func rank(l billing.DisputeLevel) int {
	switch l {
	case billing.DisputeLevelWarning:
		return 1
	case billing.DisputeLevelExcessive:
		return 2
	case billing.DisputeLevelHighExcessive:
		return 3
	}
	return 0
}

// Rank orders levels: none, warning, excessive, high excessive.
func Rank(l billing.DisputeLevel) int { return rank(l) }

// ParseRatioLimit reads a PSP's dispute_ratio_limit: a percentage such as
// "0.9%", more than 0 and less than 100. It answers basis points.
func ParseRatioLimit(v any) (int64, error) {
	s, ok := v.(string)
	if !ok {
		return 0, fmt.Errorf("dispute_ratio_limit is a percentage such as \"0.9%%\"")
	}
	number, found := strings.CutSuffix(strings.TrimSpace(s), "%")
	percent, err := strconv.ParseFloat(strings.TrimSpace(number), 64)
	if !found || err != nil || percent <= 0 || percent >= 100 {
		return 0, fmt.Errorf("dispute_ratio_limit %q is not a percentage between 0%% and 100%% such as \"0.9%%\"", s)
	}
	return int64(math.Round(percent * 100)), nil
}

// ChargebackLines are the count programs' standing levels as lines a
// chargeback ratio is drawn against, one per ratio: Visa's, Mastercard's and
// Stripe's, as they stand from their latest change.
func ChargebackLines() []billing.MetricsThreshold {
	names := map[billing.DisputeProgram]string{billing.ProgramVisaVAMP: "Visa VAMP", billing.ProgramMastercardECP: "Mastercard", billing.ProgramStripe: "Stripe"}
	levels := map[billing.DisputeLevel]string{billing.DisputeLevelWarning: "warning", billing.DisputeLevelExcessive: "excessive", billing.DisputeLevelHighExcessive: "high excessive"}
	labels := map[int64][]string{}
	for _, p := range Programs {
		name, ok := names[p.ID]
		if !ok {
			continue
		}
		for _, l := range p.Levels {
			if l.Until.IsZero() {
				labels[l.RatioBPS] = append(labels[l.RatioBPS], name+" "+levels[l.Level])
			}
		}
	}
	var out []billing.MetricsThreshold
	for _, bps := range slices.Sorted(maps.Keys(labels)) {
		out = append(out, billing.MetricsThreshold{Value: float64(bps) / 10_000, Label: strings.Join(labels[bps], " · ")})
	}
	return out
}
