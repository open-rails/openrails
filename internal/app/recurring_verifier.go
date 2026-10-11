package app

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/integrations/nmi"
	"github.com/open-rails/openrails/internal/intents"
	"github.com/open-rails/openrails/internal/modules/paymentmethods"
	"github.com/open-rails/openrails/internal/modules/payments/rails/nmidirect"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/internal/shared/apperr"
	"github.com/open-rails/openrails/internal/writeposture"
)

// RecurringVerifier runs the NMI verification a card move needs when the
// card has no recurring lineage: a decline is the card's refusal.
func (rt *Runtime) RecurringVerifier(merchantID uuid.UUID, origin intents.Origin) subscriptions.RecurringVerifier {
	return func(ctx context.Context, psp uuid.UUID, vault, billingID, order string) (string, error) {
		if blocked, reason := intents.GateExecution(ctx, writeposture.View{Config: rt.Config, DB: rt.DB}, merchantID, origin); blocked {
			return "", apperr.New(http.StatusServiceUnavailable, billing.CodeServiceUnavailable, "the recurring verification cannot be sent: "+reason)
		}
		client, ok, err := rt.CollectionResolver.ResolveNMIClient(ctx, merchantID, &psp)
		if err != nil || !ok {
			return "", fmt.Errorf("resolve the card's NMI account: %w", err)
		}
		ref, err := client.VerifyStoredCredential(ctx, vault, billingID, order, true)
		var refusal *nmi.CustomerVaultError
		if errors.As(err, &refusal) && !nmi.UncertainResponseCode(refusal.ResponseCode) {
			return "", &paymentmethods.PaymentMethodError{Err: err, LocalizationID: nmidirect.FailureCode(refusal), Rail: "nmi"}
		}
		return ref, err
	}
}
