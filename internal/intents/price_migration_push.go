package intents

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
)

// TypePriceMigrationPush carries one price migration move to its provider: a
// Stripe subscription schedule, or an NMI schedule's amount.
const TypePriceMigrationPush = "price_migration_push"

// PriceMigrationPushPayload is the stored payload.
type PriceMigrationPushPayload struct {
	ScheduledChangeID uuid.UUID `json:"scheduled_change_id"`
}

// PriceMigrationPushes records a move's push in the move's transaction
// (subscriptions.MigrationPushes).
type PriceMigrationPushes struct{}

func (PriceMigrationPushes) QueuePush(ctx context.Context, d *db.DB, sub *models.Subscription, change *models.ScheduledChange, notBefore time.Time) error {
	_, err := NewStore(d).Enqueue(ctx, EnqueueParams{
		MerchantID: change.MerchantID, Provider: string(sub.Rail), IntentType: TypePriceMigrationPush,
		SubscriptionID: &sub.ID, PriceID: &change.PriceID, PspID: sub.PspID,
		Payload:        PriceMigrationPushPayload{ScheduledChangeID: change.ID},
		IdempotencyKey: TypePriceMigrationPush + ":" + change.ID.String(),
		NextAttemptAt:  notBefore, Origin: OriginAdmin, OriginReason: "price migration",
	})
	return err
}

// PriceMigrationPushHandler runs a move's push until the move resolves. A
// push is idempotent at the provider, so a failed one is retried on the
// backoff; one the provider cannot take blocks the move.
type PriceMigrationPushHandler struct {
	DB         *db.DB
	Migrations *subscriptions.PriceMigrationService
	Policy     BackoffPolicy
}

func NewPriceMigrationPushHandler(d *db.DB, migrations *subscriptions.PriceMigrationService) *PriceMigrationPushHandler {
	return &PriceMigrationPushHandler{DB: d, Migrations: migrations, Policy: DefaultBackoff}
}

func (h *PriceMigrationPushHandler) Type() string { return TypePriceMigrationPush }
func (h *PriceMigrationPushHandler) Backoff(attempts int32) time.Duration {
	return h.Policy.Delay(attempts)
}

func (h *PriceMigrationPushHandler) move(intent gen.BillingProviderIntent) (uuid.UUID, error) {
	var p PriceMigrationPushPayload
	if err := json.Unmarshal(intent.Payload, &p); err != nil || p.ScheduledChangeID == uuid.Nil {
		return uuid.Nil, errors.New("price migration push names no move")
	}
	return p.ScheduledChangeID, nil
}

// CheckRelevance: a move that resolved needs no push.
func (h *PriceMigrationPushHandler) CheckRelevance(ctx context.Context, intent gen.BillingProviderIntent) (Relevance, error) {
	id, err := h.move(intent)
	if err != nil {
		return SupersededBy(err.Error()), nil
	}
	row, err := h.DB.Gen(ctx).GetScheduledChange(ctx, gen.GetScheduledChangeParams{MerchantID: intent.MerchantID, ID: id})
	if errors.Is(err, pgx.ErrNoRows) {
		return SupersededBy("the move is gone"), nil
	}
	if err != nil {
		return Relevance{}, err
	}
	if row.Status != string(models.ScheduledChangeScheduled) {
		return SupersededBy("the move is " + row.Status), nil
	}
	return StillRelevant(), nil
}

func (h *PriceMigrationPushHandler) Execute(ctx context.Context, intent gen.BillingProviderIntent) Outcome {
	if h.Migrations == nil {
		return Parked("price migrations are not wired")
	}
	id, err := h.move(intent)
	if err != nil {
		return Terminal(err.Error())
	}
	push, err := h.Migrations.PushMove(ctx, id)
	switch {
	case err != nil:
		return Retryable(err.Error())
	case !push.Until.IsZero():
		return Deferred("the move is pushed in the subscription's last period before it", push.Until)
	case push.Blocked != "":
		return Terminal(push.Blocked)
	}
	return Succeeded(map[string]any{"resolution": push.Resolution})
}

// Verify: Execute never answers ambiguous; a push is retried.
func (h *PriceMigrationPushHandler) Verify(context.Context, gen.BillingProviderIntent) Outcome {
	return Retryable("a price migration push is retried, never verified")
}
