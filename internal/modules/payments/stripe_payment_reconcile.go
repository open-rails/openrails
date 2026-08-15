package payments

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jonboulle/clockwork"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/modules/paymentmethods"
)

// StripeDetachedParkReason marks a provider-detached Stripe mirror as unusable.
const StripeDetachedParkReason = "stripe_payment_method_detached"

// MirrorAttachedStripePaymentMethod fetches the instrument before mirroring it.
// A stale attached event therefore cannot revive a method Stripe now reports as
// detached.
func MirrorAttachedStripePaymentMethod(
	ctx context.Context,
	database *db.DB,
	customers *RailCustomerService,
	clock clockwork.Clock,
	reader StripePaymentStateReader,
	paymentMethodID string,
) (*models.PaymentMethod, error) {
	if reader == nil {
		return nil, errors.New("stripe payment state reader is not configured")
	}
	truth, err := reader.PaymentMethod(ctx, paymentMethodID)
	if err != nil {
		return nil, err
	}
	if truth == nil || strings.TrimSpace(truth.CustomerID) == "" || truth.Card == nil {
		return nil, nil
	}
	return UpsertStripeCardForCustomer(
		ctx,
		database,
		customers,
		clock,
		truth.CustomerID,
		truth.ID,
		truth.ID,
		truth.Card,
	)
}

// ConvergeStripeCustomerPaymentState applies Stripe's current subscription
// default selections. It deliberately does not treat event ordering as truth.
func ConvergeStripeCustomerPaymentState(
	ctx context.Context,
	database *db.DB,
	customers *RailCustomerService,
	clock clockwork.Clock,
	reader StripePaymentStateReader,
	customerID string,
) error {
	if database == nil || customers == nil {
		return errors.New("stripe payment state dependencies are not configured")
	}
	if reader == nil {
		return errors.New("stripe payment state reader is not configured")
	}
	state, err := reader.CustomerPaymentState(ctx, customerID)
	if err != nil {
		return err
	}
	if state == nil {
		return nil
	}
	pspID, err := db.RequirePSPID(ctx)
	if err != nil {
		return fmt.Errorf("converge stripe customer payment state: %w", err)
	}

	q := database.Gen(ctx)
	for _, remote := range state.Subscriptions {
		railSubscriptionID := strings.TrimSpace(remote.SubscriptionID)
		if railSubscriptionID == "" {
			continue
		}
		var localMethodID *uuid.UUID
		if method := remote.PaymentMethod; method != nil && method.Card != nil {
			local, err := UpsertStripeCardForCustomer(
				ctx,
				database,
				customers,
				clock,
				state.CustomerID,
				method.ID,
				method.ID,
				method.Card,
			)
			if err != nil {
				return err
			}
			if local != nil && local.ParkReason == "" {
				id := local.ID
				localMethodID = &id
			}
		}
		if _, err := q.SetStripeSubscriptionPaymentMethod(ctx, gen.SetStripeSubscriptionPaymentMethodParams{
			PaymentMethodID:    localMethodID,
			PspID:              pspID,
			RailSubscriptionID: railSubscriptionID,
		}); err != nil {
			return fmt.Errorf("set stripe subscription %s payment method: %w", railSubscriptionID, err)
		}
	}
	return nil
}

// ParkDetachedStripePaymentMethod preserves a detached method as evidence and
// clears every exact-PSP subscription link that could otherwise charge it.
func ParkDetachedStripePaymentMethod(ctx context.Context, database *db.DB, paymentMethodID string) (*models.PaymentMethod, error) {
	if database == nil {
		return nil, errors.New("stripe payment state database is not configured")
	}
	pspID, err := db.RequirePSPID(ctx)
	if err != nil {
		return nil, fmt.Errorf("park stripe payment method: %w", err)
	}
	paymentMethodID = strings.TrimSpace(paymentMethodID)
	if paymentMethodID == "" {
		return nil, nil
	}
	repo := paymentmethods.NewPaymentMethodRepo(database)
	method, err := repo.GetByRailMethodRefForPSP(ctx, string(models.RailStripe), pspID, paymentMethodID)
	if errors.Is(err, paymentmethods.ErrPaymentMethodNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("load detached stripe payment method: %w", err)
	}
	q := database.Gen(ctx)
	if _, err := q.ParkStripePaymentMethodByRef(ctx, gen.ParkStripePaymentMethodByRefParams{
		ParkReason:    StripeDetachedParkReason,
		PspID:         pspID,
		RailMethodRef: paymentMethodID,
	}); err != nil {
		return nil, fmt.Errorf("park detached stripe payment method: %w", err)
	}
	if _, err := q.ClearStripePaymentMethodSubscriptions(ctx, gen.ClearStripePaymentMethodSubscriptionsParams{
		PspID:           pspID,
		PaymentMethodID: method.ID,
	}); err != nil {
		return nil, fmt.Errorf("clear detached stripe payment method links: %w", err)
	}
	method.ParkReason = StripeDetachedParkReason
	return method, nil
}
