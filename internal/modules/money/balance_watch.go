package money

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/open-rails/openrails/billing"
	identity "github.com/open-rails/openrails/internal/billingidentity"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/merchant"
)

// A balance watch is the one stored instant per customer and currency with a
// running charge: when its balance is next looked at. Start and stop write
// it, a change in what the charges may draw marks it due (the ledger's
// ApplyIdempotent, an admission's hold), and the watcher job looks at the due
// ones. Nothing else is written while a charge runs.

// WatchBalanceInTx looks at one customer's balance under its lock: it settles
// lapsed credit lots, posts each running charge through the last 00:00 UTC,
// tells the host of a low or depleted balance once per crossing, and writes
// when to look next. A customer with no watch is left alone.
func (s *MoneyService) WatchBalanceInTx(ctx context.Context, txDB *db.DB, customer identity.CustomerID, currency string) error {
	if s == nil || txDB == nil {
		return fmt.Errorf("balance watch requires a bound transaction")
	}
	merchantID, err := merchant.Require(ctx)
	if err != nil {
		return err
	}
	txSvc := &MoneyService{db: txDB, clock: s.clock}
	q := txDB.Gen(ctx)
	if _, err := txSvc.lockBalance(ctx, q, customer, customer.UUID().String(), currency); err != nil {
		return err
	}
	key := gen.LockBalanceWatchParams{MerchantID: merchantID.UUID(), CustomerID: customer.UUID(), Currency: currency}
	watch, err := q.LockBalanceWatch(ctx, key)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	now := instant(txSvc.now())
	// Lots that lapsed are settled in the order they lapsed; each posting
	// stops at every 00:00 UTC on its way.
	if _, err := txSvc.grantLedger(q, merchantID.UUID()).ExpireLapsedBy(ctx, customer.UUID(), currency, now); err != nil {
		return err
	}
	charges, err := q.ListRunningChargesByCustomer(ctx, gen.ListRunningChargesByCustomerParams{MerchantID: key.MerchantID, CustomerID: key.CustomerID, Currency: currency})
	if err != nil {
		return err
	}
	day := now.Truncate(24 * time.Hour)
	for _, c := range charges {
		if c.PostedThroughAt.Before(day) {
			if _, err := txSvc.postRunningCharge(ctx, q, c, day, false); err != nil {
				return err
			}
		}
	}
	bal, err := txSvc.deriveBalance(ctx, q, merchantID.UUID(), customer.UUID(), currency)
	if err != nil {
		return err
	}
	out, running, err := txSvc.runOut(ctx, q, bal)
	if err != nil {
		return err
	}
	if !running {
		return q.DeleteBalanceWatch(ctx, gen.DeleteBalanceWatchParams{MerchantID: key.MerchantID, CustomerID: key.CustomerID, Currency: currency})
	}
	event := billing.BalanceHostEvent{
		CustomerID: billing.CustomerID(customer), Currency: currency, AvailableAmount: bal.Balance - bal.HeldBalance,
		OwedAmount: out.owed, AccrualRatePerHour: out.ratePerHour, AsOf: bal.AsOf, DepletesAt: out.depletesAt,
	}
	// Each notice is sent once per crossing and cleared when the balance is
	// back above its line.
	next := nextMidnight(bal.AsOf)
	lowNotifiedAt, depletedAt := watch.LowNotifiedAt, watch.DepletedAt
	switch {
	case !out.low:
		lowNotifiedAt = nil
		if out.lowAt != nil {
			next = earlier(next, *out.lowAt)
		}
	case lowNotifiedAt == nil:
		if err := enqueueBalanceEvent(ctx, q, merchantID.UUID(), billing.HostEventBalanceLow, event); err != nil {
			return err
		}
		lowNotifiedAt = &bal.AsOf
	}
	switch {
	case !out.depleted:
		depletedAt = nil
		next = earlier(next, out.depletesAt)
	case depletedAt == nil:
		if err := enqueueBalanceEvent(ctx, q, merchantID.UUID(), billing.HostEventBalanceDepleted, event); err != nil {
			return err
		}
		depletedAt = &bal.AsOf
	}
	return q.SetBalanceWatch(ctx, gen.SetBalanceWatchParams{
		MerchantID: key.MerchantID, CustomerID: key.CustomerID, Currency: key.Currency,
		NextCheckAt: next, DepletesAt: &out.depletesAt, LowNotifiedAt: lowNotifiedAt, DepletedAt: depletedAt, Now: bal.AsOf,
	})
}

// enqueueBalanceEvent writes one balance event to the host outbox, keyed by
// the instant its crossing was seen.
func enqueueBalanceEvent(ctx context.Context, q *gen.Queries, merchantID uuid.UUID, eventType billing.HostEventType, event billing.BalanceHostEvent) error {
	data, err := json.Marshal(event)
	if err != nil {
		return err
	}
	_, err = q.EnqueueHostLifecycleEvent(ctx, gen.EnqueueHostLifecycleEventParams{
		MerchantID: merchantID, EventType: string(eventType), SubjectType: "customer", SubjectID: event.CustomerID.UUID(),
		Currency: event.Currency, OccurredAt: event.AsOf, Data: data,
		DedupeKey: fmt.Sprintf("%s:%s:%s:%s", eventType, event.CustomerID, event.Currency, event.AsOf.Format(runningChargeInstantLayout)),
	})
	return err
}
