package money

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/intents"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/internal/modules/mandates"
	"github.com/open-rails/openrails/internal/modules/paymentmethods"
	"github.com/open-rails/openrails/internal/modules/payments/charge"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
)

var ErrCustomerSessionRequired = errors.New("verified customer session required")

// ErrSubscriptionPaymentMethodMissing refuses a renewal with no card to
// charge: none of its own, and no customer default for its currency.
var ErrSubscriptionPaymentMethodMissing = errors.New("subscription has no card of its own and its customer no default for its currency")

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
	q := d.Gen(ctx)
	// The card it charges: its own, else the default it follows.
	charged, err := subscriptions.PaymentMethodOf(ctx, q, sub)
	if err != nil {
		return readFailure(err)
	}
	if charged == nil {
		return unusable(ErrSubscriptionPaymentMethodMissing)
	}
	observed, err := q.GetPaymentMethodByID(ctx, gen.GetPaymentMethodByIDParams{MerchantID: sub.MerchantID, ID: *charged})
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
	method, err = q.GetPaymentMethodForShare(ctx, gen.GetPaymentMethodForShareParams{MerchantID: sub.MerchantID, ID: *charged})
	if err != nil {
		return readFailure(err)
	}
	if err := charge.FreezeInstrument(observed, sub.PspID).Matches(method); err != nil {
		return unusable(err)
	}
	if method.CustomerID != sub.CustomerID || !charge.ChargeableOn(method, sub.PspID) || method.Rail != string(sub.Rail) || !paymentmethods.Chargeable(method) {
		return unusable(errors.New("engine recurring method is not qualified for this obligation"))
	}
	configuration := merchants.Of(s.db)
	if configuration == nil {
		return readFailure(errors.New("merchant configuration is unavailable"))
	}
	agreement, err := mandates.SubscriptionAgreement(ctx, q, sub.MerchantID, sub.CustomerID, sub.ID, method.ID)
	if err != nil {
		if errors.Is(err, mandates.ErrMissing) || errors.Is(err, mandates.ErrNotActive) {
			return unusable(err)
		}
		return readFailure(err)
	}
	psp := sub.PspID
	if method.Custodian == models.CustodianPlatform {
		psp, err = platformRenewalAccount(ctx, configuration, method, agreement)
		if err != nil {
			return unsupported(err)
		}
	} else if agreement.PspID != psp {
		return unusable(mandates.ErrMissing)
	}
	account, ok, err := configuration.PSPScopeByID(ctx, billing.MerchantID(sub.MerchantID), psp)
	if err != nil {
		return readFailure(err)
	}
	if ok && !account.Declared {
		if method.Custodian == models.CustodianPlatform {
			return unsupported(account.ErrUndeclared())
		}
		return unusable(account.ErrUndeclared())
	}
	if !ok || account.Archived {
		if method.Custodian == models.CustodianPlatform {
			return unsupported(errors.New("renewal account is unavailable"))
		}
		return unusable(errors.New("archived account cannot admit a new engine renewal"))
	}
	if method.CustodianID != nil {
		custodian, ok, err := configuration.CustodianScopeByID(ctx, billing.MerchantID(sub.MerchantID), *method.CustodianID)
		if err != nil {
			return readFailure(err)
		}
		if !ok || custodian.Archived {
			return unusable(errors.New("archived custodian cannot admit a new engine renewal"))
		}
	}
	binding, err = engineCollectionBinding(ctx, q, configuration, method, psp, s.hyperSwitchDeployment)
	if err != nil {
		return unsupported(err)
	}
	instrument = charge.FreezeInstrument(method, psp)
	if err := charge.ValidateEngineInstrument(method.Rail, instrument, engineHyperSwitchPointer(method.Custodian, binding)); err != nil {
		return unusable(err)
	}
	instrument.Mandate = mandates.Lineage(agreement)
	return method, instrument, binding, nil
}
