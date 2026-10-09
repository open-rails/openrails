package checkout

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/billingauth"
	"github.com/open-rails/openrails/internal/cardguard"
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/intents"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/modules/paymentmethods"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/internal/shared/apperr"
)

// PaymentMethodSetup is a card setup: a payment_method checkout attempt the
// customer owns. No payment, subscription or entitlement exists until a
// separate priced agreement is confirmed and paid.
type PaymentMethodSetup struct {
	ID              billing.CheckoutAttemptID `json:"id"`
	Status          string                    `json:"status"`
	SetupIntentID   string                    `json:"setup_intent_id,omitempty"`
	ClientSecret    string                    `json:"client_secret,omitempty"`
	PaymentMethodID *billing.PaymentMethodID  `json:"payment_method_id,omitempty"`
}

func stripeSetupPrincipal(ctx context.Context, p billingauth.Payer) (billing.MerchantID, uuid.UUID, error) {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return mid, uuid.Nil, err
	}
	customer, err := uuid.Parse(p.SubjectID)
	if err != nil || customer == uuid.Nil || billingauth.ValidatePayer(&p) != nil || p.CredentialClass != billingauth.CredentialClassUserSession || p.Invoker != "" || p.MerchantID != mid {
		return mid, uuid.Nil, apperr.New(403, "customer_session_required", "saved card setup requires its interactive customer session")
	}
	return mid, customer, nil
}
func (s *CheckoutService) CreateStripeMethodSetup(ctx context.Context, psp uuid.UUID, key string, principal billingauth.Payer, resolver intents.StripeEngineServiceResolver) (PaymentMethodSetup, error) {
	mid, customer, err := stripeSetupPrincipal(ctx, principal)
	if err != nil {
		return PaymentMethodSetup{}, err
	}
	if s == nil || s.SubscriptionService == nil || s.Config == nil || config.IsProviderReadOnly(s.Config) || s.customerStore() == nil || resolver == nil {
		return PaymentMethodSetup{}, errors.New("Stripe method setup unavailable")
	}
	if psp == uuid.Nil || key == "" || len(key) > 255 || strings.TrimSpace(key) != key || cardguard.ContainsPAN(key) {
		return PaymentMethodSetup{}, ErrCheckoutAttemptValidation
	}
	d := s.SubscriptionService.Database()
	repo := NewCheckoutAttemptRepo(d)
	id := captureSessionID(mid, customer, "stripe:"+key)
	session, err := repo.GetByID(ctx, id)
	if err == nil {
		if session.PspID != psp || session.CustomerID != customer || session.Mode != models.CheckoutAttemptModePaymentMethod || session.Rail != models.RailStripe {
			return PaymentMethodSetup{}, ErrCheckoutAttemptConflict
		}
		if session.Status == models.CheckoutAttemptStatusCreated {
			return s.submitStripeMethodSetup(ctx, session, principal, resolver)
		}
		return s.StripeMethodSetup(ctx, id, principal, resolver)
	}
	if !db.IsNotFound(err) {
		return PaymentMethodSetup{}, err
	}
	account, err := d.Gen(ctx).GetPSP(ctx, gen.GetPSPParams{MerchantID: mid.UUID(), ID: psp})
	if err != nil || account.Archived || account.Rail != "stripe" || account.Environment != config.ExpectedProviderEnvironment(config.IsTestMode(s.Config)) {
		return PaymentMethodSetup{}, ErrCheckoutAttemptValidation
	}
	service, found, err := resolver.ResolveStripeEngineService(ctx, mid.UUID(), &psp)
	if err != nil {
		return PaymentMethodSetup{}, err
	}
	if !found || service == nil {
		return PaymentMethodSetup{}, errors.New("Stripe setup account unavailable")
	}
	// Both mapping and remote customer are scoped to the immutable selected PSP.
	scoped := db.WithPSPID(ctx, psp)
	customerRef, err := resolveStripeCustomerWith(scoped, s.customerStore(), service, &UserIdentity{ID: customer.String()})
	if err != nil {
		return PaymentMethodSetup{}, err
	}
	if customerRef == "" {
		return PaymentMethodSetup{}, errors.New("Stripe customer mapping unavailable")
	}
	now := s.now().UTC().Truncate(time.Microsecond)
	expiry := now.Add(defaultCheckoutAttemptTTL)
	session = &models.CheckoutAttempt{ID: id, CustomerID: customer, PspID: psp, Mode: models.CheckoutAttemptModePaymentMethod, Rail: models.RailStripe, Status: models.CheckoutAttemptStatusCreated, ExpiresAt: &expiry, RailState: map[string]any{"kind": "stripe_engine_setup", "customer_ref": customerRef, "consent": "save_for_agreed_off_session_payments_v1"}, CreatedAt: now, UpdatedAt: now}
	if err := repo.Create(ctx, session); err != nil {
		existing, getErr := repo.GetByID(ctx, id)
		if getErr != nil || existing.CustomerID != customer || existing.PspID != psp {
			return PaymentMethodSetup{}, err
		}
		session = existing
	}
	return s.submitStripeMethodSetup(ctx, session, principal, resolver)
}
func (s *CheckoutService) submitStripeMethodSetup(ctx context.Context, session *models.CheckoutAttempt, principal billingauth.Payer, resolver intents.StripeEngineServiceResolver) (PaymentMethodSetup, error) {
	_, p, err := s.stripeSetupSession(ctx, session.ID, principal)
	if err != nil {
		return PaymentMethodSetup{}, err
	}
	service, found, err := resolver.ResolveStripeEngineService(ctx, p.MerchantID, &p.PSPID)
	if err != nil {
		return PaymentMethodSetup{}, err
	}
	if !found || service == nil {
		return PaymentMethodSetup{}, ErrCheckoutAttemptConflict
	}
	d := s.SubscriptionService.Database()
	first, err := d.Gen(ctx).BeginStripeMethodSetup(ctx, gen.BeginStripeMethodSetupParams{MerchantID: p.MerchantID, ID: p.SessionID, Now: s.now().UTC()})
	if err != nil {
		return PaymentMethodSetup{}, err
	}
	if first == 1 {
		result, err := service.CreateEngineSetup(ctx, p)
		if err != nil {
			return PaymentMethodSetup{ID: billing.CheckoutAttemptID(p.SessionID), Status: "processing"}, nil
		}
		if _, err = d.Gen(ctx).RetainStripeMethodSetup(ctx, gen.RetainStripeMethodSetupParams{MerchantID: p.MerchantID, ID: p.SessionID, Reference: &result.ID, Now: s.now().UTC()}); err != nil {
			return PaymentMethodSetup{}, err
		}
	}
	return s.StripeMethodSetup(ctx, p.SessionID, principal, resolver)
}

func (s *CheckoutService) stripeSetupSession(ctx context.Context, id uuid.UUID, principal billingauth.Payer) (*models.CheckoutAttempt, subscriptions.StripeEngineSetupParams, error) {
	var params subscriptions.StripeEngineSetupParams
	mid, customer, err := stripeSetupPrincipal(ctx, principal)
	if err != nil {
		return nil, params, err
	}
	if s == nil || s.SubscriptionService == nil {
		return nil, params, errors.New("Stripe setup unavailable")
	}
	session, err := NewCheckoutAttemptRepo(s.SubscriptionService.Database()).GetByID(ctx, id)
	if err != nil || session.CustomerID != customer || session.Mode != models.CheckoutAttemptModePaymentMethod || session.Rail != models.RailStripe || session.RailState["kind"] != "stripe_engine_setup" {
		return nil, params, ErrCheckoutAttemptNotFound
	}
	ref, ok := session.RailState["customer_ref"].(string)
	if !ok || ref == "" {
		return nil, params, ErrCheckoutAttemptConflict
	}
	params = subscriptions.StripeEngineSetupParams{MerchantID: mid.UUID(), PSPID: session.PspID, CustomerID: customer, SessionID: id, CustomerRef: ref}
	return session, params, nil
}
func (s *CheckoutService) StripeMethodSetup(ctx context.Context, id uuid.UUID, principal billingauth.Payer, resolver intents.StripeEngineServiceResolver) (PaymentMethodSetup, error) {
	session, p, err := s.stripeSetupSession(ctx, id, principal)
	if err != nil {
		return PaymentMethodSetup{}, err
	}
	out := PaymentMethodSetup{ID: billing.CheckoutAttemptID(id), Status: string(session.Status)}
	if session.Status == models.CheckoutAttemptStatusSucceeded {
		ref, _ := session.RailState["payment_method_id"].(string)
		method, err := uuid.Parse(ref)
		if err != nil {
			return out, ErrCheckoutAttemptConflict
		}
		typed := billing.PaymentMethodID(method)
		out.PaymentMethodID = &typed
		return out, nil
	}
	if session.ExpiresAt == nil || !session.ExpiresAt.After(s.now()) {
		return out, ErrCheckoutAttemptExpired
	}
	if resolver == nil {
		return out, errors.New("Stripe setup resolver unavailable")
	}
	service, found, err := resolver.ResolveStripeEngineService(ctx, p.MerchantID, &p.PSPID)
	if err != nil {
		return out, err
	}
	if !found || service == nil {
		return out, errors.New("Stripe setup account unavailable")
	}
	reference := ""
	if session.Reference != nil {
		reference = *session.Reference
	}
	result, found, err := service.ReadEngineSetup(ctx, p, reference)
	if err != nil {
		return out, err
	}
	if !found {
		out.Status = "processing"
		return out, nil
	}
	rows, err := s.SubscriptionService.Database().Gen(ctx).RetainStripeMethodSetup(ctx, gen.RetainStripeMethodSetupParams{MerchantID: p.MerchantID, ID: id, Reference: &result.ID, Now: s.now().UTC()})
	if err != nil {
		return out, err
	}
	if rows != 1 {
		return out, ErrCheckoutAttemptConflict
	}
	out.SetupIntentID = result.ID
	out.ClientSecret = result.ClientSecret
	out.Status = result.Status
	return out, nil
}
func (s *CheckoutService) ConfirmStripeMethodSetup(ctx context.Context, id uuid.UUID, principal billingauth.Payer, resolver intents.StripeEngineServiceResolver) (PaymentMethodSetup, error) {
	session, p, err := s.stripeSetupSession(ctx, id, principal)
	if err != nil {
		return PaymentMethodSetup{}, err
	}
	if session.Status == models.CheckoutAttemptStatusSucceeded {
		return s.StripeMethodSetup(ctx, id, principal, resolver)
	}
	// Read always recovers/retains the original identity; a browser can provide no
	// alternate SetupIntent, method, customer, or claimed successful outcome.
	if _, err = s.StripeMethodSetup(ctx, id, principal, resolver); err != nil {
		return PaymentMethodSetup{}, err
	}
	session, p, err = s.stripeSetupSession(ctx, id, principal)
	if err != nil {
		return PaymentMethodSetup{}, err
	}
	if session.Reference == nil {
		return PaymentMethodSetup{}, ErrCheckoutAttemptPending
	}
	service, found, err := resolver.ResolveStripeEngineService(ctx, p.MerchantID, &p.PSPID)
	if err != nil {
		return PaymentMethodSetup{}, err
	}
	if !found || service == nil {
		return PaymentMethodSetup{}, ErrCheckoutAttemptConflict
	}
	setup, found, err := service.ReadEngineSetup(ctx, p, *session.Reference)
	if err != nil {
		return PaymentMethodSetup{}, err
	}
	if !found || setup.Status != "succeeded" {
		return PaymentMethodSetup{}, apperr.Conflictf("card setup has not completed")
	}
	// The confirmed off-session SetupIntent is the customer's recurring
	// consent for this card: it anchors later merchant-initiated renewals.
	d := s.SubscriptionService.Database()
	err = d.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		td := d.NewWithPgxTx(tx)
		q := td.Gen(ctx)
		if _, err := q.LockCustomerForSpend(ctx, gen.LockCustomerForSpendParams{MerchantID: p.MerchantID, ID: p.CustomerID}); err != nil {
			return err
		}
		row, err := q.GetPaymentMethodSetupSessionForUpdate(ctx, gen.GetPaymentMethodSetupSessionForUpdateParams{MerchantID: p.MerchantID, ID: id})
		if err != nil {
			return err
		}
		if row.CustomerID != p.CustomerID || row.PspID != p.PSPID || row.Reference == nil || *row.Reference != setup.ID {
			return ErrCheckoutAttemptConflict
		}
		if row.Status == string(models.CheckoutAttemptStatusSucceeded) {
			return nil
		}
		if row.Status != string(models.CheckoutAttemptStatusRequiresAction) || row.ExpiresAt == nil || !row.ExpiresAt.After(s.now()) {
			return ErrCheckoutAttemptExpired
		}
		existing, err := q.GetPaymentMethodByRailMethodRefForPSP(ctx, gen.GetPaymentMethodByRailMethodRefForPSPParams{MerchantID: p.MerchantID, Rail: "stripe", PspID: p.PSPID, RailMethodRef: setup.MethodRef})
		methodID := uuid.NewSHA1(id, []byte(setup.MethodRef))
		anchor := true
		if err == nil {
			// ReadEngineSetup already verified both the exact SetupIntent and
			// attached PaymentMethod against this accepted customer/account.
			// Older webhook mirrors omitted only this reference; fill an empty
			// binding conditionally, never replace a different bound customer.
			if existing.CustomerID == p.CustomerID && existing.RailCustomerRef == nil && existing.ParkReason == nil {
				rows, err := q.BindMissingStripeCustomerReference(ctx, gen.BindMissingStripeCustomerReferenceParams{MerchantID: p.MerchantID, ID: existing.ID, CustomerID: p.CustomerID, PspID: p.PSPID, RailMethodRef: setup.MethodRef, RailCustomerRef: p.CustomerRef, Now: s.now().UTC()})
				if err != nil {
					return err
				}
				if rows != 1 {
					return ErrCheckoutAttemptConflict
				}
				existing.RailCustomerRef = new(p.CustomerRef)
			}
			if existing.CustomerID != p.CustomerID || models.DerefStr(existing.RailCustomerRef) != p.CustomerRef || existing.ParkReason != nil {
				return ErrCheckoutAttemptConflict
			}
			methodID = existing.ID
			anchor = existing.StoredCredentialRecurringRef == nil
		} else if db.IsNotFound(err) {
			now := s.now().UTC()
			card := models.ParseCard(setup.Brand, setup.LastFour, "")
			if setup.ExpMonth >= 1 && setup.ExpMonth <= 12 && setup.ExpYear >= 2000 {
				card.ExpMonth, card.ExpYear = setup.ExpMonth, setup.ExpYear
			}
			method := models.PaymentMethod{ID: methodID, CustomerID: p.CustomerID, PspID: &p.PSPID, Rail: models.RailStripe, Custodian: models.CustodianPSP, RailCustomerRef: p.CustomerRef, RailMethodRef: setup.MethodRef, Card: card, CreatedAt: now, UpdatedAt: now}
			if err := paymentmethods.NewPaymentMethodRepo(td).Create(ctx, &method); err != nil {
				return err
			}
		} else {
			return err
		}
		if anchor {
			if _, err := q.CaptureStoredCredentialRef(ctx, gen.CaptureStoredCredentialRefParams{MerchantID: p.MerchantID, ID: methodID, Agreement: "recurring", Ref: setup.ID}); err != nil {
				return err
			}
		}
		rows, err := q.CompleteStripeMethodSetup(ctx, gen.CompleteStripeMethodSetupParams{MerchantID: p.MerchantID, ID: id, Reference: &setup.ID, PaymentMethodID: methodID, Now: s.now().UTC()})
		if err != nil {
			return err
		}
		if rows != 1 {
			return ErrCheckoutAttemptConflict
		}
		return nil
	})
	if err != nil {
		return PaymentMethodSetup{}, err
	}
	return s.StripeMethodSetup(ctx, id, principal, resolver)
}
