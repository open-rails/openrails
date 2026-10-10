package payments

import (
	"github.com/open-rails/openrails/internal/merchant"

	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jonboulle/clockwork"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/modules/mandates"
	"github.com/open-rails/openrails/internal/modules/paymentmethods"
)

// MirrorAttachedStripePaymentMethod fetches the instrument before mirroring it.
// A stale attached event therefore cannot revive a method Stripe now reports as
// detached. A method already mirrored takes the card Stripe now holds as a
// version from source under eventRef.
func MirrorAttachedStripePaymentMethod(
	ctx context.Context,
	database *db.DB,
	customers *RailCustomerService,
	clock clockwork.Clock,
	reader StripePaymentStateReader,
	paymentMethodID string,
	source paymentmethods.CardSource,
	eventRef string,
) (*models.PaymentMethod, paymentmethods.CardLifecycle, error) {
	if reader == nil {
		return nil, paymentmethods.CardLifecycle{}, errors.New("stripe payment state reader is not configured")
	}
	truth, err := reader.PaymentMethod(ctx, paymentMethodID)
	if err != nil {
		return nil, paymentmethods.CardLifecycle{}, err
	}
	if truth == nil || strings.TrimSpace(truth.CustomerID) == "" || truth.Card == nil {
		return nil, paymentmethods.CardLifecycle{}, nil
	}
	pm, created, err := UpsertStripeCardForCustomer(ctx, database, customers, clock, truth.CustomerID, truth)
	if err != nil || pm == nil || created {
		return pm, paymentmethods.CardLifecycle{}, err
	}
	life, err := ObserveStripeCard(ctx, database, clock, pm.ID, truth, source, eventRef)
	if err != nil {
		return nil, life, err
	}
	pm, err = models.PaymentMethodFromGen(life.Method)
	return pm, life, err
}

// ObserveStripeCard applies the card Stripe holds for a mirrored method
// (#1168). Stripe reissues under the same pm_ and reports a brand change the
// same way, so brand and fingerprint are compared on every observation.
func ObserveStripeCard(ctx context.Context, database *db.DB, clock clockwork.Clock, methodID uuid.UUID, truth *StripePaymentMethodState, source paymentmethods.CardSource, eventRef string) (paymentmethods.CardLifecycle, error) {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return paymentmethods.CardLifecycle{}, err
	}
	if truth == nil || truth.Card == nil {
		return paymentmethods.CardLifecycle{}, errors.New("stripe card observation carries no card")
	}
	now := time.Now().UTC()
	if clock != nil {
		now = clock.Now().UTC()
	}
	if strings.TrimSpace(eventRef) == "" {
		eventRef = "read:" + now.Format(time.RFC3339Nano)
	}
	return paymentmethods.ApplyCardLifecycle(ctx, database.Gen(ctx), paymentmethods.CardEvent{
		MerchantID: mid.UUID(), PaymentMethodID: methodID, Source: source, EventRef: eventRef, At: now,
		Card: paymentmethods.Card{Card: *truth.Card, Fingerprint: truth.Fingerprint},
	})
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
	queryMerchant, queryScopeErr := merchant.Require(ctx)
	if queryScopeErr != nil {
		return queryScopeErr
	}

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
			local, _, err := UpsertStripeCardForCustomer(ctx, database, customers, clock, state.CustomerID, method)
			if err != nil {
				return err
			}
			if local != nil && local.Chargeable() {
				id := local.ID
				localMethodID = &id
			}
		}
		if _, err := q.SetStripeSubscriptionPaymentMethod(ctx, gen.SetStripeSubscriptionPaymentMethodParams{MerchantID: queryMerchant.UUID(),
			PaymentMethodID:    localMethodID,
			PspID:              pspID,
			RailSubscriptionID: railSubscriptionID,
		}); err != nil {
			return fmt.Errorf("set stripe subscription %s payment method: %w", railSubscriptionID, err)
		}
	}
	return nil
}

// RemoveDetachedStripePaymentMethod removes a detached method, keeping its row
// as evidence: its mandates end and every exact-PSP subscription link that
// could otherwise charge it is cleared.
func RemoveDetachedStripePaymentMethod(ctx context.Context, database *db.DB, paymentMethodID string) (*models.PaymentMethod, error) {
	queryMerchant, queryScopeErr := merchant.Require(ctx)
	if queryScopeErr != nil {
		return nil, queryScopeErr
	}

	if database == nil {
		return nil, errors.New("stripe payment state database is not configured")
	}
	pspID, err := db.RequirePSPID(ctx)
	if err != nil {
		return nil, fmt.Errorf("remove stripe payment method: %w", err)
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
	if _, err := q.RemoveStripePaymentMethodByRef(ctx, gen.RemoveStripePaymentMethodByRefParams{MerchantID: queryMerchant.UUID(),
		PspID:         pspID,
		RailMethodRef: paymentMethodID,
	}); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("remove detached stripe payment method: %w", err)
	}
	if _, err := mandates.EndForPaymentMethod(ctx, q, queryMerchant.UUID(), method.ID, mandates.EndPaymentMethodRemoved, time.Time{}); err != nil {
		return nil, fmt.Errorf("end detached stripe payment method mandates: %w", err)
	}
	if _, err := q.ClearStripePaymentMethodSubscriptions(ctx, gen.ClearStripePaymentMethodSubscriptionsParams{MerchantID: queryMerchant.UUID(),
		PspID:           pspID,
		PaymentMethodID: method.ID,
	}); err != nil {
		return nil, fmt.Errorf("clear detached stripe payment method links: %w", err)
	}
	method.Status = paymentmethods.StatusRemoved
	return method, nil
}
