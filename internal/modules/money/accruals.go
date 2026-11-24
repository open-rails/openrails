package money

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jonboulle/clockwork"

	"github.com/open-rails/openrails/billing"
	identity "github.com/open-rails/openrails/internal/billingidentity"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/modules/money/credits"
)

// A usage accrual posts once its UTC day ends (the billing pass's accrual
// scan), or before any other movement of the customer's balance in its
// currency: a top-up or staff grant, a credit lot's expiry, revocation or
// refund, and a usage order. A posting draws the balance first, one
// credit_spend per lot, and leaves the rest owed in one owed_accrual, at
// (usage_accrual, usage_accrual:<id>, <posting number>).

const usageAccrualSourcePrefix = "usage_accrual:"

func init() { credits.PostUsageAccruals = postUsageAccrualsHook }

// postUsageAccrualsHook is credits.PostUsageAccruals, on the caller's clock.
func postUsageAccrualsHook(ctx context.Context, q *gen.Queries, merchantID, customer uuid.UUID, currency string, liveAt *time.Time, now time.Time) (bool, error) {
	if _, bound := merchant.FromContext(ctx); !bound {
		ctx = merchant.WithID(ctx, billing.MerchantID(merchantID))
	}
	svc := &MoneyService{clock: clockwork.NewFakeClockAt(now)}
	return svc.postUsageAccrualsTx(ctx, q, merchantID, customer, currency, liveAt, nil, now)
}

// postUsageAccrualsTx posts at now what the customer's usage accrued in the
// currency and the ledger has not taken, oldest day first, drawing the lots
// live just before liveAt (now when nil); with dueBefore, only the days
// before it. What a posting leaves owed is dated when its usage occurred
// (owedAt), so the usage order of that period bills it. It takes the customer
// lock only when there is something to post.
func (s *MoneyService) postUsageAccrualsTx(ctx context.Context, q *gen.Queries, merchantID, customer uuid.UUID, currency string, liveAt, dueBefore *time.Time, now time.Time) (bool, error) {
	args := gen.ListUnpostedUsageAccrualsParams{MerchantID: merchantID, CustomerID: customer, Currency: currency}
	due := func(rows []gen.BillingUsageAccrual) []gen.BillingUsageAccrual {
		if dueBefore == nil {
			return rows
		}
		return slices.DeleteFunc(rows, func(r gen.BillingUsageAccrual) bool { return !r.DayAt.Before(*dueBefore) })
	}
	rows, err := q.ListUnpostedUsageAccruals(ctx, args)
	if rows = due(rows); err != nil || len(rows) == 0 {
		return false, err
	}
	if _, err := q.LockCustomerForSpend(ctx, gen.LockCustomerForSpendParams{MerchantID: merchantID, ID: customer}); err != nil {
		return false, err
	}
	if rows, err = q.ListUnpostedUsageAccruals(ctx, args); err != nil {
		return false, err
	}
	rows = due(rows)
	now = instant(now)
	through := now
	if liveAt != nil && liveAt.Before(now) {
		through = *liveAt
	}
	billed, err := q.LatestUsageOrderEndInCurrency(ctx, gen.LatestUsageOrderEndInCurrencyParams{MerchantID: merchantID, CustomerID: customer, Currency: currency})
	if err != nil {
		return false, err
	}
	payer := identity.CustomerID(customer)
	for _, row := range rows {
		amount := row.Amount - row.PostedAmount
		lines, err := decodeAccrualLines(row.Lines)
		if err != nil {
			return false, err
		}
		byMeter := map[string]int64{}
		var meters []string
		for i := range lines {
			delta := lines[i].Amount - lines[i].PostedAmount
			if delta > 0 {
				if _, ok := byMeter[lines[i].MeterKey]; !ok {
					meters = append(meters, lines[i].MeterKey)
				}
				byMeter[lines[i].MeterKey] += delta
			}
			lines[i].PostedAmount = lines[i].Amount
		}
		slices.Sort(meters)
		owed := make([]owedShare, 0, len(meters))
		for _, m := range meters {
			owed = append(owed, owedShare{meter: m, weight: byMeter[m]})
		}
		encoded, err := encodeAccrualLines(lines)
		if err != nil {
			return false, err
		}
		// The row advances before the ledger write, so the held total no
		// longer counts what is being posted.
		if err := q.MarkUsageAccrualPosted(ctx, gen.MarkUsageAccrualPostedParams{Lines: encoded, Now: now, MerchantID: merchantID, ID: row.ID}); err != nil {
			return false, err
		}
		posting := *s
		posting.clock = clockwork.NewFakeClockAt(now)
		posting.posting = &runningChargePosting{through: through, owed: owed, owedAt: owedAt(row, now, billed)}
		key := MustIdempotencyKey(OpUsageAccrual, usageAccrualSourcePrefix+row.ID.String(), strconv.Itoa(int(row.Postings)+1))
		_, _, applied, err := posting.spendBalanceThenOwedTx(ctx, q, payer, customer.String(), currency, key, amount, true)
		if err != nil {
			return false, err
		}
		if !applied {
			return false, fmt.Errorf("usage accrual %s: the ledger already held posting %d", row.ID, row.Postings+1)
		}
	}
	return true, nil
}

// owedAt dates what an accrual's posting leaves owed: the last instant of its
// day and pricing period (which can end within the day), or now if earlier,
// but never before the end of the customer's last usage order there, so late
// usage lands in a period no order has billed.
func owedAt(row gen.BillingUsageAccrual, now, billed time.Time) time.Time {
	at := earlier(earlier(row.DayAt.Add(24*time.Hour-time.Microsecond), row.PeriodEndsAt.Add(-time.Microsecond)), now)
	return later(at, billed)
}

// PostUsageAccruals posts at now a customer's unposted accruals in a currency
// of the days before dueBefore (every day when zero), in its own
// transaction: the day's one deduction.
func (s *MoneyService) PostUsageAccruals(ctx context.Context, customer uuid.UUID, currency string, dueBefore, now time.Time) (bool, error) {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return false, err
	}
	var posted bool
	err = s.db.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		var before *time.Time
		if !dueBefore.IsZero() {
			before = &dueBefore
		}
		posted, err = s.WithDatabase(s.db.NewWithPgxTx(tx)).postUsageAccrualsTx(ctx, gen.New(tx), mid.UUID(), customer, currency, nil, before, now)
		return err
	})
	return posted, err
}

// accrualPostingSince is the first ledger leg of the posting that took what
// the customer's usage accrued at an instant, once one has.
func (s *MoneyService) accrualPostingSince(ctx context.Context, q *gen.Queries, merchantID, customer uuid.UUID, currency string, at time.Time) (*uuid.UUID, error) {
	anchor, err := s.periodAnchorFor(ctx, q, merchantID, customer, currency)
	if err != nil {
		return nil, err
	}
	period, err := anchor.start(at)
	if err != nil {
		return nil, err
	}
	row, err := q.GetUsageAccrual(ctx, gen.GetUsageAccrualParams{MerchantID: merchantID, CustomerID: customer, Currency: currency, DayAt: dayOf(at), PeriodStartsAt: period})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if row.PostedAt == nil || row.PostedAt.Before(at) {
		return nil, nil
	}
	id, err := q.GetFirstPostingSince(ctx, gen.GetFirstPostingSinceParams{MerchantID: merchantID, CustomerID: customer, Currency: currency,
		Operation: string(OpUsageAccrual), Source: usageAccrualSourcePrefix + row.ID.String(), Since: at})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return &id, err
}

func encodeAccrualLines(lines []AccrualLine) ([]byte, error) {
	if lines == nil {
		lines = []AccrualLine{}
	}
	slices.SortFunc(lines, func(x, y AccrualLine) int {
		return cmp.Or(strings.Compare(x.MeterKey, y.MeterKey), strings.Compare(x.Cell, y.Cell))
	})
	return json.Marshal(lines)
}

// usageDeductions are the usage breakdowns of the transfers that posted an
// accrual, by transfer.
func usageDeductions(ctx context.Context, q *gen.Queries, merchantID, customer uuid.UUID, transfers []gen.BillingLedgerTransfer) (map[uuid.UUID]*billing.UsageDeduction, error) {
	ids := map[uuid.UUID][]uuid.UUID{}
	var accruals []uuid.UUID
	for _, t := range transfers {
		if t.Operation != string(OpUsageAccrual) || !strings.HasPrefix(t.Source, usageAccrualSourcePrefix) {
			continue
		}
		id, err := uuid.Parse(strings.TrimPrefix(t.Source, usageAccrualSourcePrefix))
		if err != nil {
			continue
		}
		if _, ok := ids[id]; !ok {
			accruals = append(accruals, id)
		}
		ids[id] = append(ids[id], t.ID)
	}
	out := map[uuid.UUID]*billing.UsageDeduction{}
	if len(accruals) == 0 {
		return out, nil
	}
	rows, err := q.ListUsageAccrualsByIDs(ctx, gen.ListUsageAccrualsByIDsParams{MerchantID: merchantID, CustomerID: customer, Ids: accruals})
	if err != nil {
		return nil, err
	}
	for _, row := range rows {
		lines, err := decodeAccrualLines(row.Lines)
		if err != nil {
			return nil, err
		}
		d := &billing.UsageDeduction{Day: row.DayAt.UTC().Format(time.DateOnly), Amount: row.Amount, Lines: make([]billing.UsageDeductionLine, 0, len(lines))}
		for _, l := range lines {
			line := billing.UsageDeductionLine{MeterKey: l.MeterKey, Requests: l.Requests, Quantity: l.Quantity, Amount: l.Amount}
			if l.Cell != "" {
				cell := l.Cell
				line.Cell = &cell
			}
			d.Lines = append(d.Lines, line)
		}
		for _, t := range ids[row.ID] {
			out[t] = d
		}
	}
	return out, nil
}
