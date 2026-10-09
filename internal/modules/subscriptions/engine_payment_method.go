package subscriptions

import (
	"context"
	"errors"
	"net/http"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/modules/attempts"
	"github.com/open-rails/openrails/internal/modules/mandates"
	"github.com/open-rails/openrails/internal/modules/paymentmethods"
	"github.com/open-rails/openrails/internal/modules/payments/charge"
	"github.com/open-rails/openrails/internal/shared/apperr"
	"github.com/open-rails/openrails/internal/shared/uuidutil"
)

// RecurringVerifier runs a customer-present $0 verification declaring a
// recurring agreement on an NMI card of the subscription's account, returning
// NMI's transaction id.
type RecurringVerifier func(ctx context.Context, vault, billing, order string) (string, error)

// UpdateEnginePaymentMethod moves a subscription onto another saved card of
// its customer. Its recurring mandate moves with it: the new one cites the
// card's recurring lineage on the subscription's account, or a recurring
// verification run now when the card has none (NMI cards the PSP holds).
// It never changes schedule ownership or issues a provider schedule mutation.
// Accepted uncertain payments keep their frozen instrument until resolved.
func (s *SubscriptionLifecycleService) UpdateEnginePaymentMethod(ctx context.Context, subscriptionID, customer, methodID uuid.UUID, verify RecurringVerifier) error {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return err
	}
	var verified *charge.Mandate
	for {
		var needs *gen.BillingPaymentMethod
		err := s.DB.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
			var err error
			needs, err = s.moveEngineMethod(ctx, tx, mid.UUID(), subscriptionID, customer, methodID, verified)
			return err
		})
		if err != nil || needs == nil {
			return err
		}
		if verify == nil {
			return apperr.Conflictf("replacement card requires a qualified recurring agreement")
		}
		ref, err := verify(ctx, models.DerefStr(needs.RailCustomerRef), models.DerefStr(needs.RailMethodRef), "pmr-"+uuidutil.NewV7().String())
		if err != nil {
			return err
		}
		verified = &charge.Mandate{Kind: charge.AgreementRecurring, InitialTransactionID: ref}
	}
}

// moveEngineMethod returns the card when it needs a recurring verification
// first and verified is nil.
func (s *SubscriptionLifecycleService) moveEngineMethod(ctx context.Context, tx pgx.Tx, merchantID, subscriptionID, customer, methodID uuid.UUID, verified *charge.Mandate) (*gen.BillingPaymentMethod, error) {
	d := s.DB.NewWithPgxTx(tx)
	q := d.Gen(ctx)
	if _, err := q.LockCustomerForSpend(ctx, gen.LockCustomerForSpendParams{MerchantID: merchantID, ID: customer}); err != nil {
		return nil, err
	}
	sub, err := NewSubscriptionRepo(d).GetByIDForUpdate(ctx, subscriptionID)
	if err != nil {
		return nil, err
	}
	if sub.CustomerID != customer {
		return nil, pgx.ErrNoRows
	}
	if sub.CollectionPolicy != models.CollectionPolicyEngine || (sub.Rail != models.RailNMI && sub.Rail != models.RailStripe) || (sub.Status != models.StatusActive && sub.Status != models.StatusPastDue && sub.Status != models.StatusAwaitingMethod) {
		return nil, apperr.Conflictf("subscription cannot select an engine payment method")
	}
	if err := RefuseOwnedRebillTerms(ctx, d, sub); err != nil {
		return nil, err
	}
	observed, err := q.GetPaymentMethodByID(ctx, gen.GetPaymentMethodByIDParams{MerchantID: merchantID, ID: methodID})
	if err != nil {
		return nil, err
	}
	if observed.CustodianID != nil {
		handle := paymentmethods.CustodianHandle{Custodian: *observed.CustodianID, Method: models.DerefStr(observed.RailMethodRef)}
		if err := paymentmethods.LockCustodianHandles(ctx, q, merchantID, handle); err != nil {
			return nil, err
		}
		if err := paymentmethods.RequireCustodianHandleAvailable(ctx, q, merchantID, handle); err != nil {
			return nil, err
		}
	}
	method, err := q.GetPaymentMethodForShare(ctx, gen.GetPaymentMethodForShareParams{MerchantID: merchantID, ID: methodID})
	if err != nil {
		return nil, err
	}
	if method.CustomerID != customer {
		// Another customer's method is indistinguishable from a missing one.
		return nil, apperr.New(http.StatusNotFound, billing.CodeResourceNotFound, "payment method not found")
	}
	if !charge.ChargeableOn(method, sub.PspID) || method.Rail != string(sub.Rail) || method.ParkReason != nil || method.ChargeVia != "pan_proxy" {
		return nil, charge.ErrInstrumentChanged
	}
	if err := charge.FreezeInstrument(observed, sub.PspID).Matches(method); err != nil {
		return nil, err
	}
	psp, err := q.GetPSP(ctx, gen.GetPSPParams{MerchantID: merchantID, ID: sub.PspID})
	if err != nil {
		return nil, err
	}
	if psp.Archived || psp.Rail != method.Rail {
		return nil, apperr.Conflictf("payment account is archived")
	}
	var binding *charge.HyperSwitchBinding
	if method.Custodian == models.CustodianHyperSwitch {
		if s.Config == nil || s.Config.HyperSwitch == nil {
			return nil, errors.New("HyperSwitch deployment is unavailable")
		}
		frozen, err := charge.FreezeHyperSwitchBinding(ctx, q, method, sub.PspID, s.Config.HyperSwitch.APIBaseURL)
		if err != nil {
			return nil, err
		}
		binding = &frozen
		custodian, err := q.GetCustodian(ctx, gen.GetCustodianParams{MerchantID: merchantID, ID: *method.CustodianID})
		if err != nil {
			return nil, err
		}
		if custodian.Archived {
			return nil, apperr.Conflictf("payment custodian is archived")
		}
	}
	if err := charge.ValidateEngineInstrument(method.Rail, charge.FreezeInstrument(method, sub.PspID), binding); err != nil {
		return nil, apperr.Conflictf("replacement card requires a qualified recurring agreement")
	}
	lineage, err := mandates.Citable(ctx, q, merchantID, customer, methodID, sub.PspID, method.Rail, charge.AgreementRecurring)
	if err != nil {
		return nil, err
	}
	now := s.now()
	if lineage == nil {
		switch {
		case verified != nil:
			lineage = verified
			if err := attempts.Record(ctx, q, attempts.Attempt{MerchantID: merchantID, CustomerID: customer, PSPID: sub.PspID, Rail: method.Rail, Kind: attempts.Verify,
				Approved: true, TransactionID: verified.InitialTransactionID, PaymentMethodID: &methodID, SubscriptionID: &sub.ID, Step: "verify", At: now, TokenType: charge.TokenTypePSPToken}); err != nil {
				return nil, err
			}
		case method.Rail == string(models.RailNMI) && method.Custodian == models.CustodianPSP:
			return &method, nil
		default:
			return nil, apperr.Conflictf("replacement card requires a qualified recurring agreement")
		}
	}
	if _, err := mandates.Replace(ctx, q, mandates.Agreement{MerchantID: merchantID, CustomerID: customer, PaymentMethodID: methodID, PSPID: sub.PspID, Rail: method.Rail,
		Kind: charge.AgreementRecurring, SubscriptionID: &sub.ID, Lineage: lineage, AcceptedAt: now}, now); err != nil {
		return nil, err
	}
	sub.PaymentMethodID = &methodID
	// A delinquent membership retries on the new card at the next due
	// pass instead of waiting out the old card's schedule; one waiting
	// for a card resumes dunning.
	if err := ReplaceMethod(sub, now); err != nil {
		return nil, err
	}
	return nil, NewSubscriptionRepo(d).UpdateAt(ctx, sub, now)
}
