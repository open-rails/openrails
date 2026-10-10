package service

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/integrations/nmi"
	"github.com/open-rails/openrails/internal/intents"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/modules/attempts"
	"github.com/open-rails/openrails/internal/modules/mandates"
	"github.com/open-rails/openrails/internal/modules/paymentmethods"
	"github.com/open-rails/openrails/internal/modules/payments/charge"
	"github.com/open-rails/openrails/internal/modules/payments/rails/nmidirect"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/internal/shared/cardholdername"
	"github.com/open-rails/openrails/internal/shared/uuidutil"
)

var (
	// ErrPaymentMethodNotUsable: the card is closed, replaced, removed or held.
	ErrPaymentMethodNotUsable = errors.New("payment method cannot be used in its current state")
	// ErrPaymentMethodEditUnsupported: the card's holder takes no such change.
	ErrPaymentMethodEditUnsupported = errors.New("payment method's holder does not take this change through OpenRails")
	// ErrPaymentMethodEditRefused: the holder refused the change.
	ErrPaymentMethodEditRefused = errors.New("the payment provider refused the change")
)

// EditPaymentMethod edits a customer's saved card in place (#1168): its
// expiry and billing details, pushed to its holder first, and whether it is
// kept for one-click reuse. An expiry edit is a card version; the card number
// never changes.
func (s *Service) EditPaymentMethod(ctx context.Context, customerID, methodID uuid.UUID, edit billing.UpdatePaymentMethodParams) error {
	rt, err := s.runtime()
	if err != nil {
		return err
	}
	mid, err := merchant.Require(ctx)
	if err != nil {
		return err
	}
	method, err := s.liveMethod(ctx, mid.UUID(), customerID, methodID)
	if err != nil {
		return err
	}
	expMonth, expYear := 0, 0
	if edit.ExpMonth != nil && edit.ExpYear != nil {
		expMonth, expYear = *edit.ExpMonth, *edit.ExpYear
	}
	if expMonth > 0 || edit.BillingDetails != nil {
		if err := s.pushCardEdit(ctx, method, expMonth, expYear, edit.BillingDetails); err != nil {
			return err
		}
	}
	now := s.now().UTC()
	var notices []*models.NotificationQueue
	err = rt.DB.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		d := rt.DB.NewWithPgxTx(tx)
		q := d.Gen(ctx)
		locked, err := q.GetPaymentMethodForUpdate(ctx, gen.GetPaymentMethodForUpdateParams{MerchantID: mid.UUID(), ID: methodID})
		if err != nil {
			return err
		}
		if locked.CustomerID != customerID || !paymentmethods.Chargeable(locked) {
			return ErrPaymentMethodNotUsable
		}
		notices = nil
		if edit.BillingDetails != nil {
			var metadata map[string]any
			if err := models.FromJSONB(locked.Metadata, &metadata, "payment_methods.metadata"); err != nil {
				return err
			}
			raw, err := models.ToJSONB(paymentmethods.MergeBillingDetails(metadata, edit.BillingDetails))
			if err != nil {
				return err
			}
			if _, err := q.SetPaymentMethodMetadata(ctx, gen.SetPaymentMethodMetadataParams{MerchantID: mid.UUID(), ID: methodID, Metadata: raw, UpdatedAt: now}); err != nil {
				return err
			}
		}
		if expMonth > 0 {
			_, asked, err := rt.SubscriptionLifecycleService.ApplyCardLifecycle(ctx, d, paymentmethods.CardEvent{MerchantID: mid.UUID(), PaymentMethodID: methodID,
				Source: paymentmethods.SourceCustomerEdit, EventRef: "edit:" + uuidutil.NewV7().String(), Card: paymentmethods.Card{Card: models.Card{ExpMonth: expMonth, ExpYear: expYear}}, At: now})
			if err != nil {
				return err
			}
			notices = asked
		}
		if edit.Reusable != nil {
			if !*edit.Reusable {
				return mandates.RevokeReuse(ctx, q, mid.UUID(), methodID, now)
			}
			psp, err := reusePSP(ctx, charge.CustodyOf(d), locked)
			if err != nil {
				return err
			}
			return mandates.Reuse(ctx, q, mid.UUID(), customerID, methodID, psp, locked.Rail, now)
		}
		return nil
	})
	if err != nil {
		return err
	}
	rt.SubscriptionLifecycleService.DispatchNotifications(ctx, notices)
	return nil
}

// pushCardEdit sets the expiry and billing details at the card's holder: the
// NMI vault entry, or the Stripe payment method. A custodian's card takes
// billing details here only.
func (s *Service) pushCardEdit(ctx context.Context, method gen.BillingPaymentMethod, expMonth, expYear int, details *billing.BillingDetails) error {
	rt := s.rt
	switch {
	case method.Custodian == models.CustodianPSP && method.Rail == string(models.RailNMI):
		client, ok, err := rt.CollectionResolver.ResolveNMIClient(ctx, method.MerchantID, method.PspID)
		if err != nil || !ok {
			return fmt.Errorf("%w: %v", paymentmethods.ErrPaymentMethodProviderUnavailable, err)
		}
		update := nmi.UpdateCustomerVaultData{CustomerVaultID: models.DerefStr(method.RailCustomerRef), BillingID: models.DerefStr(method.RailMethodRef)}
		if expMonth > 0 {
			update.CardExp = fmt.Sprintf("%02d%02d", expMonth, expYear%100)
		}
		if d := details; d != nil {
			update.FirstName, update.LastName = cardholdername.Parts(trimmedValue(d.Name), "", "")
			update.Email, update.Phone = trimmedValue(d.Email), trimmedValue(d.Phone)
			if a := d.Address; a != nil {
				update.Address1, update.Address2, update.City = trimmedValue(a.Line1), trimmedValue(a.Line2), trimmedValue(a.City)
				update.State, update.Zip, update.Country = trimmedValue(a.State), trimmedValue(a.PostalCode), trimmedValue(a.Country)
			}
		}
		if err := client.UpdateCustomerVault(ctx, update); err != nil {
			return fmt.Errorf("%w: %v", ErrPaymentMethodEditRefused, err)
		}
		return nil
	case method.Custodian == models.CustodianPSP && method.Rail == string(models.RailStripe):
		service, err := s.stripeAccount(ctx, method)
		if err != nil {
			return err
		}
		if err := service.EditPaymentMethodCard(ctx, models.DerefStr(method.RailMethodRef), expMonth, expYear, details, "pmedit:"+uuidutil.NewV7().String()); err != nil {
			return fmt.Errorf("%w: %v", ErrPaymentMethodEditRefused, err)
		}
		return nil
	case expMonth > 0:
		return ErrPaymentMethodEditUnsupported
	}
	return nil
}

// VerifyPaymentMethod re-establishes a card's agreements after its issuer
// reissued it under another brand (POST /me/payment-methods/{id}/verify): one
// customer-present verification on the card's own account per agreement
// sequence, and each mandate waiting for consent is replaced by an active one
// citing it. Subscriptions waiting on the card resume. A card with nothing
// waiting is left as it is.
func (s *Service) VerifyPaymentMethod(ctx context.Context, customerID, methodID uuid.UUID, requestKey string) error {
	rt, err := s.runtime()
	if err != nil {
		return err
	}
	mid, err := merchant.Require(ctx)
	if err != nil {
		return err
	}
	method, err := s.liveMethod(ctx, mid.UUID(), customerID, methodID)
	if err != nil {
		return err
	}
	waiting, err := rt.DB.Gen(ctx).ListLiveMandatesOfPaymentMethods(ctx, gen.ListLiveMandatesOfPaymentMethodsParams{MerchantID: mid.UUID(), CustomerID: customerID, PaymentMethodIds: []uuid.UUID{methodID}})
	if err != nil {
		return err
	}
	recurring, unscheduled := false, false
	for _, m := range waiting {
		if m.Status != mandates.StatusRequiresReconsent {
			continue
		}
		if method.PspID == nil || m.PspID != *method.PspID {
			return ErrPaymentMethodEditUnsupported
		}
		if m.Kind == string(charge.AgreementRecurring) {
			recurring = true
		} else {
			unscheduled = true
		}
	}
	if !recurring && !unscheduled {
		return nil
	}
	lineages, err := s.verifyCard(ctx, method, recurring, unscheduled, requestKey)
	if err != nil {
		return err
	}
	now := s.now().UTC()
	return rt.DB.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		d := rt.DB.NewWithPgxTx(tx)
		q := d.Gen(ctx)
		locked, err := q.GetPaymentMethodForUpdate(ctx, gen.GetPaymentMethodForUpdateParams{MerchantID: mid.UUID(), ID: methodID})
		if err != nil {
			return err
		}
		if !paymentmethods.Chargeable(locked) {
			return ErrPaymentMethodNotUsable
		}
		for _, l := range lineages {
			if err := attempts.Record(ctx, q, attempts.Attempt{MerchantID: mid.UUID(), CustomerID: customerID, PSPID: *method.PspID, Rail: method.Rail, Kind: attempts.Verify,
				Approved: true, TransactionID: l.InitialTransactionID, PaymentMethodID: &methodID, Step: "verify:" + string(l.Kind), At: now, TokenType: charge.TokenTypePSPToken}); err != nil {
				return err
			}
		}
		awaiting, err := mandates.AwaitingConsent(ctx, q, mid.UUID(), methodID)
		if err != nil {
			return err
		}
		for _, m := range awaiting {
			l := lineages[charge.AgreementUnscheduled]
			if m.Kind == string(charge.AgreementRecurring) && method.Rail != string(models.RailStripe) {
				l = lineages[charge.AgreementRecurring]
			}
			if l.InitialTransactionID == "" {
				continue
			}
			if _, err := mandates.Reconsent(ctx, q, m, l, now); err != nil {
				return err
			}
		}
		return subscriptions.WakeForReplacedMethod(ctx, d, mid.UUID(), methodID, now)
	})
}

// verifyCard runs the customer-present verifications: on NMI one declaring
// recurring and one for card-on-file use, as the waiting agreements need; on
// Stripe one confirmed SetupIntent covers every later off-session use.
func (s *Service) verifyCard(ctx context.Context, method gen.BillingPaymentMethod, recurring, unscheduled bool, requestKey string) (map[charge.Agreement]charge.Mandate, error) {
	out := map[charge.Agreement]charge.Mandate{}
	switch {
	case method.Custodian == models.CustodianPSP && method.Rail == string(models.RailNMI):
		client, ok, err := s.rt.CollectionResolver.ResolveNMIClient(ctx, method.MerchantID, method.PspID)
		if err != nil || !ok {
			return nil, fmt.Errorf("%w: %v", paymentmethods.ErrPaymentMethodProviderUnavailable, err)
		}
		verify := func(declareRecurring bool) (string, error) {
			ref, err := client.VerifyStoredCredential(ctx, models.DerefStr(method.RailCustomerRef), models.DerefStr(method.RailMethodRef), "pmv-"+uuidutil.NewV7().String(), declareRecurring)
			var refusal *nmi.CustomerVaultError
			if errors.As(err, &refusal) && !nmi.UncertainResponseCode(refusal.ResponseCode) {
				return "", &paymentmethods.PaymentMethodError{Err: err, LocalizationID: nmidirect.FailureCode(refusal), Rail: "nmi"}
			}
			return ref, err
		}
		if recurring {
			ref, err := verify(true)
			if err != nil {
				return nil, err
			}
			out[charge.AgreementRecurring] = charge.Mandate{Kind: charge.AgreementRecurring, InitialTransactionID: ref}
		}
		if unscheduled {
			ref, err := verify(false)
			if err != nil {
				return nil, err
			}
			out[charge.AgreementUnscheduled] = charge.Mandate{Kind: charge.AgreementUnscheduled, InitialTransactionID: ref}
		}
		return out, nil
	case method.Custodian == models.CustodianPSP && method.Rail == string(models.RailStripe):
		service, err := s.stripeAccount(ctx, method)
		if err != nil {
			return nil, err
		}
		key := "pmverify:" + method.ID.String() + ":" + strings.TrimSpace(requestKey)
		if strings.TrimSpace(requestKey) == "" {
			key = "pmverify:" + uuidutil.NewV7().String()
		}
		verified, err := service.VerifyPaymentMethod(ctx, models.DerefStr(method.RailCustomerRef), models.DerefStr(method.RailMethodRef), key)
		var refusal *subscriptions.StripeAPIError
		if errors.As(err, &refusal) && refusal.StatusCode == 402 {
			code := refusal.DeclineCode
			if code == "" {
				code = refusal.Code
			}
			return nil, &paymentmethods.PaymentMethodError{Err: err, LocalizationID: code, Rail: "stripe"}
		}
		if err != nil {
			return nil, err
		}
		if verified.Status != "succeeded" {
			// The issuer asked for authentication, which this route cannot
			// run; the setup is canceled so nothing is left waiting.
			_ = service.CancelSetup(ctx, verified.ID, key+":cancel")
			return nil, &paymentmethods.PaymentMethodError{Err: fmt.Errorf("stripe setup %s is %s", verified.ID, verified.Status), Rail: "stripe", Reason: billing.DeclineAuthenticationRequired}
		}
		lineage := charge.Mandate{Kind: charge.AgreementUnscheduled, InitialTransactionID: verified.ID}
		out[charge.AgreementUnscheduled] = lineage
		return out, nil
	}
	return nil, ErrPaymentMethodEditUnsupported
}

// liveMethod is a customer's active card; another customer's is missing.
func (s *Service) liveMethod(ctx context.Context, merchantID, customerID, methodID uuid.UUID) (gen.BillingPaymentMethod, error) {
	method, err := s.rt.DB.Gen(ctx).GetPaymentMethodByID(ctx, gen.GetPaymentMethodByIDParams{MerchantID: merchantID, ID: methodID})
	if db.IsNotFound(err) || err == nil && method.CustomerID != customerID {
		return method, paymentmethods.ErrPaymentMethodNotFound
	}
	if err != nil {
		return method, err
	}
	if !paymentmethods.Chargeable(method) {
		return method, ErrPaymentMethodNotUsable
	}
	return method, nil
}

func (s *Service) stripeAccount(ctx context.Context, method gen.BillingPaymentMethod) (*subscriptions.StripeService, error) {
	resolver, ok := s.rt.CollectionResolver.(intents.StripeEngineServiceResolver)
	if !ok {
		return nil, paymentmethods.ErrPaymentMethodProviderUnavailable
	}
	service, ok, err := resolver.ResolveStripeEngineService(ctx, method.MerchantID, method.PspID)
	if err != nil || !ok {
		return nil, fmt.Errorf("%w: %v", paymentmethods.ErrPaymentMethodProviderUnavailable, err)
	}
	return service, nil
}

// reusePSP is the account a card's reuse consent belongs to: the PSP holding
// it, or the one PSP routing reaches a custodian's card through.
func reusePSP(ctx context.Context, custody charge.Custody, method gen.BillingPaymentMethod) (uuid.UUID, error) {
	if method.PspID != nil {
		return *method.PspID, nil
	}
	if method.CustodianID == nil || custody == nil {
		return uuid.Nil, ErrPaymentMethodEditUnsupported
	}
	psps, err := custody.CustodianRoutePSPs(ctx, billing.MerchantID(method.MerchantID), method.Rail, *method.CustodianID)
	if err != nil {
		return uuid.Nil, err
	}
	if len(psps) != 1 {
		return uuid.Nil, ErrPaymentMethodEditUnsupported
	}
	return psps[0], nil
}

func trimmedValue(s *string) string {
	if s == nil {
		return ""
	}
	return strings.TrimSpace(*s)
}
