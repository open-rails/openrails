package subscriptions

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/internal/modules/attempts"
	"github.com/open-rails/openrails/internal/modules/mandates"
	"github.com/open-rails/openrails/internal/modules/paymentmethods"
	"github.com/open-rails/openrails/internal/modules/payments/charge"
	"github.com/open-rails/openrails/internal/shared/apperr"
	"github.com/open-rails/openrails/internal/shared/uuidutil"
)

// RecurringVerifier runs a customer-present $0 verification declaring a
// recurring agreement on an NMI card through psp, returning NMI's
// transaction id.
type RecurringVerifier func(ctx context.Context, psp uuid.UUID, vault, billing, order string) (string, error)

// ErrCardUnusable: the saved card can no longer be charged.
var ErrCardUnusable = apperr.New(http.StatusPaymentRequired, billing.CodePaymentMethodStale, "the card can no longer be charged; add it again")

// ErrRecurringAgreementRequired: the card has no recurring agreement on the
// subscription's account and none can be verified here.
var ErrRecurringAgreementRequired = apperr.Conflictf("the card requires a qualified recurring agreement")

// UpdateEnginePaymentMethod sets a subscription's own card, or with methodID
// nil clears it so the subscription follows its customer's default for its
// currency. Its recurring mandate moves to the card it then charges, citing
// the card's recurring lineage on the subscription's account, or a recurring
// verification run now when the card has none (NMI cards the PSP holds).
// It never changes schedule ownership or issues a provider schedule mutation.
// Accepted uncertain payments keep their frozen instrument until resolved.
func (s *SubscriptionLifecycleService) UpdateEnginePaymentMethod(ctx context.Context, subscriptionID, customer uuid.UUID, methodID *uuid.UUID, verify RecurringVerifier) error {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return err
	}
	var verified *charge.Mandate
	for {
		var needs *verification
		err := s.DB.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
			var err error
			needs, err = s.moveEngineMethod(ctx, s.DB.NewWithPgxTx(tx), mid.UUID(), subscriptionID, customer, methodID, verified)
			return err
		})
		if err != nil || needs == nil {
			return err
		}
		if verified, err = s.verifyRecurring(ctx, verify, *needs); err != nil {
			return err
		}
	}
}

// moveEngineMethod returns the verification the move needs first when the
// card has no recurring lineage and verified is nil.
func (s *SubscriptionLifecycleService) moveEngineMethod(ctx context.Context, d *db.DB, merchantID, subscriptionID, customer uuid.UUID, methodID *uuid.UUID, verified *charge.Mandate) (*verification, error) {
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
	sub.PaymentMethodID = methodID
	target, err := PaymentMethodOf(ctx, q, sub)
	if err != nil {
		return nil, err
	}
	if target == nil {
		return nil, ErrDefaultPaymentMethodRequired
	}
	method, err := s.engineCard(ctx, d, sub, *target)
	if err != nil {
		return nil, err
	}
	lineage, needs, err := recurringLineage(ctx, q, sub, method, verified)
	if err != nil || needs != nil {
		return needs, err
	}
	return nil, s.moveRecurring(ctx, d, sub, method.ID, lineage, s.now())
}

// engineCard reads, under its shared row lock, a saved card of sub's customer
// that can pay sub's renewals through sub's account.
func (s *SubscriptionLifecycleService) engineCard(ctx context.Context, d *db.DB, sub *models.Subscription, methodID uuid.UUID) (gen.BillingPaymentMethod, error) {
	q := d.Gen(ctx)
	merchantID := sub.MerchantID
	observed, err := q.GetPaymentMethodByID(ctx, gen.GetPaymentMethodByIDParams{MerchantID: merchantID, ID: methodID})
	if err != nil {
		return gen.BillingPaymentMethod{}, err
	}
	if observed.CustodianID != nil {
		handle := paymentmethods.CustodianHandle{Custodian: *observed.CustodianID, Method: models.DerefStr(observed.RailMethodRef)}
		if err := paymentmethods.LockCustodianHandles(ctx, q, merchantID, handle); err != nil {
			return gen.BillingPaymentMethod{}, err
		}
		if err := paymentmethods.RequireCustodianHandleAvailable(ctx, q, merchantID, handle); err != nil {
			return gen.BillingPaymentMethod{}, err
		}
	}
	method, err := q.GetPaymentMethodForShare(ctx, gen.GetPaymentMethodForShareParams{MerchantID: merchantID, ID: methodID})
	if err != nil {
		return method, err
	}
	if method.CustomerID != sub.CustomerID {
		// Another customer's method is indistinguishable from a missing one.
		return method, apperr.New(http.StatusNotFound, billing.CodeResourceNotFound, "payment method not found")
	}
	if !charge.ChargeableOn(method, sub.PspID) || method.Rail != string(sub.Rail) {
		return method, fmt.Errorf("%w: subscription %s", ErrCardNotForSubscription, billing.SubscriptionID(sub.ID))
	}
	if !paymentmethods.Chargeable(method) || method.ChargeVia != "pan_proxy" {
		return method, ErrCardUnusable
	}
	if err := charge.FreezeInstrument(observed, sub.PspID).Matches(method); err != nil {
		return method, err
	}
	configuration := merchants.Of(d)
	if configuration == nil {
		return method, errors.New("merchant configuration is unavailable")
	}
	psp, ok, err := configuration.PSPScopeByID(ctx, billing.MerchantID(merchantID), sub.PspID)
	if err != nil {
		return method, err
	}
	if !ok || psp.Archived || psp.Rail != method.Rail {
		return method, apperr.Conflictf("payment account is archived")
	}
	var binding *charge.HyperSwitchBinding
	if method.Custodian == models.CustodianHyperSwitch {
		if s.Config == nil || s.Config.HyperSwitch == nil {
			return method, errors.New("HyperSwitch deployment is unavailable")
		}
		frozen, err := charge.FreezeHyperSwitchBinding(ctx, q, configuration, method, sub.PspID, s.Config.HyperSwitch.APIBaseURL)
		if err != nil {
			return method, err
		}
		binding = &frozen
		custodian, ok, err := configuration.CustodianScopeByID(ctx, billing.MerchantID(merchantID), *method.CustodianID)
		if err != nil {
			return method, err
		}
		if !ok || custodian.Archived {
			return method, apperr.Conflictf("payment custodian is archived")
		}
	}
	if err := charge.ValidateEngineInstrument(method.Rail, charge.FreezeInstrument(method, sub.PspID), binding); err != nil {
		return method, ErrRecurringAgreementRequired
	}
	return method, nil
}

// verification is a recurring verification a move needs on a card through
// one account before it commits.
type verification struct {
	method gen.BillingPaymentMethod
	psp    uuid.UUID
}

// recurringLineage is the lineage moving sub's recurring agreement onto
// method cites: the card's own recurring lineage on sub's account, else
// verified. Without either, an NMI card the PSP holds needs a verification.
func recurringLineage(ctx context.Context, q *gen.Queries, sub *models.Subscription, method gen.BillingPaymentMethod, verified *charge.Mandate) (*charge.Mandate, *verification, error) {
	lineage, err := mandates.Citable(ctx, q, sub.MerchantID, sub.CustomerID, method.ID, sub.PspID, method.Rail, charge.AgreementRecurring)
	if err != nil || lineage != nil {
		return lineage, nil, err
	}
	if verified != nil {
		return verified, nil, nil
	}
	if method.Rail == string(models.RailNMI) && method.Custodian == models.CustodianPSP {
		return nil, &verification{method: method, psp: sub.PspID}, nil
	}
	return nil, nil, ErrRecurringAgreementRequired
}

// verifyRecurring runs the one customer-present recurring verification a move
// needs and records it; its transaction is the storing transaction the moved
// agreements cite.
func (s *SubscriptionLifecycleService) verifyRecurring(ctx context.Context, verify RecurringVerifier, v verification) (*charge.Mandate, error) {
	if verify == nil {
		return nil, ErrRecurringAgreementRequired
	}
	ref, err := verify(ctx, v.psp, models.DerefStr(v.method.RailCustomerRef), models.DerefStr(v.method.RailMethodRef), "pmr-"+uuidutil.NewV7().String())
	if err != nil {
		return nil, err
	}
	err = s.DB.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		return attempts.Record(ctx, s.DB.NewWithPgxTx(tx).Gen(ctx), attempts.Attempt{MerchantID: v.method.MerchantID, CustomerID: v.method.CustomerID, PSPID: v.psp, Rail: v.method.Rail,
			Kind: attempts.Verify, Approved: true, TransactionID: ref, PaymentMethodID: &v.method.ID, Step: "verify", At: s.now(), TokenType: charge.TokenTypePSPToken})
	})
	if err != nil {
		return nil, err
	}
	return &charge.Mandate{Kind: charge.AgreementRecurring, InitialTransactionID: ref}, nil
}

// moveRecurring makes sub's recurring mandate the one on methodID citing
// lineage, and stores sub with the card it now has (its own, or none to follow
// the default). A live membership waiting on its card retries on this one.
func (s *SubscriptionLifecycleService) moveRecurring(ctx context.Context, d *db.DB, sub *models.Subscription, methodID uuid.UUID, lineage *charge.Mandate, now time.Time) error {
	if _, err := mandates.Replace(ctx, d.Gen(ctx), mandates.Agreement{MerchantID: sub.MerchantID, CustomerID: sub.CustomerID, PaymentMethodID: methodID, PSPID: sub.PspID, Rail: string(sub.Rail),
		Kind: charge.AgreementRecurring, SubscriptionID: &sub.ID, Lineage: lineage, AcceptedAt: now}, now); err != nil {
		return err
	}
	if sub.Status == models.StatusActive || sub.Status == models.StatusPastDue || sub.Status == models.StatusAwaitingMethod {
		if err := ReplaceMethod(sub, now); err != nil {
			return err
		}
	}
	return NewSubscriptionRepo(d).UpdateAt(ctx, sub, now)
}
