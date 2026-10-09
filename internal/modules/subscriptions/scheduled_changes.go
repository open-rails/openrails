package subscriptions

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/shared/apperr"
)

// A subscription's one pending change (Subscription.scheduled_change): a
// price, a seat count, or both, applied at its first renewal on or after
// EffectiveAt. Every writer holds the subscription's row lock, the same lock
// renewal admission and completion take.

var (
	ErrChangeAlreadyScheduled  = apperr.New(http.StatusConflict, billing.CodeScheduledChangeExists, "the subscription already has a scheduled change")
	ErrScheduledChangeNotFound = apperr.New(http.StatusNotFound, billing.CodeScheduledChangeNotFound, "the subscription has no scheduled change")
	// ErrScheduledChangeHeldByProvider refuses removing a change a provider
	// already bills (a provider-owned subscription's schedule was moved).
	ErrScheduledChangeHeldByProvider = apperr.New(http.StatusConflict, billing.CodeScheduledChangeHeldByProvider, "the provider already bills this change; change it back at the provider")
	// errChangeNotScheduled: a status-predicated transition lost to another.
	errChangeNotScheduled = errors.New("scheduled change is no longer scheduled")
)

// NewScheduledChange is one change to schedule.
type NewScheduledChange struct {
	// PriceID is the price billed from the change on; a seat change passes
	// the subscription's own.
	PriceID uuid.UUID
	// Quantity is the seats from then on; nil keeps the subscription's.
	Quantity *int
	// EffectiveAt zero is now: the next renewal.
	EffectiveAt             time.Time
	Source                  billing.ScheduledChangeSource
	PriceMigrationID        *uuid.UUID
	AcknowledgedShortNotice bool
}

func requireTx(d *db.DB) error {
	if d == nil || d.Pool() != nil {
		return errors.New("scheduled changes are written in the caller's transaction")
	}
	return nil
}

// ScheduleChange records sub's pending change. d is the caller's transaction,
// in which sub was locked (GetByIDForUpdate). It refuses
// ErrChangeAlreadyScheduled while another is pending and
// ErrRebillTermsCommitted while an accepted renewal owns the terms.
func ScheduleChange(ctx context.Context, d *db.DB, sub *models.Subscription, c NewScheduledChange, now time.Time) (*models.ScheduledChange, error) {
	if err := requireTx(d); err != nil {
		return nil, err
	}
	if err := RefuseOwnedRebillTerms(ctx, d, sub); err != nil {
		return nil, err
	}
	return insertChange(ctx, d, sub, c, now)
}

// RecordProviderChange records a tier change the provider already holds (its
// schedule was moved when the change was accepted), in the caller's
// transaction on the locked subscription.
func RecordProviderChange(ctx context.Context, d *db.DB, sub *models.Subscription, priceID uuid.UUID, now time.Time) (*models.ScheduledChange, error) {
	return insertChange(ctx, d, sub, NewScheduledChange{PriceID: priceID, Source: billing.ScheduledChangeChange}, now)
}

// insertChange records a change whose provider side was already decided
// (an accepted in-place tier change): no rebill-ownership refusal.
func insertChange(ctx context.Context, d *db.DB, sub *models.Subscription, c NewScheduledChange, now time.Time) (*models.ScheduledChange, error) {
	if err := requireTx(d); err != nil {
		return nil, err
	}
	if c.PriceID == uuid.Nil || (c.Source == billing.ScheduledChangeMigration) != (c.PriceMigrationID != nil) ||
		(c.Source != billing.ScheduledChangeChange && c.Source != billing.ScheduledChangeMigration) || (c.Quantity != nil && *c.Quantity < 1) {
		return nil, errors.New("scheduled change is incomplete")
	}
	pending, err := PendingChange(ctx, d, sub.ID)
	if err != nil {
		return nil, err
	}
	if pending != nil {
		return nil, ErrChangeAlreadyScheduled
	}
	effective := c.EffectiveAt
	if effective.IsZero() {
		effective = now
	}
	var quantity *int32
	if c.Quantity != nil {
		q := int32(*c.Quantity) // #nosec G115 -- bounded by the seat bounds
		quantity = &q
	}
	row, err := d.Gen(ctx).CreateScheduledChange(ctx, gen.CreateScheduledChangeParams{
		MerchantID: sub.MerchantID, SubscriptionID: sub.ID, FromPriceID: sub.PriceID, PriceID: c.PriceID,
		Quantity: quantity, EffectiveAt: effective.UTC(), Source: string(c.Source), PriceMigrationID: c.PriceMigrationID,
		AcknowledgedShortNotice: c.AcknowledgedShortNotice,
	})
	if db.IsUniqueViolation(err) {
		return nil, ErrChangeAlreadyScheduled
	}
	if err != nil {
		return nil, err
	}
	return models.ScheduledChangeFromGen(row), nil
}

// PendingChange is the subscription's scheduled change, or nil.
func PendingChange(ctx context.Context, d *db.DB, subscriptionID uuid.UUID) (*models.ScheduledChange, error) {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	row, err := d.Gen(ctx).GetPendingScheduledChange(ctx, gen.GetPendingScheduledChangeParams{MerchantID: mid.UUID(), SubscriptionID: subscriptionID})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return models.ScheduledChangeFromGen(row), nil
}

// PendingChanges are the scheduled changes of several subscriptions, by
// subscription.
func PendingChanges(ctx context.Context, d *db.DB, subscriptionIDs []uuid.UUID) (map[uuid.UUID]*models.ScheduledChange, error) {
	out := map[uuid.UUID]*models.ScheduledChange{}
	if len(subscriptionIDs) == 0 {
		return out, nil
	}
	mid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := d.Gen(ctx).ListPendingScheduledChanges(ctx, gen.ListPendingScheduledChangesParams{MerchantID: mid.UUID(), SubscriptionIds: subscriptionIDs})
	if err != nil {
		return nil, err
	}
	for _, c := range models.ScheduledChangesFromGen(rows) {
		out[c.SubscriptionID] = c
	}
	return out, nil
}

// LoadScheduledChanges fills each subscription's ScheduledChange for the wire.
func LoadScheduledChanges(ctx context.Context, d *db.DB, subs []*models.Subscription) error {
	ids := make([]uuid.UUID, 0, len(subs))
	for _, sub := range subs {
		if sub != nil {
			ids = append(ids, sub.ID)
		}
	}
	pending, err := PendingChanges(ctx, d, ids)
	if err != nil {
		return err
	}
	for _, sub := range subs {
		if sub != nil {
			sub.ScheduledChange = pending[sub.ID]
		}
	}
	return nil
}

// CancelPendingChange cancels sub's scheduled change in the caller's
// transaction (sub locked) and answers it, or ErrScheduledChangeNotFound.
func CancelPendingChange(ctx context.Context, d *db.DB, sub *models.Subscription, now time.Time) (*models.ScheduledChange, error) {
	if err := requireTx(d); err != nil {
		return nil, err
	}
	pending, err := PendingChange(ctx, d, sub.ID)
	if err != nil {
		return nil, err
	}
	if pending == nil {
		return nil, ErrScheduledChangeNotFound
	}
	if err := RefuseOwnedRebillTerms(ctx, d, sub); err != nil {
		return nil, err
	}
	if err := cancelChange(ctx, d, pending.ID, now); err != nil {
		return nil, err
	}
	return pending, nil
}

// ApplyChange marks a scheduled change applied once the subscription bills
// it: at the renewal it applies to, or when a provider moved it. Call in the
// transaction that moves the subscription.
func ApplyChange(ctx context.Context, d *db.DB, id uuid.UUID, now time.Time) error {
	if err := requireTx(d); err != nil {
		return err
	}
	mid, err := merchant.Require(ctx)
	if err != nil {
		return err
	}
	return changed(d.Gen(ctx).ApplyScheduledChange(ctx, gen.ApplyScheduledChangeParams{MerchantID: mid.UUID(), ID: id, Now: now.UTC()}))
}

// ProviderMovedPrice settles the pending change when the provider (Stripe)
// moved the subscription to priceID: the change asking for it is applied; a
// tier change asking for another price is dropped. A migration's move to
// another price stays.
func ProviderMovedPrice(ctx context.Context, d *db.DB, subscriptionID, priceID uuid.UUID, now time.Time) error {
	if err := requireTx(d); err != nil {
		return err
	}
	pending, err := PendingChange(ctx, d, subscriptionID)
	switch {
	case err != nil || pending == nil:
		return err
	case pending.PriceID == priceID:
		return ApplyChange(ctx, d, pending.ID, now)
	case pending.Source == billing.ScheduledChangeChange:
		return cancelChange(ctx, d, pending.ID, now)
	}
	return nil
}

// dropPendingChange cancels sub's scheduled change, if any, when the
// subscription itself moved on (superseded, changed tier in place).
func dropPendingChange(ctx context.Context, d *db.DB, subscriptionID uuid.UUID, now time.Time) error {
	pending, err := PendingChange(ctx, d, subscriptionID)
	if err != nil || pending == nil {
		return err
	}
	return cancelChange(ctx, d, pending.ID, now)
}

func cancelChange(ctx context.Context, d *db.DB, id uuid.UUID, now time.Time) error {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return err
	}
	return changed(d.Gen(ctx).CancelScheduledChange(ctx, gen.CancelScheduledChangeParams{MerchantID: mid.UUID(), ID: id, Now: now.UTC()}))
}

// changed maps a status-predicated transition that matched no row.
func changed(rows int64, err error) error {
	if err != nil {
		return err
	}
	if rows < 1 {
		return errChangeNotScheduled
	}
	return nil
}

// ScheduleChange locks the subscription, checks it still bills expectedPrice
// and records c (see the package function).
func (r *SubscriptionRepo) ScheduleChange(ctx context.Context, subscriptionID, expectedPrice uuid.UUID, c NewScheduledChange, now time.Time) (*models.ScheduledChange, error) {
	var out *models.ScheduledChange
	err := r.db.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		d := r.db.NewWithPgxTx(tx)
		sub, err := NewSubscriptionRepo(d).GetByIDForUpdate(ctx, subscriptionID)
		if err != nil {
			return err
		}
		if sub.PriceID != expectedPrice {
			return fmt.Errorf("%w: the subscription's price changed", ErrChangeAlreadyScheduled)
		}
		out, err = ScheduleChange(ctx, d, sub, c, now)
		return err
	})
	return out, err
}

// CancelScheduledChange removes the subscription's scheduled change. A change
// on a subscription its provider bills was already pushed to that provider
// and is refused (ErrScheduledChangeHeldByProvider).
func (r *SubscriptionRepo) CancelScheduledChange(ctx context.Context, subscriptionID uuid.UUID, now time.Time) error {
	return r.db.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		d := r.db.NewWithPgxTx(tx)
		sub, err := NewSubscriptionRepo(d).GetByIDForUpdate(ctx, subscriptionID)
		if err != nil {
			return err
		}
		pending, err := PendingChange(ctx, d, sub.ID)
		if err != nil {
			return err
		}
		if pending == nil {
			return ErrScheduledChangeNotFound
		}
		if sub.CollectionPolicy != models.CollectionPolicyEngine {
			return ErrScheduledChangeHeldByProvider
		}
		_, err = CancelPendingChange(ctx, d, sub, now)
		return err
	})
}
