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
	"github.com/open-rails/openrails/internal/modules/paymentmethods"
	"github.com/open-rails/openrails/internal/modules/payments/charge"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
)

var ErrCustomerSessionRequired = errors.New("verified customer session required")

// ErrSubscriptionPaymentMethodMissing refuses a renewal whose stored method is
// gone. Renewals charge only the method chosen at subscribe time (or replaced
// by the customer); they never fall back to the customer's current default.
var ErrSubscriptionPaymentMethodMissing = errors.New("subscription has no stored payment method; renewals never fall back to the default card")

// engineCollectionMethod checks the local instrument and account facts shared
// by admission and recovery readback. Call with the customer/subscription locked;
// the handle lock precedes the method lock, as it does in method retirement.
func (s *MoneyService) engineCollectionMethod(ctx context.Context, d *db.DB, sub *models.Subscription) (gen.OpenrailsPaymentMethod, charge.HyperSwitchBinding, error) {
	var method gen.OpenrailsPaymentMethod
	var binding charge.HyperSwitchBinding
	unsupported := func(err error) (gen.OpenrailsPaymentMethod, charge.HyperSwitchBinding, error) {
		return method, binding, fmt.Errorf("%w: %w", intents.ErrRebillUnsupported, err)
	}
	readFailure := func(err error) (gen.OpenrailsPaymentMethod, charge.HyperSwitchBinding, error) {
		if errors.Is(err, pgx.ErrNoRows) {
			return unsupported(err)
		}
		return method, binding, err
	}
	if sub.PaymentMethodID == nil {
		return unsupported(ErrSubscriptionPaymentMethodMissing)
	}
	q := d.Gen(ctx)
	observed, err := q.GetPaymentMethodByID(ctx, gen.GetPaymentMethodByIDParams{MerchantID: sub.MerchantID, ID: *sub.PaymentMethodID})
	if err != nil {
		return readFailure(err)
	}
	if observed.CustodianID != nil {
		handle := paymentmethods.CustodianHandle{Custodian: *observed.CustodianID, Method: observed.RailMethodRef}
		if err := paymentmethods.LockCustodianHandles(ctx, q, sub.MerchantID, handle); err != nil {
			return readFailure(err)
		}
		if err := paymentmethods.RequireCustodianHandleAvailable(ctx, q, sub.MerchantID, handle); err != nil {
			if errors.Is(err, paymentmethods.ErrPaymentMethodDeleteUnsafe) || errors.Is(err, paymentmethods.ErrPaymentMethodDeleteProcessing) {
				return unsupported(err)
			}
			return readFailure(err)
		}
	}
	method, err = q.GetPaymentMethodForShare(ctx, gen.GetPaymentMethodForShareParams{MerchantID: sub.MerchantID, ID: *sub.PaymentMethodID})
	if err != nil {
		return readFailure(err)
	}
	if err := charge.FreezeInstrument(observed).Matches(method, charge.AgreementRecurring); err != nil {
		return unsupported(err)
	}
	if method.CustomerID != sub.CustomerID || method.PspID != sub.PspID || method.Rail != string(sub.Rail) || method.ParkReason != "" {
		return unsupported(errors.New("engine recurring method is not qualified for this obligation"))
	}
	account, err := q.GetPSP(ctx, gen.GetPSPParams{MerchantID: sub.MerchantID, ID: method.PspID})
	if err != nil {
		return readFailure(err)
	}
	if account.Archived {
		return unsupported(errors.New("archived account cannot admit a new engine renewal"))
	}
	if method.CustodianID != nil {
		custodian, err := q.GetCustodian(ctx, gen.GetCustodianParams{MerchantID: sub.MerchantID, ID: *method.CustodianID})
		if err != nil {
			return readFailure(err)
		}
		if custodian.Archived {
			return unsupported(errors.New("archived custodian cannot admit a new engine renewal"))
		}
	}
	binding, err = engineCollectionBinding(ctx, q, method, s.hyperSwitchDeployment)
	if err != nil {
		return unsupported(err)
	}
	if err := charge.ValidateEngineInstrument(method.Rail, charge.FreezeInstrument(method), engineHyperSwitchPointer(method.Custodian, binding), true); err != nil {
		return unsupported(err)
	}
	return method, binding, nil
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
		if _, _, err := s.engineCollectionMethod(ctx, d, sub); err != nil {
			return err
		}
		_, err = intents.PrepareEngineRenewalTerms(ctx, d, sub, s.now())
		return err
	})
}
