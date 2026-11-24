package books

import (
	"context"
	"fmt"
	"math/bits"
	"time"

	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/shared/calendarutil"
)

// The general ledger's operations, the first part of each posting's
// coordinate.
const (
	OpCash        Operation = "cash"
	OpApplication Operation = "application"
	OpRecognition Operation = "recognition"
	OpTermination Operation = "termination"
	// OpReinstatement takes back revenue an early end forfeited once that end
	// is undone or moved later.
	OpReinstatement Operation = "reinstatement"
)

// Method is how a revenue schedule recognises.
type Method string

const (
	// Ratable recognises by elapsed service.
	Ratable Method = "ratable"
	// PointInTime recognises everything at the application.
	PointInTime Method = "point_in_time"
)

// Service is the service a line sells: [Starts, Ends), in Months calendar
// months when it is sold by the month (each an equal share), else by elapsed
// time.
type Service struct {
	Starts, Ends time.Time
	Months       int
}

// calendar is s, by the month only when it is exactly Months calendar months.
func (s Service) calendar() Service {
	if s.Months > 1 && !s.Ends.Equal(calendarutil.AddMonths(s.Starts, s.Months)) {
		s.Months = 0
	}
	return s
}

// boundary is where the service's i-th month ends; Ends from the last.
func (s Service) boundary(i int) time.Time {
	if i >= s.Months {
		return s.Ends
	}
	if b := calendarutil.AddMonths(s.Starts, i); b.Before(s.Ends) {
		return b
	}
	return s.Ends
}

// target is how much of funded is earned by t: nothing before the service
// starts, all of it from its end, the elapsed share between (rounded down).
func (s Service) target(funded int64, t time.Time) int64 {
	if !t.Before(s.Ends) {
		return funded
	}
	if !t.After(s.Starts) {
		return 0
	}
	if s.Months <= 1 {
		return share(funded, t.Sub(s.Starts).Microseconds(), s.Ends.Sub(s.Starts).Microseconds())
	}
	k := 0
	for k+1 < s.Months && !s.boundary(k+1).After(t) {
		k++
	}
	from, to := s.boundary(k), s.boundary(k+1)
	month := to.Sub(from).Microseconds()
	return share(funded, int64(k)*month+t.Sub(from).Microseconds(), int64(s.Months)*month)
}

// share is ⌊f × num ÷ den⌋, in 128 bits; f from num ≥ den.
func share(f, num, den int64) int64 {
	if f <= 0 || num <= 0 || den <= 0 {
		return 0
	}
	if num >= den {
		return f
	}
	hi, lo := bits.Mul64(uint64(f), uint64(num))
	q, _ := bits.Div64(hi, lo, uint64(den))
	return int64(q) // #nosec G115 -- q < f
}

// spread divides amount over weights in proportion, rounding cumulatively so
// the parts sum to amount exactly; a part never exceeds its weight when amount
// does not exceed their sum.
func spread(amount int64, weights []int64) []int64 {
	parts := make([]int64, len(weights))
	var total int64
	for _, w := range weights {
		total += max(w, 0)
	}
	if amount <= 0 || total <= 0 {
		return parts
	}
	var before, given int64
	for i, w := range weights {
		before += max(w, 0)
		upto := share(amount, before, total)
		if before >= total {
			upto = amount
		}
		parts[i] = upto - given
		given = upto
	}
	return parts
}

// MonthStart is the instant the next 1st begins in loc after t: where the
// accounting month that holds t ends. The 1st begins as PostgreSQL resolves
// its midnight (calendarutil.DayStart): at the transition where clocks spring
// forward over it, at the later one where it happens twice. An instant in that
// repeated hour is still before its 1st begins.
func MonthStart(t time.Time, loc *time.Location) time.Time {
	l := t.In(loc)
	if b := calendarutil.DayStart(l.Year(), l.Month(), 1, loc); b.After(t) {
		return b
	}
	return calendarutil.DayStart(l.Year(), l.Month()+1, 1, loc)
}

// schedule is a revenue schedule's terms and progress.
type schedule struct {
	service    Service
	funded     int64
	recognized int64
	through    time.Time
	ended      *time.Time
	// reinstatements counts forfeits taken back: the generation of the
	// termination and reinstatement coordinates.
	reinstatements int
	// forfeited is what of recognized an early end forfeited and no undone
	// end took back: all an undone end reinstates.
	forfeited int64
}

// recognition is one posting a schedule owes: amount from deferred revenue,
// at the instant its coordinate names (in its generation, for a termination
// or reinstatement), dated when it takes effect.
type recognition struct {
	op     Operation
	at     time.Time
	gen    int
	dated  time.Time
	amount int64
}

func scheduleOf(row gen.BillingRevenueSchedule) schedule {
	s := schedule{service: Service{Starts: row.ServiceStartsAt, Ends: row.ServiceEndsAt}, funded: row.Funded,
		recognized: row.RecognizedNet, through: row.RecognizedThroughAt, ended: row.EndedAt, reinstatements: int(row.Reinstatements), forfeited: row.Forfeited}
	if row.ServiceMonths != nil {
		s.service.Months = int(*row.ServiceMonths)
	}
	return s
}

// end is where recognition stops: the service's end, or its early end.
func (s *schedule) end() time.Time {
	if s.ended != nil && s.ended.Before(s.service.Ends) {
		return *s.ended
	}
	return s.service.Ends
}

func (s *schedule) earn(steps []recognition, op Operation, at, dated time.Time, target int64) []recognition {
	if amount := target - s.recognized; amount > 0 {
		steps = append(steps, recognition{op: op, at: at, gen: s.reinstatements, dated: dated, amount: amount})
		s.recognized = target
	}
	return steps
}

// catchUp recognises at the application what the service earned by then, in
// one posting dated then: recognition starts at payment. A service already
// over is recognised whole, its early end forfeiting the rest.
func (s *schedule) catchUp(at time.Time, loc *time.Location) ([]recognition, *time.Time) {
	end := s.end()
	var steps []recognition
	if upto := minTime(at, end); upto.After(s.through) || !at.Before(end) {
		steps = s.earn(steps, OpRecognition, at, at, s.service.target(s.funded, upto))
		s.through = maxTime(s.through, upto)
	}
	if at.Before(end) {
		return steps, s.nextDue(loc)
	}
	if s.ended != nil {
		steps = s.forfeit(steps, at, at)
	}
	return steps, nil
}

// forfeit recognises what an early end leaves unearned, at its end.
func (s *schedule) forfeit(steps []recognition, at, dated time.Time) []recognition {
	before := s.recognized
	steps = s.earn(steps, OpTermination, at, dated, s.funded)
	s.forfeited += s.recognized - before
	return steps
}

// advance recognises every month boundary reached by now, each dated the last
// instant before it, and at the end the rest; an early end forfeits what its
// service did not earn, and an end undone takes back what it forfeited. It
// returns when the schedule is next due, nil once done.
func (s *schedule) advance(ctx context.Context, loc *time.Location, now time.Time) ([]recognition, *time.Time, error) {
	return s.progress(ctx, loc, now, true)
}

// progress is advance; without forfeit an early end recognises what its
// service earned and leaves the rest deferred, for a reversal to take first.
func (s *schedule) progress(ctx context.Context, loc *time.Location, now time.Time, forfeit bool) ([]recognition, *time.Time, error) {
	end := s.end()
	var steps []recognition
	// An end undone takes back what it forfeited, never more: revenue a
	// reversal took is no forfeit.
	if back := min(s.recognized-s.service.target(s.funded, s.through), s.forfeited); back > 0 && (s.ended == nil || s.ended.After(s.through)) {
		s.reinstatements++
		steps = append(steps, recognition{op: OpReinstatement, at: s.through, gen: s.reinstatements, dated: now, amount: back})
		s.recognized -= back
		s.forfeited -= back
	}
	for {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		b := MonthStart(s.through, loc)
		if !b.After(s.through) {
			return nil, nil, fmt.Errorf("books: no month boundary after %s in %s", s.through.UTC().Format(time.RFC3339Nano), loc)
		}
		if !b.Before(end) {
			break
		}
		if b.After(now) {
			return steps, &b, nil
		}
		steps = s.earn(steps, OpRecognition, b, b.Add(-time.Microsecond), s.service.target(s.funded, b))
		s.through = b
	}
	if end.After(now) {
		return steps, &end, nil
	}
	dated := end.Add(-time.Microsecond)
	steps = s.earn(steps, OpRecognition, end, dated, s.service.target(s.funded, end))
	if s.ended != nil && forfeit {
		steps = s.forfeit(steps, end, dated)
	}
	s.through = maxTime(s.through, end)
	return steps, nil, nil
}

// nextDue is the next boundary, or the end; nil once recognition is done. A
// schedule whose recognition reached its end owes the rest there: an early end
// that has not yet forfeited it.
func (s *schedule) nextDue(loc *time.Location) *time.Time {
	end := s.end()
	if !s.through.Before(end) {
		if s.recognized < s.funded {
			return &end
		}
		return nil
	}
	b := MonthStart(s.through, loc)
	if b.Before(end) {
		return &b
	}
	return &end
}

func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

func maxTime(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}
