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
	at, now        time.Time
	loc            *time.Location
}

// changeSchedule moves one schedule's funding by its share: a lot
// reversal's (reduce) or a recovery's, as a payment's reversal moves the
// schedules it funded (moveFunding). A recovery's legs are at the won
// dispute's operations, as a payment's are: its contra and deferred parts
// each at its own. A line not applied yet has no schedule: the share moves
// its deferred revenue, and the application funds the schedule with what the
// lots' money holds then.
func (b *Books) changeSchedule(ctx context.Context, sh scheduleShare, reduce bool, legs scheduleLegs) error {
	post := func(debit, credit Ref, amount int64, typ TransferType, op Operation, currency string, product *uuid.UUID, lot bool) error {
		if amount <= 0 {
			return nil
		}
		customer := legs.lot.Customer
		coord := legs.coord
		if op != "" {
			coord.Operation = op
		}
		p := Posting{Debit: debit, Credit: credit, Amount: amount, Currency: currency, Type: typ, Coord: coord, EffectiveAt: legs.at,
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
			return post(Ref{Type: DeferredRevenue}, legs.cash, sh.amount, legs.deferredType, "", c, nil, true)
		}
		return post(legs.cash, Ref{Type: DeferredRevenue}, sh.amount, DisputeWon, OpDisputeWonDeferred, c, nil, true)
	}
	if err != nil {
		return fmt.Errorf("books: lock revenue schedule %s: %w", sh.id, err)
	}
	delta := sh.amount
	if reduce {
		delta = -delta
	}
	s, contra, deferred, earned, err := b.moveFunding(ctx, row, delta, sh.contra, legs.at, legs.now, legs.loc)
	if err != nil {
		return err
	}
	if reduce {
		if err := post(Ref{Type: legs.contra}, legs.cash, contra, legs.recognizedType, "", row.Currency, row.ProductID, true); err != nil {
			return err
		}
		if err := post(Ref{Type: DeferredRevenue}, legs.cash, deferred, legs.deferredType, "", row.Currency, row.ProductID, true); err != nil {
			return err
		}
	} else {
		if err := post(legs.cash, Ref{Type: legs.contra}, contra, DisputeWon, OpDisputeWon, row.Currency, row.ProductID, true); err != nil {
			return err
		}
		if err := post(legs.cash, Ref{Type: DeferredRevenue}, deferred, DisputeWon, OpDisputeWonDeferred, row.Currency, row.ProductID, true); err != nil {
			return err
		}
		if err := post(Ref{Type: DeferredRevenue}, Ref{Type: AccountType(row.RevenueAccount)}, earned, RevenueRecognized, OpDisputeWon, row.Currency, row.ProductID, false); err != nil {
			return err
		}
	}
	return b.saveSchedule(ctx, sh.id, s, legs.loc)
}

// fundMore brings a unit line's schedule up to what the lots' money funds of
// it now, held: money released into it since it was made, already in
// deferred revenue, release the latest, at. The schedule takes it as a
// recovery would (moveFunding), and what that recognises at once posts at
// that release.
func (b *Books) fundMore(ctx context.Context, id uuid.UUID, held int64, release uuid.UUID, at time.Time, loc *time.Location) error {
	row, err := b.q.LockRevenueSchedule(ctx, gen.LockRevenueScheduleParams{MerchantID: b.merchant, ID: id})
	if err != nil {
		return fmt.Errorf("books: lock revenue schedule %s: %w", id, err)
	}
	if held <= row.Funded {
		return nil
	}
	s, recognized, _, earned, err := b.moveFunding(ctx, row, held-row.Funded, 0, at, at, loc)
	if err != nil {
		return err
	}
	if amount := recognized + earned; amount > 0 {
		coord := Coord{Operation: OpApplication, Source: "revenue_schedule", SourceID: id.String() + "@release:" + release.String()}
		_, applied, err := b.Post(ctx, Posting{
			Debit: Ref{Type: DeferredRevenue}, Credit: Ref{Type: AccountType(row.RevenueAccount)}, Amount: amount, Currency: row.Currency, Type: RevenueRecognized,
			Coord: coord, EffectiveAt: maxTime(at, s.through), Customer: &row.CustomerID, Order: &row.OrderID, OrderLine: &row.OrderLineID, Product: row.ProductID,
			RevenueSchedule: &row.ID,
		})
		if err != nil {
			return err
		}
		if !applied {
			return fmt.Errorf("%w: revenue schedule %s already posted %s", ErrCoordinateReused, id, coord)
		}
	}
	return b.saveSchedule(ctx, id, s, loc)
}
