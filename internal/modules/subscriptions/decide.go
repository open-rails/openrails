package subscriptions

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/open-rails/openrails/internal/billing/lifecycle"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/models"
)

// Premise is what a caller decided on, re-checked on the locked row.
type Premise func(ctx context.Context, d *db.DB, sub *models.Subscription) (bool, error)

// Decide applies one lifecycle event to a subscription under its row lock,
// only while premise holds on the locked row, and carries out the effects in
// the same transaction. A row whose premise no longer holds, or that moved,
// is left alone and reports false: the next pass decides again.
func (s *SubscriptionLifecycleService) Decide(ctx context.Context, d *db.DB, id uuid.UUID, ev lifecycle.Event, premise Premise) (bool, error) {
	now := s.now()
	var notices []*models.NotificationQueue
	applied := false
	err := d.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		txdb := d.NewWithPgxTx(tx)
		repo := NewSubscriptionRepo(txdb)
		sub, err := repo.GetByIDForUpdate(ctx, id)
		if err != nil {
			return fmt.Errorf("decide %s: lock %s: %w", lifecycle.Name(ev), id, err)
		}
		if ok, err := premise(ctx, txdb, sub); err != nil || !ok {
			return err
		}
		effects, err := Transition(sub, ev, now)
		if err != nil {
			return fmt.Errorf("decide %s on %s: %w", lifecycle.Name(ev), id, err)
		}
		if !sub.LifecycleChanged() && len(effects) == 0 {
			return nil
		}
		if notices, err = s.ApplyEffects(ctx, txdb, sub, effects, now, EffectOptions{}); err != nil {
			return err
		}
		if err := repo.UpdateAt(ctx, sub, now); err != nil {
			return err
		}
		applied = true
		return nil
	})
	if errors.Is(err, ErrSubscriptionMoved) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	s.DispatchNotifications(ctx, notices)
	return applied, nil
}
