package money

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jonboulle/clockwork"

	"github.com/open-rails/openrails/billing"
	identity "github.com/open-rails/openrails/internal/billingidentity"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/modules/money/credits"
)

// A posting moves what a running charge accrued through an instant to the
// ledger: balance first, the rest owed, as an accrual's posting does.
// Postings happen on events only: the stop, each 00:00 UTC (the balance watch
// gets to it), before a credit lot the accrual draws is expired, revoked or
// refunded, and before usage is billed. No posting straddles 00:00 UTC, and any number of
// them sums to the charge's total through its stop.

const (
	// runningChargeSource is the ledger source of a posting; its source id is
	// "<charge id>/<through>".
	runningChargeSource        = "running_charge"
	runningChargeInstantLayout = "2006-01-02T15:04:05.000000Z"
)

func init() { credits.PostRunningCharges = postRunningChargesThrough }

// runningChargePosting is what a posting's ledger write runs under: accrual is
// held as of through, the lots that had not lapsed just before it are drawn,
// and the balance legs carry the charge's resource. A usage posting runs
// under it too.
type runningChargePosting struct {
	through  time.Time
	resource string
	// usagePosted is a posting after the customer's usage before through
	// posted, or the usage posting itself: it holds no unposted usage, which
	// as of through is none.
	usagePosted bool
	// owed splits what a usage posting leaves owed by meter.
	owed []owedShare
	// owedAt dates what a usage posting leaves owed: when its usage
	// occurred. Zero dates it at the posting.
	owedAt time.Time
	// from is when what the posting charges began: a usage posting's pricing
	// period, a running charge's last posting. The usage order that bills
	// what it leaves owed starts no later.
	from time.Time
}

// owedShare is one meter's weight in a usage posting's owed remainder.
type owedShare struct {
	meter  string
	weight int64
}

func (s *MoneyService) accruedAsOf() *time.Time {
	if s.posting == nil {
		return nil
	}
	return &s.posting.through
}

func (s *MoneyService) lotsLiveAt() *time.Time {
	if s.posting == nil {
		return nil
	}
	t := s.posting.through.Add(-time.Microsecond)
	return &t
}

func (s *MoneyService) postingResource() string {
	if s.posting == nil {
		return ""
	}
	return s.posting.resource
}

// postRunningCharge posts c through an instant not before its last posting,
// and stops it there when stop is set: one posting through each 00:00 UTC on
// the way, then one through the instant. The caller holds the customer lock.
// It works through q alone.
func (s *MoneyService) postRunningCharge(ctx context.Context, q *gen.Queries, c gen.BillingRunningCharge, through time.Time, stop bool) (gen.BillingRunningCharge, error) {
	for day := nextMidnight(c.PostedThroughAt); day.Before(through); day = nextMidnight(day) {
		var err error
		if c, err = s.postRunningChargeOnce(ctx, q, c, day, false); err != nil {
			return c, err
		}
	}
	return s.postRunningChargeOnce(ctx, q, c, through, stop)
}

// nextMidnight is the first 00:00 UTC after t.
func nextMidnight(t time.Time) time.Time { return t.UTC().Truncate(24 * time.Hour).Add(24 * time.Hour) }

// postRunningChargeOnce is one posting. The row advances before the ledger
// write, so the held total no longer counts what is being posted.
func (s *MoneyService) postRunningChargeOnce(ctx context.Context, q *gen.Queries, c gen.BillingRunningCharge, through time.Time, stop bool) (gen.BillingRunningCharge, error) {
	total, err := RunningChargeTotal(c.RatePerHour, c.StartedAt, through)
	if err != nil {
		return c, err
	}
	amount := total - c.PostedAmount
	if amount < 0 {
		return c, fmt.Errorf("running charge %s: posting through %s is behind its posted amount", c.ID, through)
	}
	row, err := q.PostRunningCharge(ctx, gen.PostRunningChargeParams{
		MerchantID: c.MerchantID, ID: c.ID, Through: through, PostedAmount: total, Stop: stop, Now: instant(s.now()),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return c, fmt.Errorf("running charge %s changed under the customer lock", c.ID)
	}
	if err != nil {
		return c, err
	}
	if amount == 0 {
		return row, nil
	}
	posting := s.WithDatabase(s.db)
	posting.posting = &runningChargePosting{through: through, usagePosted: s.usagePosted, from: c.PostedThroughAt}
	if c.Resource != nil {
		posting.posting.resource = *c.Resource
	}
	payer := identity.CustomerID(c.CustomerID)
	key := MustIdempotencyKey(OpCapture, runningChargeSource, c.ID.String()+"/"+through.UTC().Format(runningChargeInstantLayout))
	_, _, applied, err := posting.spendBalanceThenOwedTx(ctx, q, payer, payer.UUID().String(), c.Currency, key, amount, true)
	if err != nil {
		return row, err
	}
	if !applied {
		return row, fmt.Errorf("running charge %s: the ledger already held its posting through %s", c.ID, through)
	}
	return row, nil
}

// postRunningChargesThrough is credits.PostRunningCharges: it posts every
// running charge of the customer in the currency through an instant, under the
// customer lock, on the caller's clock, once the customer's usage before that
// instant posted.
func postRunningChargesThrough(ctx context.Context, q *gen.Queries, merchantID, customer uuid.UUID, currency string, through, now time.Time) (bool, error) {
	args := gen.ListRunningChargesByCustomerParams{MerchantID: merchantID, CustomerID: customer, Currency: currency}
	charges, err := q.ListRunningChargesByCustomer(ctx, args)
	if err != nil || len(charges) == 0 {
		return false, err
	}
	if _, bound := merchant.FromContext(ctx); !bound {
		ctx = merchant.WithID(ctx, billing.MerchantID(merchantID))
	}
	svc := &MoneyService{db: db.TxOf(q), clock: clockwork.NewFakeClockAt(now), usagePosted: true}
	payer := identity.CustomerID(customer)
	if _, err := svc.lockBalance(ctx, q, payer, customer.String(), currency); err != nil {
		return false, err
	}
	if charges, err = q.ListRunningChargesByCustomer(ctx, args); err != nil {
		return false, err
	}
	through = instant(through)
	posted := false
	for _, c := range charges {
		if !through.After(c.PostedThroughAt) {
			continue
		}
		row, err := svc.postRunningCharge(ctx, q, c, through, false)
		if err != nil {
			return posted, err
		}
		posted = posted || row.PostedAmount > c.PostedAmount
	}
	return posted, nil
}

// settleRunningCharges posts what the customer's usage and then running
// charges have accrued through now, in the caller's transaction: a read that
// follows sees what its credit lots hold once that spend is in the ledger.
func (s *MoneyService) settleRunningCharges(ctx context.Context, q *gen.Queries, merchantID, customer uuid.UUID, currency string) error {
	now := s.now()
	if _, err := s.postUsageTx(ctx, q, merchantID, customer, currency, now, usagePostingMovement); err != nil {
		return err
	}
	_, err := postRunningChargesThrough(ctx, q, merchantID, customer, currency, now, now)
	return err
}
