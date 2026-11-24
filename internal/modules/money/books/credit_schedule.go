package books

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/open-rails/openrails/internal/db/gen"
)

// A lot's spends into an order line priced in its unit fund that line's
// revenue schedules. A reversal of the lot's payment takes the consumed share
// of its cash from what the lot's money funds, pro rata: each schedule, as a
// price reduction with catch-up, and usage revenue, as contra. A won dispute
// brings back, pro rata, what its reversal took.

// scheduleShare is one schedule's part of a lot reversal or recovery, and the
// order line it schedules: what it moves, and of a recovery the part that
// returns to recognised revenue.
type scheduleShare struct {
	id, order, line uuid.UUID
	amount, contra  int64
}

func shareOf(id, order, line *uuid.UUID) scheduleShare {
	sh := scheduleShare{id: *id}
	if order != nil {
		sh.order = *order
	}
	if line != nil {
		sh.line = *line
	}
	return sh
}

// lotTakes is what a reversal's consumed share takes from the schedules the
// lot's spends funded: consumed is the cash for units the lot spent, of the
// cash beyond what was still deferred (the rest paid for units that lapsed).
func (b *Books) lotTakes(ctx context.Context, l CreditLot, cash, consumedUnits, lapsedUnits int64) ([]scheduleShare, error) {
	if cash <= 0 || consumedUnits <= 0 {
		return nil, nil
	}
	consumed := spread(cash, []int64{consumedUnits, max(lapsedUnits, 0)})[0]
	rows, err := b.q.ListCreditLotRevenue(ctx, gen.ListCreditLotRevenueParams{MerchantID: b.merchant, CreditGrantID: l.ID})
	if err != nil {
		return nil, fmt.Errorf("books: read lot %s's revenue: %w", l.ID, err)
	}
	var shares []scheduleShare
	var weights []int64
	var usage int64
	for _, r := range rows {
		if r.RevenueScheduleID == nil {
			usage += r.Held
			continue
		}
		shares, weights = append(shares, shareOf(r.RevenueScheduleID, r.OrderID, r.OrderLineID)), append(weights, max(r.Held, 0))
	}
	if len(shares) == 0 {
		return nil, nil
	}
	usage = max(usage, 0)
	parts := spread(min(consumed, sum(weights)+usage), append(weights, usage))
	var out []scheduleShare
	for i, sh := range shares {
		if parts[i] > 0 {
			sh.amount = parts[i]
			out = append(out, sh)
		}
	}
	return out, nil
}

// lotRestores is what a recovery brings back to the schedules its reversal
// took from, pro rata to what each still lacks of it beside what the
// reversal took as usage revenue's contra: each share's contra part is what
// returns to recognised revenue.
func (b *Books) lotRestores(ctx context.Context, l CreditLot, origin uuid.UUID, cash int64) ([]scheduleShare, error) {
	if cash <= 0 {
		return nil, nil
	}
	rows, err := b.q.ListCreditReversalSchedules(ctx, gen.ListCreditReversalSchedulesParams{MerchantID: b.merchant, CreditGrantID: l.ID, ReversalID: origin.String()})
	if err != nil {
		return nil, fmt.Errorf("books: read what reversal %s took from lot %s's revenue: %w", origin, l.ID, err)
	}
	var shares []scheduleShare
	var weights, contras []int64
	var other int64
	for _, r := range rows {
		if r.RevenueScheduleID == nil {
			other += r.Contra
			continue
		}
		shares = append(shares, shareOf(r.RevenueScheduleID, r.OrderID, r.OrderLineID))
		weights = append(weights, max(r.Contra, 0)+max(r.Deferred, 0))
		contras = append(contras, max(r.Contra, 0))
	}
	if len(shares) == 0 {
		return nil, nil
	}
	other = max(other, 0)
	parts := spread(min(cash, sum(weights)+other), append(weights, other))
	var out []scheduleShare
	for i, sh := range shares {
		if parts[i] > 0 {
			sh.amount, sh.contra = parts[i], spread(parts[i], []int64{contras[i], weights[i] - contras[i]})[0]
			out = append(out, sh)
		}
	}
	return out, nil
}

// scheduleLegs books a schedule's change: its cash legs at the lot's
// reversal coordinate, naming the lot, and a recovery's recognition.
type scheduleLegs struct {
	lot            CreditLot
	cash           Ref
	contra         AccountType
	deferredType   TransferType
	recognizedType TransferType
	coord          Coord
	at             time.Time
	loc            *time.Location
}

// changeSchedule moves one schedule's funding by its share: a lot
// reversal's (reduce) or a recovery's. The schedule first recognises what its
// service earned: up to the change while it serves, else to its end. A line
// not applied yet has no schedule: the share moves its deferred revenue, and
// the application funds the schedule with what the lots' money holds then.
func (b *Books) changeSchedule(ctx context.Context, sh scheduleShare, reduce bool, legs scheduleLegs) error {
	post := func(debit, credit Ref, amount int64, typ TransferType, currency string, product *uuid.UUID, lot bool) error {
		if amount <= 0 {
			return nil
		}
		customer := legs.lot.Customer
		p := Posting{Debit: debit, Credit: credit, Amount: amount, Currency: currency, Type: typ, Coord: legs.coord, EffectiveAt: legs.at,
			Customer: &customer, OrderLine: &sh.line, Product: product, RevenueSchedule: &sh.id}
		if sh.order != uuid.Nil {
			p.Order = &sh.order
		}
		if lot {
			p.CreditGrant = &legs.lot.ID
		}
		if _, _, err := b.Post(ctx, p); err != nil {
			return fmt.Errorf("books: revenue schedule %s %s: %w", sh.id, typ, err)
		}
		return nil
	}
	row, err := b.q.LockRevenueSchedule(ctx, gen.LockRevenueScheduleParams{MerchantID: b.merchant, ID: sh.id})
	if errors.Is(err, pgx.ErrNoRows) {
		c := legs.lot.PaidCurrency
		if reduce {
			return post(Ref{Type: DeferredRevenue}, legs.cash, sh.amount, legs.deferredType, c, nil, true)
		}
		return post(legs.cash, Ref{Type: DeferredRevenue}, sh.amount, DisputeWon, c, nil, true)
	}
	if err != nil {
		return fmt.Errorf("books: lock revenue schedule %s: %w", sh.id, err)
	}
	s := scheduleOf(row)
	end := s.end()
	t := maxTime(legs.at, s.through)
	serving := t.Before(end)
	var steps []recognition
	if serving {
		if steps, _, err = s.advance(ctx, legs.loc, t); err != nil {
			return err
		}
		more, _ := s.catchUp(t, legs.loc)
		steps = append(steps, more...)
	} else if steps, _, err = s.advance(ctx, legs.loc, legs.at); err != nil {
		return err
	}
	if err := b.recognize(ctx, row, steps); err != nil {
		return err
	}
	if reduce {
		contra, deferred := s.reduce(sh.amount, serving)
		if err := post(Ref{Type: legs.contra}, legs.cash, contra, legs.recognizedType, row.Currency, row.ProductID, true); err != nil {
			return err
		}
		if err := post(Ref{Type: DeferredRevenue}, legs.cash, deferred, legs.deferredType, row.Currency, row.ProductID, true); err != nil {
			return err
		}
	} else {
		c, deferred, earned := s.restore(sh.amount, sh.contra, serving, end)
		if err := post(legs.cash, Ref{Type: legs.contra}, c, DisputeWon, row.Currency, row.ProductID, true); err != nil {
			return err
		}
		if err := post(legs.cash, Ref{Type: DeferredRevenue}, deferred, DisputeWon, row.Currency, row.ProductID, true); err != nil {
			return err
		}
		if err := post(Ref{Type: DeferredRevenue}, Ref{Type: AccountType(row.RevenueAccount)}, earned, RevenueRecognized, row.Currency, row.ProductID, false); err != nil {
			return err
		}
	}
	n, err := b.q.AdjustRevenueSchedule(ctx, gen.AdjustRevenueScheduleParams{
		MerchantID: b.merchant, ID: sh.id, Funded: s.funded, RecognizedNet: s.recognized, RecognizedThroughAt: s.through,
		NextRecognitionAt: s.nextDue(legs.loc), Reinstatements: int32(s.reinstatements), // #nosec G115 -- one per undone end
	})
	if err == nil && n != 1 {
		err = fmt.Errorf("books: revenue schedule %s did not record its change", sh.id)
	}
	return err
}

// fundMore brings a unit line's schedule up to what the lots' money funds of
// it now, held: money released into it since it was made. The money is in
// deferred revenue already; the schedule recognises what the higher price
// earned by at.
func (b *Books) fundMore(ctx context.Context, id uuid.UUID, held int64, at time.Time, loc *time.Location) error {
	row, err := b.q.LockRevenueSchedule(ctx, gen.LockRevenueScheduleParams{MerchantID: b.merchant, ID: id})
	if err != nil {
		return fmt.Errorf("books: lock revenue schedule %s: %w", id, err)
	}
	if held <= row.Funded {
		return nil
	}
	s := scheduleOf(row)
	t := maxTime(at, s.through)
	steps, _, err := s.advance(ctx, loc, t)
	if err != nil {
		return err
	}
	s.funded = held
	more, _ := s.catchUp(t, loc)
	if err := b.recognize(ctx, row, append(steps, more...)); err != nil {
		return err
	}
	n, err := b.q.AdjustRevenueSchedule(ctx, gen.AdjustRevenueScheduleParams{
		MerchantID: b.merchant, ID: id, Funded: s.funded, RecognizedNet: s.recognized, RecognizedThroughAt: s.through,
		NextRecognitionAt: s.nextDue(loc), Reinstatements: int32(s.reinstatements), // #nosec G115 -- one per undone end
	})
	if err == nil && n != 1 {
		err = fmt.Errorf("books: revenue schedule %s did not record its funding", id)
	}
	return err
}
