package app

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/integrations/nmi"
	"github.com/open-rails/openrails/internal/intents"
	"github.com/open-rails/openrails/internal/modules/paymentmethods"
	"github.com/open-rails/openrails/internal/modules/payments/rails/nmidirect"
	"github.com/open-rails/openrails/internal/modules/payments/rails/nmiproxy"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/internal/shared/apperr"
	"github.com/open-rails/openrails/internal/writeposture"
)

// RecurringVerifier runs the NMI verification a card move needs when the
// card has no recurring lineage: a decline is the card's refusal.
func (rt *Runtime) RecurringVerifier(merchantID uuid.UUID, origin intents.Origin) subscriptions.RecurringVerifier {
	return func(ctx context.Context, psp uuid.UUID, method gen.BillingPaymentMethod, order string) (string, error) {
		if blocked, reason := intents.GateExecution(ctx, writeposture.View{Config: rt.Config, DB: rt.DB}, merchantID, origin); blocked {
			return "", apperr.New(http.StatusServiceUnavailable, billing.CodeServiceUnavailable, "the recurring verification cannot be sent: "+reason)
		}
		var ref string
		var err error
		if models.BasisTheoryCustody(method.Custodian) {
			resolver, ok := rt.CollectionResolver.(interface {
				ResolveBasisTheoryNMICharge(context.Context, gen.BillingPaymentMethod, uuid.UUID) (*nmiproxy.Charger, error)
			})
			if !ok {
				return "", errors.New("platform card verification is unavailable")
			}
			proxy, resolveErr := resolver.ResolveBasisTheoryNMICharge(ctx, method, psp)
			if resolveErr != nil {
				return "", resolveErr
			}
			ref, err = proxy.VerifyRecurring(ctx, order)
		} else {
			client, ok, resolveErr := rt.CollectionResolver.ResolveNMIClient(ctx, merchantID, &psp)
			if resolveErr != nil || !ok {
				return "", fmt.Errorf("resolve the card's NMI account: %w", resolveErr)
			}
			ref, err = client.VerifyStoredCredential(ctx, models.DerefStr(method.RailCustomerRef), models.DerefStr(method.RailMethodRef), order, true)
		}
		var refusal *nmi.CustomerVaultError
		if errors.As(err, &refusal) && !nmi.UncertainResponseCode(refusal.ResponseCode) {
			return "", &paymentmethods.PaymentMethodError{Err: err, LocalizationID: nmidirect.FailureCode(refusal), Rail: "nmi"}
		}
		return ref, err
	}
}
