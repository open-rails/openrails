package books

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestRecognitionBoundaries(t *testing.T) {
	ny, err := time.LoadLocation("America/New_York")
	require.NoError(t, err)
	at := func(s string) time.Time {
		v, err := time.Parse(time.RFC3339Nano, s)
		require.NoError(t, err)
		return v
	}
	type step struct {
		op        Operation
		at, dated string
		amount    int64
	}
	steps := func(rs []recognition) []step {
		var out []step
		for _, r := range rs {
			out = append(out, step{r.op, r.at.UTC().Format(time.RFC3339Nano), r.dated.UTC().Format(time.RFC3339Nano), r.amount})
		}
		return out
	}

	// A month bought mid-June: 15.5 of 30 days in June.
	s := schedule{service: Service{Starts: at("2026-06-15T12:00:00Z"), Ends: at("2026-07-15T12:00:00Z")}, funded: 10_000_000, through: at("2026-06-15T12:00:00Z")}
	got, next, err := s.advance(context.Background(), time.UTC, at("2026-07-20T00:00:00Z"))
	require.NoError(t, err)
	require.Equal(t, []step{{OpRecognition, "2026-07-01T00:00:00Z", "2026-06-30T23:59:59.999999Z", 5_166_666}, {OpRecognition, "2026-07-15T12:00:00Z", "2026-07-15T11:59:59.999999Z", 4_833_334}}, steps(got))
	require.Nil(t, next)

	// A year by the calendar month: a twelfth each, leap February included.
	s = schedule{service: Service{Starts: at("2024-01-01T00:00:00Z"), Ends: at("2025-01-01T00:00:00Z"), Months: 12}, funded: 120_000_000, through: at("2024-01-01T00:00:00Z")}
	got, next, err = s.advance(context.Background(), time.UTC, at("2024-03-01T01:00:00Z"))
	require.NoError(t, err)
	require.Equal(t, []step{{OpRecognition, "2024-02-01T00:00:00Z", "2024-01-31T23:59:59.999999Z", 10_000_000}, {OpRecognition, "2024-03-01T00:00:00Z", "2024-02-29T23:59:59.999999Z", 10_000_000}}, steps(got))
	require.Equal(t, at("2024-04-01T00:00:00Z"), *next)
	got, _, err = s.advance(context.Background(), time.UTC, at("2025-02-01T00:00:00Z"))
	require.NoError(t, err)
	require.Len(t, got, 10)
	require.EqualValues(t, 120_000_000, s.recognized)

	// New York's April starts at 04:00 UTC once DST began.
	start := time.Date(2026, time.March, 1, 0, 0, 0, 0, ny).UTC()
	s = schedule{service: Service{Starts: start, Ends: start.AddDate(0, 1, 0)}, funded: 10_000_000, through: start}
	got, next, err = s.advance(context.Background(), ny, at("2026-04-01T04:30:00Z"))
	require.NoError(t, err)
	require.Equal(t, []step{{OpRecognition, "2026-04-01T04:00:00Z", "2026-04-01T03:59:59.999999Z", 9_986_559}}, steps(got))
	require.Equal(t, at("2026-04-01T05:00:00Z"), *next)

	// An immediate cancellation six hours into a day: earned, then forfeited.
	day := at("2026-09-10T08:00:00Z")
	ended := day.Add(6 * time.Hour)
	s = schedule{service: Service{Starts: day, Ends: day.Add(24 * time.Hour)}, funded: 10_000_000, through: day, ended: &ended}
	got, next, err = s.advance(context.Background(), time.UTC, ended.Add(time.Hour))
	require.NoError(t, err)
	require.Equal(t, []step{{OpRecognition, "2026-09-10T14:00:00Z", "2026-09-10T13:59:59.999999Z", 2_500_000}, {OpTermination, "2026-09-10T14:00:00Z", "2026-09-10T13:59:59.999999Z", 7_500_000}}, steps(got))
	require.Nil(t, next)

	// Recognition starts at payment: an hour of a 720-hour period paid late.
	period := at("2026-08-03T00:00:00Z")
	s = schedule{service: Service{Starts: period, Ends: period.Add(720 * time.Hour)}, funded: 18_000_000, through: period}
	got, next = s.catchUp(period.Add(time.Hour), time.UTC)
	require.Equal(t, []step{{OpRecognition, "2026-08-03T01:00:00Z", "2026-08-03T01:00:00Z", 25_000}}, steps(got))
	require.Equal(t, at("2026-09-01T00:00:00Z"), *next)

	// Delivery at once.
	s = schedule{service: Service{Starts: period, Ends: period}, funded: 25_000_000, through: period}
	got, next = s.catchUp(period, time.UTC)
	require.Equal(t, []step{{OpRecognition, "2026-08-03T00:00:00Z", "2026-08-03T00:00:00Z", 25_000_000}}, steps(got))
	require.Nil(t, next)
}

func TestAllocation(t *testing.T) {
	// 20.00 less a 2.00 adjustment, 3.00 settled without cash, 15.00 paid.
	a := allocate([]int64{20_000_000, -2_000_000}, []bool{false, false}, []int64{15_000_000}, 3_000_000)
	require.Equal(t, []int64{18_000_000, 0}, a.net)
	require.Equal(t, []int64{3_000_000, 0}, a.nonCash)
	require.Equal(t, [][]int64{{15_000_000, 0}}, a.cash)

	// Two lines, an adjustment spread in proportion, two payments and more
	// money than the bill: the excess stays unapplied.
	a = allocate([]int64{10, 20, -3}, []bool{false, false, false}, []int64{20, 20}, 0)
	require.Equal(t, []int64{9, 18, 0}, a.net)
	require.Equal(t, [][]int64{{6, 14, 0}, {3, 4, 0}}, a.cash)
	for i := range a.net {
		require.Equal(t, a.net[i], a.cash[0][i]+a.cash[1][i])
	}
}

// Two applications of one bill: a line applied first, the other once its
// access exists, with a payment added between them that sorts before one
// already applied. No payment is applied beyond what it brought.
func TestApplicationsStayWithinReceipts(t *testing.T) {
	type second struct {
		receipts []int64
		// from[i] is the first application's receipt that second receipt i is.
		from []int
	}
	for _, c := range []struct {
		lines    []int64
		receipts []int64
		second   second
	}{
		// The invoice's own 5.00 and another 15.00 paid 20.00 of 36.00; a
		// 12.50 reported late sorts between them.
		{[]int64{22_000_000, 14_000_000}, []int64{5_000_000, 15_000_000}, second{[]int64{5_000_000, 12_500_000, 15_000_000}, []int{0, -1, 1}}},
		{[]int64{4_000_000, 23_000_000}, []int64{7_500_000, 12_000_000}, second{[]int64{7_500_000, 2_000_000, 12_000_000}, []int{0, -1, 1}}},
		// One payment pruned, another added.
		{[]int64{10_000_000, 17_000_000}, []int64{8_000_000, 19_000_000}, second{[]int64{8_000_000, 19_000_000}, []int{-1, 1}}},
	} {
		done := []bool{false, false}
		first := allocateRest(c.lines, done, c.receipts, make([]int64, len(c.receipts)), 0, 0)
		// Only the second line is ready: it is applied.
		applied := make([]int64, len(c.receipts))
		for r := range c.receipts {
			applied[r] = first.cash[r][1]
		}
		before := make([]int64, len(c.second.receipts))
		for r, from := range c.second.from {
			if from >= 0 {
				before[r] = applied[from]
			}
		}
		next := allocateRest(c.lines, []bool{false, true}, c.second.receipts, before, 0, 0)
		var line int64
		for r, amount := range c.second.receipts {
			require.LessOrEqual(t, before[r]+next.cash[r][0], amount, "receipt %d of %v", r, c.second.receipts)
			line += next.cash[r][0]
		}
		for r := range c.second.receipts {
			require.Zero(t, next.cash[r][1], "an applied line takes nothing again")
		}
		require.LessOrEqual(t, line, c.lines[0])
	}
}

func TestMonthStartWhereMidnightIsSkipped(t *testing.T) {
	// Paraguay sprang forward at midnight on 1 October 2023: that midnight
	// never happened, and October began at 01:00 -03, 04:00 UTC.
	asuncion, err := time.LoadLocation("America/Asuncion")
	require.NoError(t, err)
	october := time.Date(2023, time.October, 1, 4, 0, 0, 0, time.UTC)
	for _, from := range []time.Time{
		time.Date(2023, time.September, 15, 12, 0, 0, 0, time.UTC),
		time.Date(2023, time.October, 1, 3, 0, 0, 0, time.UTC),
		time.Date(2023, time.October, 1, 3, 30, 0, 0, time.UTC),
	} {
		b := MonthStart(from, asuncion)
		require.Equal(t, october, b.UTC(), from)
		require.Equal(t, 1, b.In(asuncion).Day())
	}
	require.Equal(t, time.Date(2023, time.November, 1, 3, 0, 0, 0, time.UTC), MonthStart(october, asuncion).UTC())

	// A schedule across the change moves on, never stalling on the missing
	// midnight.
	start := time.Date(2023, time.September, 15, 12, 0, 0, 0, time.UTC)
	s := schedule{service: Service{Starts: start, Ends: start.Add(61 * 24 * time.Hour)}, funded: 61_000_000, through: start}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	got, next, err := s.advance(ctx, asuncion, time.Date(2023, time.November, 20, 0, 0, 0, 0, time.UTC))
	require.NoError(t, err)
	require.Nil(t, next)
	require.Len(t, got, 3)
	require.Equal(t, october, got[0].at.UTC())
	require.Equal(t, time.Date(2023, time.November, 1, 3, 0, 0, 0, time.UTC), got[1].at.UTC())
	require.EqualValues(t, 61_000_000, s.recognized)
}

func TestMonthStartWhereMidnightRepeats(t *testing.T) {
	// Cuba falls back from 01:00 to midnight on 1 November 2026: that midnight
	// happens at 04:00 and again at 05:00 UTC. November begins at the later,
	// as PostgreSQL's date_trunc has it.
	havana, err := time.LoadLocation("America/Havana")
	require.NoError(t, err)
	november := time.Date(2026, time.November, 1, 5, 0, 0, 0, time.UTC)
	for _, from := range []time.Time{
		time.Date(2026, time.October, 15, 12, 0, 0, 0, time.UTC),
		time.Date(2026, time.November, 1, 3, 59, 0, 0, time.UTC),
		// In the repeated hour, before November begins.
		time.Date(2026, time.November, 1, 4, 30, 0, 0, time.UTC),
	} {
		require.Equal(t, november, MonthStart(from, havana).UTC(), from)
	}
	require.Equal(t, time.Date(2026, time.December, 1, 5, 0, 0, 0, time.UTC), MonthStart(november, havana).UTC())

	// A schedule starting in that hour recognises its half hour of October at
	// November's start: the boundary is not skipped.
	start := time.Date(2026, time.November, 1, 4, 30, 0, 0, time.UTC)
	end := start.Add(30 * 24 * time.Hour)
	s := schedule{service: Service{Starts: start, Ends: end}, funded: 30_000_000, through: start}
	got, next, err := s.advance(context.Background(), havana, start.Add(2*time.Hour))
	require.NoError(t, err)
	require.Equal(t, &end, next, "December begins after the service ends")
	require.Len(t, got, 1)
	require.Equal(t, november, got[0].at.UTC())
	require.Equal(t, november.Add(-time.Microsecond), got[0].dated.UTC())
	require.EqualValues(t, 20_833, got[0].amount, "half an hour of 720")
}

func TestScheduleEnds(t *testing.T) {
	at := func(s string) time.Time {
		v, err := time.Parse(time.RFC3339, s)
		require.NoError(t, err)
		return v
	}
	ctx := context.Background()

	// A period paid in advance whose service ended before it began: due at its
	// start, where all of it is forfeited.
	start := at("2026-10-01T00:00:00Z")
	ended := start
	s := schedule{service: Service{Starts: start, Ends: start.Add(720 * time.Hour)}, funded: 10_000_000, through: start, ended: &ended}
	got, next := s.catchUp(start.Add(-48*time.Hour), time.UTC)
	require.Empty(t, got)
	require.NotNil(t, next, "an end not yet forfeited stays due")
	require.Equal(t, start, *next)
	got, next, err := s.advance(ctx, time.UTC, start.Add(time.Hour))
	require.NoError(t, err)
	require.Nil(t, next)
	require.Equal(t, []recognition{{op: OpTermination, at: start, dated: start.Add(-time.Microsecond), amount: 10_000_000}}, got)

	// An end undone after it forfeited: the forfeit is taken back and the
	// service earns on from there.
	day := at("2026-09-10T08:00:00Z")
	cut := day.Add(6 * time.Hour)
	s = schedule{service: Service{Starts: day, Ends: day.Add(24 * time.Hour)}, funded: 10_000_000, through: day, ended: &cut}
	_, _, err = s.advance(ctx, time.UTC, cut.Add(time.Hour))
	require.NoError(t, err)
	require.EqualValues(t, 10_000_000, s.recognized)
	s.ended = nil
	undone := cut.Add(2 * time.Hour)
	got, next, err = s.advance(ctx, time.UTC, undone)
	require.NoError(t, err)
	require.Equal(t, []recognition{{op: OpReinstatement, at: cut, gen: 1, dated: undone, amount: 7_500_000}}, got)
	require.Equal(t, day.Add(24*time.Hour), *next)
	got, _, err = s.advance(ctx, time.UTC, day.Add(25*time.Hour))
	require.NoError(t, err)
	require.Equal(t, []recognition{{op: OpRecognition, at: day.Add(24 * time.Hour), gen: 1, dated: day.Add(24*time.Hour - time.Microsecond), amount: 7_500_000}}, got)

	// An end that flaps: ended, undone and ended again at one instant, then
	// undone again. Each termination and reinstatement is a new generation.
	s = schedule{service: Service{Starts: day, Ends: day.Add(24 * time.Hour)}, funded: 10_000_000, through: day, ended: &cut}
	var ops []recognition
	for i, now := range []time.Time{cut.Add(time.Hour), cut.Add(2 * time.Hour), cut.Add(3 * time.Hour), cut.Add(4 * time.Hour)} {
		if i%2 == 0 {
			s.ended = &cut
		} else {
			s.ended = nil
		}
		got, _, err = s.advance(ctx, time.UTC, now)
		require.NoError(t, err)
		ops = append(ops, got...)
	}
	type coordinate struct {
		op  Operation
		gen int
	}
	var seen []coordinate
	for _, r := range ops {
		seen = append(seen, coordinate{r.op, r.gen})
	}
	require.Equal(t, []coordinate{{OpRecognition, 0}, {OpTermination, 0}, {OpReinstatement, 1}, {OpTermination, 1}, {OpReinstatement, 2}}, seen)
	require.Equal(t, 2, s.reinstatements)

	// A zone changed after the application: the boundaries not yet posted are
	// the new zone's.
	ny, err := time.LoadLocation("America/New_York")
	require.NoError(t, err)
	bought := at("2026-01-20T12:00:00Z")
	s = schedule{service: Service{Starts: bought, Ends: bought.AddDate(0, 3, 0)}, funded: 89_000_000, through: bought}
	_, next = s.catchUp(bought, time.UTC)
	require.Equal(t, at("2026-02-01T00:00:00Z"), *next)
	got, next, err = s.advance(ctx, ny, at("2026-03-01T06:00:00Z"))
	require.NoError(t, err)
	require.Equal(t, at("2026-02-01T05:00:00Z"), got[0].at.UTC())
	require.Equal(t, at("2026-03-01T05:00:00Z"), got[1].at.UTC())
	require.Equal(t, at("2026-04-01T04:00:00Z"), next.UTC())

	// A calendar term only when the service is exactly its months.
	require.Equal(t, 12, Service{Starts: at("2025-02-28T00:00:00Z"), Ends: at("2026-02-28T00:00:00Z"), Months: 12}.calendar().Months)
	require.Zero(t, Service{Starts: at("2025-02-28T00:00:00Z"), Ends: at("2026-03-31T00:00:00Z"), Months: 12}.calendar().Months)
}
