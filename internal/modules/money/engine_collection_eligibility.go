package money

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/intents"
	"github.com/open-rails/openrails/internal/modules/mandates"
	"github.com/open-rails/openrails/internal/modules/paymentmethods"
	"github.com/open-rails/openrails/internal/modules/payments/charge"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
)

var ErrCustomerSessionRequired = errors.New("verified customer session required")

// ErrSubscriptionPaymentMethodMissing refuses a renewal whose stored method is
// gone. Renewals charge only the method chosen at subscribe time (or replaced
// by the customer); they never fall back to the customer's current default.
var ErrSubscriptionPaymentMethodMissing = errors.New("subscription has no stored payment method; renewals never fall back to the default card")

// ErrEngineMethodUnusable is a renewal refusal only the member can fix: the
// stored method is gone, parked, being deleted, no longer qualified, or on an
// archived account. The due pass routes it to awaiting_method. Every other
// refusal (configuration, a read failure) is a stop the operator fixes.
var ErrEngineMethodUnusable = errors.New("the subscription's payment method cannot be charged; the member must add one")

// engineCollectionMethod checks the local instrument and account facts shared
// by admission and recovery readback. Call with the customer/subscription locked;
// the handle lock precedes the method lock, as it does in method retirement.
func (s *MoneyService) engineCollectionMethod(ctx context.Context, d *db.DB, sub *models.Subscription) (gen.BillingPaymentMethod, charge.FrozenInstrument, charge.HyperSwitchBinding, error) {
	var method gen.BillingPaymentMethod
	var instrument charge.FrozenInstrument
	var binding charge.HyperSwitchBinding
	unsupported := func(err error) (gen.BillingPaymentMethod, charge.FrozenInstrument, charge.HyperSwitchBinding, error) {
		return method, instrument, binding, fmt.Errorf("%w: %w", intents.ErrRebillUnsupported, err)
	}
	unusable := func(err error) (gen.BillingPaymentMethod, charge.FrozenInstrument, charge.HyperSwitchBinding, error) {
		return method, instrument, binding, fmt.Errorf("%w: %w: %w", intents.ErrRebillUnsupported, ErrEngineMethodUnusable, err)
	}
	readFailure := func(err error) (gen.BillingPaymentMethod, charge.FrozenInstrument, charge.HyperSwitchBinding, error) {
		if errors.Is(err, pgx.ErrNoRows) {
			return unusable(err)
		}
		return method, instrument, binding, err
	}
	if sub.PaymentMethodID == nil {
		return unusable(ErrSubscriptionPaymentMethodMissing)
	}
	q := d.Gen(ctx)
	observed, err := q.GetPaymentMethodByID(ctx, gen.GetPaymentMethodByIDParams{MerchantID: sub.MerchantID, ID: *sub.PaymentMethodID})
	if err != nil {
		return readFailure(err)
	}
	if observed.CustodianID != nil {
		handle := paymentmethods.CustodianHandle{Custodian: *observed.CustodianID, Method: models.DerefStr(observed.RailMethodRef)}
		if err := paymentmethods.LockCustodianHandles(ctx, q, sub.MerchantID, handle); err != nil {
			return readFailure(err)
		}
		if err := paymentmethods.RequireCustodianHandleAvailable(ctx, q, sub.MerchantID, handle); err != nil {
			if errors.Is(err, paymentmethods.ErrPaymentMethodDeleteUnsafe) || errors.Is(err, paymentmethods.ErrPaymentMethodDeleteProcessing) {
				return unusable(err)
			}
			return readFailure(err)
		}
	}
	method, err = q.GetPaymentMethodForShare(ctx, gen.GetPaymentMethodForShareParams{MerchantID: sub.MerchantID, ID: *sub.PaymentMethodID})
	if err != nil {
		return readFailure(err)
	}
	if err := charge.FreezeInstrument(observed, sub.PspID).Matches(method); err != nil {
		return unusable(err)
	}
	if method.CustomerID != sub.CustomerID || !charge.ChargeableOn(method, sub.PspID) || method.Rail != string(sub.Rail) || !paymentmethods.Chargeable(method) {
		return unusable(errors.New("engine recurring method is not qualified for this obligation"))
	}
	account, err := q.GetPSP(ctx, gen.GetPSPParams{MerchantID: sub.MerchantID, ID: sub.PspID})
	if err != nil {
		return readFailure(err)
	}
	if account.Archived {
		return unusable(errors.New("archived account cannot admit a new engine renewal"))
	}
	if method.CustodianID != nil {
		custodian, err := q.GetCustodian(ctx, gen.GetCustodianParams{MerchantID: sub.MerchantID, ID: *method.CustodianID})
		if err != nil {
			return readFailure(err)
		}
		if custodian.Archived {
			return unusable(errors.New("archived custodian cannot admit a new engine renewal"))
		}
	}
	binding, err = engineCollectionBinding(ctx, q, method, sub.PspID, s.hyperSwitchDeployment)
	if err != nil {
		return unsupported(err)
	}
	instrument = charge.FreezeInstrument(method, sub.PspID)
	if err := charge.ValidateEngineInstrument(method.Rail, instrument, engineHyperSwitchPointer(method.Custodian, binding)); err != nil {
		return unusable(err)
	}
	// A renewal runs only under the subscription's active recurring mandate on
	// this card and account; anything else waits for the member.
	if instrument.Mandate, err = mandates.ForSubscription(ctx, q, sub.MerchantID, sub.CustomerID, sub.ID, method.ID, sub.PspID); err != nil {
		if errors.Is(err, mandates.ErrMissing) || errors.Is(err, mandates.ErrNotActive) {
			return unusable(err)
		}
		return readFailure(err)
	}
	return method, instrument, binding, nil
}

// EngineCustomerRetryEligibility performs no writes or provider calls. Its short
// transaction uses admission's lock order and qualifies the accepted paid lineage
// against the current instrument, rather than promising eligibility from fields.
func (s *MoneyService) EngineCustomerRetryEligibility(ctx context.Context, observed *models.Subscription) error {
	return s.db.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		d := s.db.NewWithPgxTx(tx)
		if _, err := d.Gen(ctx).LockCustomerForSpend(ctx, gen.LockCustomerForSpendParams{MerchantID: observed.MerchantID, ID: observed.CustomerID}); err != nil {
			return err
		}
		sub, err := subscriptions.NewSubscriptionRepo(d).GetByIDForUpdate(ctx, observed.ID)
		if err != nil {
			return err
		}
		if sub.CustomerID != observed.CustomerID || sub.CollectionPolicy != models.CollectionPolicyEngine || sub.RailSubscriptionID != "" || !subscriptions.EngineCollectionDue(sub, s.now(), true) {
			return intents.ErrRebillNotRetryable
		}
		if _, _, _, err := s.engineCollectionMethod(ctx, d, sub); err != nil {
			return err
		}
		_, err = intents.PrepareEngineRenewalTerms(ctx, d, sub, s.now())
		return err
	})
}
