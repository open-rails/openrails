package webhooks

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/intents"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/pkg/merchant"
)

type stripeEngineNotification struct {
	ID       string            `json:"id"`
	Metadata map[string]string `json:"metadata"`
}

// PaymentIntent events only wake an already accepted operation. The operation
// worker fetches and qualifies provider receipts before changing money/access.
func (s *StripeWebhookService) wakeStripeEngineOperation(ctx context.Context, raw json.RawMessage) error {
	var event stripeEngineNotification
	if err := json.Unmarshal(raw, &event); err != nil {
		return err
	}
	key := event.Metadata["openrails_engine_operation"]
	if key == "" {
		return nil // A native Stripe invoice or another application's payment.
	}
	id, err := uuid.Parse(key)
	if err != nil || id == uuid.Nil || s.DB == nil {
		return errors.New("Stripe engine notification has no valid local operation")
	}
	mid, err := merchant.Require(ctx)
	if err != nil {
		return err
	}
	psp, err := db.RequirePSPID(ctx)
	if err != nil {
		return err
	}
	store := intents.NewStore(s.DB)
	operation, err := store.Get(ctx, id)
	if err != nil {
		return fmt.Errorf("load Stripe engine notification operation: %w", err)
	}
	if operation.MerchantID != mid.UUID() || operation.PspID == nil || *operation.PspID != psp {
		return errors.New("Stripe engine notification account differs from accepted operation")
	}
	account, err := s.DB.Gen(ctx).GetPSP(ctx, gen.GetPSPParams{MerchantID: mid.UUID(), ID: psp})
	if err != nil {
		return err
	}
	if err := qualifyStripeEngineNotification(operation, event, raw, account.Environment); err != nil {
		return err
	}
	switch operation.Status {
	case intents.StatusInFlight, intents.StatusUnknownNeedsVerify:
		return store.WakeOperation(ctx, operation.ID)
	case intents.StatusSucceeded, intents.StatusFailedTerminal, intents.StatusSuperseded, intents.StatusExpired:
		return nil
	default:
		// A notification cannot cause first submission or release a parked
		// operation. Its original River job retains the accepted schedule.
		return nil
	}
}

func qualifyStripeEngineNotification(operation gen.OpenrailsRailIntent, event stripeEngineNotification, raw json.RawMessage, environment string) error {
	params, err := intents.StripeEngineParams(operation)
	if err != nil {
		return err
	}
	if err := subscriptions.ValidateStripeEnginePaymentNotification(raw, params, environment); err != nil {
		return err
	}
	if candidate, found, err := intents.LoadCollectionCandidate(operation); err != nil {
		return err
	} else if found && candidate.TransactionID != event.ID {
		return errors.New("Stripe engine notification differs from retained payment identity")
	}
	if retained := intents.EvidenceString(operation, "stripe_payment_intent_id"); retained != "" && retained != event.ID {
		return errors.New("Stripe engine notification differs from authentication payment identity")
	}
	return nil
}
