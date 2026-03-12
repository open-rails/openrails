package analytics

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"github.com/jonboulle/clockwork"

	"github.com/open-rails/openrails/config"
)

// AdminMetricsService serves admin dashboard metrics backed by ClickHouse daily_metrics.
// The ClickHouse MV already carries forward quiet days and aligns processors per currency.
type AdminMetricsService struct {
	cfg   *config.ClickHouseConfig
	clock clockwork.Clock
}

func NewAdminMetricsService(cfg *config.ClickHouseConfig) *AdminMetricsService {
	return &AdminMetricsService{cfg: cfg, clock: clockwork.NewRealClock()}
}

func (s *AdminMetricsService) Clock() clockwork.Clock {
	if s.clock != nil {
		return s.clock
	}
	return clockwork.NewRealClock()
}

func (s *AdminMetricsService) SetClock(clock clockwork.Clock) {
	s.clock = clock
}

type MetricsDateRange struct {
	Start time.Time
	End   time.Time
}

func clampToToday(t time.Time) time.Time {
	today := truncateToDay(time.Now().UTC())
	if t.After(today) {
		return today
	}
	return t
}

type SummaryResponse struct {
	PeriodStart         time.Time                   `json:"period_start"`
	PeriodEnd           time.Time                   `json:"period_end"`
	Currency            string                      `json:"currency"`
	MRR                 int64                       `json:"mrr"`
	ARR                 int64                       `json:"arr"`
	TotalRevenue        int64                       `json:"total_revenue"`        // net: gross - refunds - chargebacks
	GrossRevenue        int64                       `json:"gross_revenue"`        // gross charge success
	SubscriptionRevenue int64                       `json:"subscription_revenue"` // gross subscription charges
	OneTimeRevenue      int64                       `json:"one_time_revenue"`     // gross one-time charges
	Refunds             int64                       `json:"refunds"`
	Chargebacks         int64                       `json:"chargebacks"`
	NewSubscriptions    int                         `json:"new_subscriptions"`
	Cancellations       CancellationBreakdown       `json:"cancellations"`
	NetNewSubscriptions int                         `json:"net_new_subscriptions"`
	ActiveSubscriptions ActiveSubscriptionBreakdown `json:"active_subscriptions"`
	ARPU                int64                       `json:"arpu"`
	EntitlementGrants   int                         `json:"entitlement_grants"`
	Comparison          *SummaryComparison          `json:"comparison,omitempty"`
	DataFreshAsOf       time.Time                   `json:"data_fresh_as_of"`
}

type CancellationBreakdown struct {
	Total       int `json:"total"`
	Voluntary   int `json:"voluntary"`
	Involuntary int `json:"involuntary"`
}

type ActiveSubscriptionBreakdown struct {
	Active            int `json:"active"`
	PastDue           int `json:"past_due"`
	Pending           int `json:"pending"`
	CancelledInPeriod int `json:"cancelled_in_period"`
}

type SummaryComparison struct {
	PreviousPeriod MetricsDateRange `json:"previous_period"`
	MRRDelta       int64            `json:"mrr_delta"`
	RevenueDelta   int64            `json:"total_revenue_delta"`
	NetNewDelta    int              `json:"net_new_delta"`
}

type RevenueSeriesResponse struct {
	Currency    string                `json:"currency"`
	Granularity string                `json:"granularity"`
	Buckets     []RevenueSeriesBucket `json:"buckets"`
	Totals      RevenueTotals         `json:"totals"`
}

type RevenueSeriesBucket struct {
	PeriodStart         time.Time              `json:"period_start"`
	PeriodEnd           time.Time              `json:"period_end"`
	TotalRevenue        int64                  `json:"total_revenue"` // net
	SubscriptionRevenue int64                  `json:"subscription_revenue"`
	OneTimeRevenue      int64                  `json:"one_time_revenue"`
	Refunds             int64                  `json:"refunds"`
	Chargebacks         int64                  `json:"chargebacks"`
	Payments            PaymentSeriesBreakdown `json:"payments"`
}

type PaymentSeriesBreakdown struct {
	Count         int   `json:"count"`
	AverageAmount int64 `json:"average_amount"`
}

type RevenueTotals struct {
	TotalRevenue        int64 `json:"total_revenue"` // net
	GrossRevenue        int64 `json:"gross_revenue"`
	SubscriptionRevenue int64 `json:"subscription_revenue"`
	OneTimeRevenue      int64 `json:"one_time_revenue"`
	Refunds             int64 `json:"refunds"`
	Chargebacks         int64 `json:"chargebacks"`
}

type SubscriptionSeriesResponse struct {
	Currency    string                     `json:"currency"`
	Granularity string                     `json:"granularity"`
	Buckets     []SubscriptionSeriesBucket `json:"buckets"`
	Totals      SubscriptionSeriesTotals   `json:"totals"`
}

type SubscriptionSeriesBucket struct {
	PeriodStart      time.Time                   `json:"period_start"`
	PeriodEnd        time.Time                   `json:"period_end"`
	NewSubscriptions int                         `json:"new_subscriptions"`
	ScheduledStarts  int                         `json:"scheduled_starts"`
	Cancellations    CancellationReasonBreakdown `json:"cancellations"`
	Reactivations    int                         `json:"reactivations"`
	NetChange        int                         `json:"net_change"`
	ActiveCountEnd   int                         `json:"active_count_end"`
}

type SubscriptionSeriesTotals struct {
	NewSubscriptions int `json:"new_subscriptions"`
	Cancellations    int `json:"cancellations"`
	Reactivations    int `json:"reactivations"`
	NetChange        int `json:"net_change"`
}

type CancellationReasonBreakdown struct {
	Voluntary   int `json:"voluntary"`
	Involuntary int `json:"involuntary"`
}

type ProcessorMetricsResponse struct {
	Currency    string             `json:"currency"`
	PeriodStart time.Time          `json:"period_start"`
	PeriodEnd   time.Time          `json:"period_end"`
	Processors  []ProcessorMetrics `json:"processors"`
}

type ProcessorMetrics struct {
	Processor string                `json:"processor"`
	Metrics   DailyProcessorMetrics `json:"metrics"`
}

type DailyProcessorMetrics struct {
	Revenue  DailyProcessorRevenue `json:"revenue"`
	Payments DailyProcessorPayment `json:"payments"`

	ActiveSubscriptions int `json:"active_subscriptions"`
	NewSubscriptions    int `json:"new_subscriptions"`
	Cancellations       int `json:"cancellations"`
}

type DailyProcessorRevenue struct {
	Total        int64 `json:"total"`
	Subscription int64 `json:"subscription"`
	OneTime      int64 `json:"one_time"`
	Refunds      int64 `json:"refunds"`
	Chargebacks  int64 `json:"chargebacks"`
}

type DailyProcessorPayment struct {
	Successful int `json:"successful"`
	Failed     int `json:"failed"`
}

type ChurnResponse struct {
	Currency            string                 `json:"currency"`
	PeriodStart         time.Time              `json:"period_start"`
	PeriodEnd           time.Time              `json:"period_end"`
	MonthlyChurn        []MonthlyChurnPoint    `json:"monthly_churn"`
	CancellationReasons []ReasonCount          `json:"cancellation_reasons"`
	CohortRetention     []CohortRetentionEntry `json:"cohort_retention"`
}

type MonthlyChurnPoint struct {
	Month           string  `json:"month"`
	ChurnRate       float64 `json:"churn_rate"`
	VoluntaryRate   float64 `json:"voluntary_rate"`
	InvoluntaryRate float64 `json:"involuntary_rate"`
	ActiveStart     int     `json:"active_start"`
	ActiveEnd       int     `json:"active_end"`
}

type ReasonCount struct {
	Reason string `json:"reason"`
	Count  int    `json:"count"`
}

type CohortRetentionEntry struct {
	Cohort         string                 `json:"cohort"`
	InitialSignups int                    `json:"initial_signups"`
	Retention      []CohortRetentionPoint `json:"retention"`
}

type CohortRetentionPoint struct {
	Month  int     `json:"month"`
	Active int     `json:"active"`
	Rate   float64 `json:"rate"`
}

type summaryAggRow struct {
	Currency                string    `ch:"currency"`
	SubscriptionRevenue     int64     `ch:"subscription_revenue_cents"`
	OneTimeRevenue          int64     `ch:"one_time_revenue_cents"`
	Refunds                 int64     `ch:"refunds_cents"`
	Chargebacks             int64     `ch:"chargebacks_cents"`
	GrossRevenue            int64     `ch:"gross_revenue_cents"`
	NetRevenue              int64     `ch:"net_revenue_cents"`
	NewSubscriptions        int64     `ch:"new_subscriptions"`
	CancellationsUser       int64     `ch:"cancellations_user"`
	CancellationsMerchant   int64     `ch:"cancellations_merchant"`
	CancellationsExpired    int64     `ch:"cancellations_expired"`
	CancellationsChargeback int64     `ch:"cancellations_chargeback"`
	Reactivations           int64     `ch:"reactivations"`
	EntitlementsGranted     int64     `ch:"entitlements_granted"`
	ActiveSum               int64     `ch:"active_sum"`
	PastDueSum              int64     `ch:"past_due_sum"`
	PendingSum              int64     `ch:"pending_sum"`
	DayCount                uint64    `ch:"day_count"`
	MRR                     int64     `ch:"mrr_cents"`
	ActiveEnd               int64     `ch:"active_end"`
	PastDueEnd              int64     `ch:"past_due_end"`
	PendingEnd              int64     `ch:"pending_end"`
	DataFreshAsOf           time.Time `ch:"data_fresh_as_of"`
}

type revenueBucketRow struct {
	BucketStart         time.Time `ch:"bucket_start"`
	Currency            string    `ch:"currency"`
	SubscriptionRevenue int64     `ch:"subscription_revenue_cents"`
	OneTimeRevenue      int64     `ch:"one_time_revenue_cents"`
	Refunds             int64     `ch:"refunds_cents"`
	Chargebacks         int64     `ch:"chargebacks_cents"`
	GrossRevenue        int64     `ch:"total_revenue_cents"`
	NetRevenue          int64     `ch:"total_revenue_net_cents"`
	PaymentsSuccessful  uint64    `ch:"payments_successful"`
}

type subscriptionBucketRow struct {
	BucketStart             time.Time `ch:"bucket_start"`
	Currency                string    `ch:"currency"`
	NewSubscriptions        int64     `ch:"new_subscriptions"`
	ScheduledStarts         int64     `ch:"scheduled_starts"`
	CancellationsUser       int64     `ch:"cancellations_user"`
	CancellationsMerchant   int64     `ch:"cancellations_merchant"`
	CancellationsExpired    int64     `ch:"cancellations_expired"`
	CancellationsChargeback int64     `ch:"cancellations_chargeback"`
	Reactivations           int64     `ch:"reactivations"`
	ActiveCountEnd          int64     `ch:"active_count_end"`
}

type processorAggRow struct {
	Currency            string `ch:"currency"`
	Processor           string `ch:"processor"`
	ActiveSubscriptions int64  `ch:"active_subscriptions"`
	NewSubscriptions    int64  `ch:"new_subscriptions"`
	Cancellations       int64  `ch:"cancellations"`
	RevenueTotal        int64  `ch:"revenue_total_cents"`
	RevenueSubscription int64  `ch:"revenue_subscription_cents"`
	RevenueOneTime      int64  `ch:"revenue_one_time_cents"`
	RevenueRefunds      int64  `ch:"revenue_refunds_cents"`
	RevenueChargebacks  int64  `ch:"revenue_chargebacks_cents"`
	PaymentsSuccessful  int64  `ch:"payments_successful"`
	PaymentsFailed      int64  `ch:"payments_failed"`
}

type churnBucketRow struct {
	Currency                string    `ch:"currency"`
	MonthStart              time.Time `ch:"month_start"`
	CancellationsUser       int64     `ch:"cancellations_user"`
	CancellationsMerchant   int64     `ch:"cancellations_merchant"`
	CancellationsExpired    int64     `ch:"cancellations_expired"`
	CancellationsChargeback int64     `ch:"cancellations_chargeback"`
	ActiveEnd               int64     `ch:"active_count_end"`
}

func bucketStartExpr(granularity string) string {
	switch granularity {
	case "week":
		return "toStartOfWeek(snapshot_date)"
	case "month":
		return "toStartOfMonth(snapshot_date)"
	default:
		return "snapshot_date"
	}
}

// GetSummary returns per-currency summaries using daily_metrics (already carried forward in ClickHouse).
func (s *AdminMetricsService) GetSummary(ctx context.Context, rng MetricsDateRange, currency string) ([]SummaryResponse, error) {
	startDay := truncateToDay(rng.Start)
	endDay := truncateToDay(rng.End.Add(-time.Nanosecond))
	endDay = clampToToday(endDay)

	rows, err := s.querySummaryAggregates(ctx, startDay, endDay, currency)
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return []SummaryResponse{}, nil
	}

	// Previous period for comparison
	prevLen := endDay.Add(24 * time.Hour).Sub(startDay)
	prevStart := startDay.Add(-prevLen)
	prevEnd := startDay.Add(-time.Nanosecond)
	prevRows, _ := s.querySummaryAggregates(ctx, prevStart, prevEnd, currency)
	prevByCurrency := make(map[string]summaryAggRow)
	for _, r := range prevRows {
		prevByCurrency[r.Currency] = r
	}

	var out []SummaryResponse
	for _, r := range rows {
		cancelsVol := r.CancellationsUser + r.CancellationsMerchant
		cancelsInv := r.CancellationsExpired + r.CancellationsChargeback
		netNew := r.NewSubscriptions - (cancelsVol + cancelsInv) + r.Reactivations

		avgActive := int64(0)
		if r.DayCount > 0 {
			avgActive = r.ActiveSum / int64(r.DayCount)
		}
		arpu := int64(0)
		if avgActive > 0 {
			arpu = r.NetRevenue / avgActive
		}

		resp := SummaryResponse{
			PeriodStart:         startDay,
			PeriodEnd:           endDay.Add(24 * time.Hour),
			Currency:            r.Currency,
			MRR:                 r.MRR,
			ARR:                 r.MRR * 12,
			TotalRevenue:        r.NetRevenue,
			GrossRevenue:        r.GrossRevenue,
			SubscriptionRevenue: r.SubscriptionRevenue,
			OneTimeRevenue:      r.OneTimeRevenue,
			Refunds:             r.Refunds,
			Chargebacks:         r.Chargebacks,
			NewSubscriptions:    int(r.NewSubscriptions),
			Cancellations: CancellationBreakdown{
				Total:       int(cancelsVol + cancelsInv),
				Voluntary:   int(cancelsVol),
				Involuntary: int(cancelsInv),
			},
			NetNewSubscriptions: int(netNew),
			ActiveSubscriptions: ActiveSubscriptionBreakdown{
				Active:            int(r.ActiveEnd),
				PastDue:           int(r.PastDueEnd),
				Pending:           int(r.PendingEnd),
				CancelledInPeriod: int(cancelsVol + cancelsInv),
			},
			ARPU:              arpu,
			EntitlementGrants: int(r.EntitlementsGranted),
			DataFreshAsOf:     r.DataFreshAsOf,
		}

		if prev, ok := prevByCurrency[r.Currency]; ok {
			prevCancelsVol := prev.CancellationsUser + prev.CancellationsMerchant
			prevCancelsInv := prev.CancellationsExpired + prev.CancellationsChargeback
			prevNetNew := prev.NewSubscriptions - (prevCancelsVol + prevCancelsInv) + prev.Reactivations
			resp.Comparison = &SummaryComparison{
				PreviousPeriod: MetricsDateRange{Start: prevStart, End: prevEnd.Add(24 * time.Hour)},
				MRRDelta:       resp.MRR - prev.MRR,
				RevenueDelta:   resp.TotalRevenue - prev.NetRevenue,
				NetNewDelta:    resp.NetNewSubscriptions - int(prevNetNew),
			}
		}
		out = append(out, resp)
	}

	sort.Slice(out, func(i, j int) bool {
		return out[i].Currency < out[j].Currency
	})
	return out, nil
}

func (s *AdminMetricsService) GetRevenueSeries(ctx context.Context, rng MetricsDateRange, granularity string, currency string) ([]RevenueSeriesResponse, error) {
	if granularity == "" {
		granularity = "day"
	}
	startDay := truncateToDay(rng.Start)
	endDay := truncateToDay(rng.End.Add(-time.Nanosecond))
	endDay = clampToToday(endDay)

	buckets, err := s.queryRevenueBuckets(ctx, startDay, endDay, granularity, currency)
	if err != nil {
		return nil, err
	}

	responses := make(map[string]*RevenueSeriesResponse)
	for _, r := range buckets {
		next := advance(r.BucketStart, granularity)
		if next.After(endDay.Add(24 * time.Hour)) {
			next = endDay.Add(24 * time.Hour)
		}

		resp, ok := responses[r.Currency]
		if !ok {
			resp = &RevenueSeriesResponse{
				Currency:    r.Currency,
				Granularity: granularity,
			}
			responses[r.Currency] = resp
		}

		bucket := RevenueSeriesBucket{
			PeriodStart:         r.BucketStart,
			PeriodEnd:           next,
			TotalRevenue:        r.NetRevenue,
			SubscriptionRevenue: r.SubscriptionRevenue,
			OneTimeRevenue:      r.OneTimeRevenue,
			Refunds:             r.Refunds,
			Chargebacks:         r.Chargebacks,
		}
		if r.PaymentsSuccessful > 0 {
			bucket.Payments.Count = int(r.PaymentsSuccessful)
			bucket.Payments.AverageAmount = r.NetRevenue / int64(r.PaymentsSuccessful)
		}
		resp.Buckets = append(resp.Buckets, bucket)
		resp.Totals.TotalRevenue += r.NetRevenue
		resp.Totals.GrossRevenue += r.GrossRevenue
		resp.Totals.SubscriptionRevenue += r.SubscriptionRevenue
		resp.Totals.OneTimeRevenue += r.OneTimeRevenue
		resp.Totals.Refunds += r.Refunds
		resp.Totals.Chargebacks += r.Chargebacks
	}

	var out []RevenueSeriesResponse
	for _, resp := range responses {
		out = append(out, *resp)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].Currency < out[j].Currency
	})
	return out, nil
}

func (s *AdminMetricsService) GetSubscriptionSeries(ctx context.Context, rng MetricsDateRange, granularity string, currency string) ([]SubscriptionSeriesResponse, error) {
	if granularity == "" {
		granularity = "day"
	}
	startDay := truncateToDay(rng.Start)
	endDay := truncateToDay(rng.End.Add(-time.Nanosecond))
	endDay = clampToToday(endDay)

	buckets, err := s.querySubscriptionBuckets(ctx, startDay, endDay, granularity, currency)
	if err != nil {
		return nil, err
	}

	responses := make(map[string]*SubscriptionSeriesResponse)
	for _, r := range buckets {
		next := advance(r.BucketStart, granularity)
		if next.After(endDay.Add(24 * time.Hour)) {
			next = endDay.Add(24 * time.Hour)
		}

		resp, ok := responses[r.Currency]
		if !ok {
			resp = &SubscriptionSeriesResponse{
				Currency:    r.Currency,
				Granularity: granularity,
			}
			responses[r.Currency] = resp
		}

		bucket := SubscriptionSeriesBucket{
			PeriodStart:      r.BucketStart,
			PeriodEnd:        next,
			NewSubscriptions: int(r.NewSubscriptions),
			ScheduledStarts:  int(r.ScheduledStarts),
			Cancellations: CancellationReasonBreakdown{
				Voluntary:   int(r.CancellationsUser + r.CancellationsMerchant),
				Involuntary: int(r.CancellationsExpired + r.CancellationsChargeback),
			},
			Reactivations:  int(r.Reactivations),
			ActiveCountEnd: int(r.ActiveCountEnd),
		}
		bucket.NetChange = bucket.NewSubscriptions - (bucket.Cancellations.Voluntary + bucket.Cancellations.Involuntary) + bucket.Reactivations

		resp.Buckets = append(resp.Buckets, bucket)
		resp.Totals.NewSubscriptions += bucket.NewSubscriptions
		resp.Totals.Cancellations += bucket.Cancellations.Voluntary + bucket.Cancellations.Involuntary
		resp.Totals.Reactivations += bucket.Reactivations
		resp.Totals.NetChange += bucket.NetChange
	}

	var out []SubscriptionSeriesResponse
	for _, resp := range responses {
		out = append(out, *resp)
	}
	if len(out) == 0 {
		return []SubscriptionSeriesResponse{}, nil
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].Currency < out[j].Currency
	})
	return out, nil
}

func (s *AdminMetricsService) GetProcessorMetrics(ctx context.Context, rng MetricsDateRange, currency string) ([]ProcessorMetricsResponse, error) {
	startDay := truncateToDay(rng.Start)
	endDay := truncateToDay(rng.End.Add(-time.Nanosecond))
	endDay = clampToToday(endDay)

	aggRows, err := s.queryProcessorAggregates(ctx, startDay, endDay, currency)
	if err != nil {
		return nil, err
	}

	responses := make(map[string]*ProcessorMetricsResponse)
	for _, r := range aggRows {
		resp, ok := responses[r.Currency]
		if !ok {
			resp = &ProcessorMetricsResponse{
				Currency:    r.Currency,
				PeriodStart: startDay,
				PeriodEnd:   endDay.Add(24 * time.Hour),
			}
			responses[r.Currency] = resp
		}
		resp.Processors = append(resp.Processors, ProcessorMetrics{
			Processor: r.Processor,
			Metrics: DailyProcessorMetrics{
				Revenue: DailyProcessorRevenue{
					Total:        r.RevenueTotal,
					Subscription: r.RevenueSubscription,
					OneTime:      r.RevenueOneTime,
					Refunds:      r.RevenueRefunds,
					Chargebacks:  r.RevenueChargebacks,
				},
				Payments: DailyProcessorPayment{
					Successful: int(r.PaymentsSuccessful),
					Failed:     int(r.PaymentsFailed),
				},
				ActiveSubscriptions: int(r.ActiveSubscriptions),
				NewSubscriptions:    int(r.NewSubscriptions),
				Cancellations:       int(r.Cancellations),
			},
		})
	}

	var out []ProcessorMetricsResponse
	for _, resp := range responses {
		sort.Slice(resp.Processors, func(i, j int) bool {
			return resp.Processors[i].Processor < resp.Processors[j].Processor
		})
		out = append(out, *resp)
	}
	if len(out) == 0 {
		return []ProcessorMetricsResponse{}, nil
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].Currency < out[j].Currency
	})
	return out, nil
}

func (s *AdminMetricsService) GetChurn(ctx context.Context, rng MetricsDateRange, currency string) ([]ChurnResponse, error) {
	startMonth := firstOfMonth(rng.Start)
	endMonthStart := firstOfMonth(rng.End)
	endBoundary := endMonthStart.AddDate(0, 1, 0).Add(-time.Nanosecond)
	endBoundary = clampToToday(endBoundary)

	monthly, err := s.queryChurnBuckets(ctx, startMonth, endBoundary, currency)
	if err != nil {
		return nil, err
	}
	if len(monthly) == 0 {
		return []ChurnResponse{}, nil
	}

	grouped := make(map[string][]churnBucketRow)
	for _, m := range monthly {
		grouped[m.Currency] = append(grouped[m.Currency], m)
	}
	for _, list := range grouped {
		sort.Slice(list, func(i, j int) bool { return list[i].MonthStart.Before(list[j].MonthStart) })
	}

	var out []ChurnResponse
	for currency, rows := range grouped {
		resp := ChurnResponse{
			Currency:    currency,
			PeriodStart: startMonth,
			PeriodEnd:   endMonthStart.AddDate(0, 1, 0),
		}

		prevActive := 0
		reasonCounts := map[string]int{"user": 0, "merchant": 0, "expired": 0, "chargeback": 0}

		for _, m := range rows {
			totalCancels := int(m.CancellationsUser + m.CancellationsMerchant + m.CancellationsExpired + m.CancellationsChargeback)

			activeStart := prevActive
			if activeStart == 0 {
				activeStart = int(m.ActiveEnd) + totalCancels
			}

			var churnRate, volRate, invRate float64
			if activeStart > 0 {
				churnRate = float64(totalCancels) / float64(activeStart)
				volRate = float64(m.CancellationsUser+m.CancellationsMerchant) / float64(activeStart)
				invRate = float64(m.CancellationsExpired+m.CancellationsChargeback) / float64(activeStart)
			}

			resp.MonthlyChurn = append(resp.MonthlyChurn, MonthlyChurnPoint{
				Month:           m.MonthStart.Format("2006-01"),
				ChurnRate:       churnRate,
				VoluntaryRate:   volRate,
				InvoluntaryRate: invRate,
				ActiveStart:     activeStart,
				ActiveEnd:       int(m.ActiveEnd),
			})

			reasonCounts["user"] += int(m.CancellationsUser)
			reasonCounts["merchant"] += int(m.CancellationsMerchant)
			reasonCounts["expired"] += int(m.CancellationsExpired)
			reasonCounts["chargeback"] += int(m.CancellationsChargeback)

			prevActive = int(m.ActiveEnd)
		}

		for reason, count := range reasonCounts {
			if count == 0 {
				continue
			}
			resp.CancellationReasons = append(resp.CancellationReasons, ReasonCount{
				Reason: reason,
				Count:  count,
			})
		}

		// Cohort retention: not implemented (no cohort table); return empty slice
		resp.CohortRetention = []CohortRetentionEntry{}
		out = append(out, resp)
	}

	sort.Slice(out, func(i, j int) bool {
		return out[i].Currency < out[j].Currency
	})
	return out, nil
}

func (s *AdminMetricsService) querySummaryAggregates(ctx context.Context, startDay, endDay time.Time, currency string) ([]summaryAggRow, error) {
	conn, err := s.openClickHouse()
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	query := `
        SELECT
            currency,
            sum(subscription_revenue_cents) AS subscription_revenue_cents,
            sum(one_time_revenue_cents) AS one_time_revenue_cents,
            sum(refunds_cents) AS refunds_cents,
            sum(chargebacks_cents) AS chargebacks_cents,
            sum(total_revenue_cents) AS gross_revenue_cents,
            sum(total_revenue_net_cents) AS net_revenue_cents,
            sum(new_subscriptions) AS new_subscriptions,
            sum(cancellations_user) AS cancellations_user,
            sum(cancellations_merchant) AS cancellations_merchant,
            sum(cancellations_expired) AS cancellations_expired,
            sum(cancellations_chargeback) AS cancellations_chargeback,
            sum(reactivations) AS reactivations,
            sum(coalesce(entitlements_granted, 0)) AS entitlements_granted,
            sum(active_count_end) AS active_sum,
            sum(past_due_count_end) AS past_due_sum,
            sum(pending_count_end) AS pending_sum,
            count() AS day_count,
            argMax(active_count_end, snapshot_date) AS active_end,
            argMax(past_due_count_end, snapshot_date) AS past_due_end,
            argMax(pending_count_end, snapshot_date) AS pending_end,
            argMax(mrr_cents, snapshot_date) AS mrr_cents,
            max(created_at) AS data_fresh_as_of
        FROM daily_metrics
        WHERE snapshot_date >= ? AND snapshot_date <= ?
        %[1]s
        GROUP BY currency
        ORDER BY currency`

	filter := ""
	args := []any{startDay, endDay}
	if currency != "" {
		filter = "AND currency = ?"
		args = append(args, currency)
	}

	rows, err := conn.Query(ctx, fmt.Sprintf(query, filter), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var result []summaryAggRow
	for rows.Next() {
		var r summaryAggRow
		if err := rows.ScanStruct(&r); err != nil {
			return nil, err
		}
		result = append(result, r)
	}
	return result, nil
}

func (s *AdminMetricsService) queryRevenueBuckets(ctx context.Context, startDay, endDay time.Time, granularity string, currency string) ([]revenueBucketRow, error) {
	conn, err := s.openClickHouse()
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	bucketExpr := bucketStartExpr(granularity)
	query := fmt.Sprintf(`
        SELECT
            %[1]s AS bucket_start,
            currency,
            sum(subscription_revenue_cents) AS subscription_revenue_cents,
            sum(one_time_revenue_cents) AS one_time_revenue_cents,
            sum(refunds_cents) AS refunds_cents,
            sum(chargebacks_cents) AS chargebacks_cents,
            sum(total_revenue_cents) AS total_revenue_cents,
            sum(total_revenue_net_cents) AS total_revenue_net_cents,
            sum(payments_successful) AS payments_successful
        FROM daily_metrics
        WHERE snapshot_date >= ? AND snapshot_date <= ?
        %[2]s
        GROUP BY currency, bucket_start
        ORDER BY currency, bucket_start`, bucketExpr, "%s")

	filter := ""
	args := []any{startDay, endDay}
	if currency != "" {
		filter = "AND currency = ?"
		args = append(args, currency)
	}

	rows, err := conn.Query(ctx, fmt.Sprintf(query, filter), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var result []revenueBucketRow
	for rows.Next() {
		var r revenueBucketRow
		if err := rows.ScanStruct(&r); err != nil {
			return nil, err
		}
		result = append(result, r)
	}
	return result, nil
}

func (s *AdminMetricsService) querySubscriptionBuckets(ctx context.Context, startDay, endDay time.Time, granularity string, currency string) ([]subscriptionBucketRow, error) {
	conn, err := s.openClickHouse()
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	bucketExpr := bucketStartExpr(granularity)
	query := fmt.Sprintf(`
        SELECT
            %[1]s AS bucket_start,
            currency,
            sum(new_subscriptions) AS new_subscriptions,
            sum(coalesce(scheduled_starts, 0)) AS scheduled_starts,
            sum(cancellations_user) AS cancellations_user,
            sum(cancellations_merchant) AS cancellations_merchant,
            sum(cancellations_expired) AS cancellations_expired,
            sum(cancellations_chargeback) AS cancellations_chargeback,
            sum(reactivations) AS reactivations,
            argMax(active_count_end, snapshot_date) AS active_count_end
        FROM daily_metrics
        WHERE snapshot_date >= ? AND snapshot_date <= ?
        %[2]s
        GROUP BY currency, bucket_start
        ORDER BY currency, bucket_start`, bucketExpr, "%s")

	var result []subscriptionBucketRow
	filter := ""
	args := []any{startDay, endDay}
	if currency != "" {
		filter = "AND currency = ?"
		args = append(args, currency)
	}

	rows, err := conn.Query(ctx, fmt.Sprintf(query, filter), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	for rows.Next() {
		var r subscriptionBucketRow
		if err := rows.ScanStruct(&r); err != nil {
			return nil, err
		}
		result = append(result, r)
	}
	return result, nil
}

func (s *AdminMetricsService) queryProcessorAggregates(ctx context.Context, startDay, endDay time.Time, currency string) ([]processorAggRow, error) {
	conn, err := s.openClickHouse()
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	query := `
        SELECT
            currency,
            proc.1 AS processor,
            argMax(proc.2, snapshot_date) AS active_subscriptions,
            sum(proc.3) AS new_subscriptions,
            sum(proc.4) AS cancellations,
            sum(proc.5) AS revenue_total_cents,
            sum(proc.6) AS revenue_subscription_cents,
            sum(proc.7) AS revenue_one_time_cents,
            sum(proc.8) AS revenue_refunds_cents,
            sum(proc.9) AS revenue_chargebacks_cents,
            sum(proc.10) AS payments_successful,
            sum(proc.11) AS payments_failed
        FROM (
            SELECT
                snapshot_date,
                currency,
                arrayJoin(arrayZip(
                    processor.name,
                    processor.active_subscriptions,
                    processor.new_subscriptions,
                    processor.cancellations,
                    processor.revenue_total_cents,
                    processor.revenue_subscription_cents,
                    processor.revenue_one_time_cents,
                    processor.revenue_refunds_cents,
                    processor.revenue_chargebacks_cents,
                    processor.payments_successful,
                    processor.payments_failed
                )) AS proc
            FROM daily_metrics
            WHERE snapshot_date >= ? AND snapshot_date <= ?
            %[1]s
        )
        GROUP BY currency, processor
        ORDER BY currency, processor`

	filter := ""
	args := []any{startDay, endDay}
	if currency != "" {
		filter = "AND currency = ?"
		args = append(args, currency)
	}

	rows, err := conn.Query(ctx, fmt.Sprintf(query, filter), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var result []processorAggRow
	for rows.Next() {
		var r processorAggRow
		if err := rows.ScanStruct(&r); err != nil {
			return nil, err
		}
		result = append(result, r)
	}
	return result, nil
}

func (s *AdminMetricsService) queryChurnBuckets(ctx context.Context, startMonth, endDay time.Time, currency string) ([]churnBucketRow, error) {
	conn, err := s.openClickHouse()
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	query := `
        SELECT
            currency,
            toStartOfMonth(snapshot_date) AS month_start,
            sum(cancellations_user) AS cancellations_user,
            sum(cancellations_merchant) AS cancellations_merchant,
            sum(cancellations_expired) AS cancellations_expired,
            sum(cancellations_chargeback) AS cancellations_chargeback,
            argMax(active_count_end, snapshot_date) AS active_count_end
        FROM daily_metrics
        WHERE snapshot_date >= ? AND snapshot_date <= ?
        %[1]s
        GROUP BY currency, month_start
        ORDER BY currency, month_start`

	filter := ""
	args := []any{startMonth, endDay}
	if currency != "" {
		filter = "AND currency = ?"
		args = append(args, currency)
	}

	rows, err := conn.Query(ctx, fmt.Sprintf(query, filter), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var result []churnBucketRow
	for rows.Next() {
		var r churnBucketRow
		if err := rows.ScanStruct(&r); err != nil {
			return nil, err
		}
		result = append(result, r)
	}
	return result, nil
}

func truncateToDay(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}

func advance(day time.Time, granularity string) time.Time {
	switch granularity {
	case "week":
		return day.AddDate(0, 0, 7)
	case "month":
		return time.Date(day.Year(), day.Month()+1, 1, 0, 0, 0, 0, time.UTC)
	default:
		return day.AddDate(0, 0, 1)
	}
}

func firstOfMonth(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC)
}

func (s *AdminMetricsService) openClickHouse() (driver.Conn, error) {
	if s.cfg == nil || s.cfg.ClientAddr == "" {
		return nil, fmt.Errorf("clickhouse not configured")
	}
	opts := &clickhouse.Options{
		Addr: []string{s.cfg.ClientAddr},
		Auth: clickhouse.Auth{
			Database: s.cfg.Database,
			Username: s.cfg.Username,
			Password: s.cfg.Password,
		},
		Settings: clickhouse.Settings{
			"max_execution_time": 60,
		},
		DialTimeout: 10 * time.Second,
	}
	conn, err := clickhouse.Open(opts)
	if err != nil {
		return nil, fmt.Errorf("open clickhouse: %w", err)
	}
	if err := conn.Ping(context.Background()); err != nil {
		return nil, fmt.Errorf("ping clickhouse: %w", err)
	}
	return conn, nil
}
