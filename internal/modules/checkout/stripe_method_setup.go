package checkout

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/cardguard"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/intents"
	"github.com/open-rails/openrails/internal/modules/paymentmethods"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/internal/shared/apperr"
	"github.com/open-rails/openrails/pkg/billingauth"
	"github.com/open-rails/openrails/pkg/merchant"
)

// StripeMethodSetupResponse is an owned setup resource. No payment, subscription
// or entitlement exists until a separate priced agreement is confirmed and paid.
type StripeMethodSetupResponse struct {
	ID              openrails.CheckoutSessionID `json:"id"`
	Status          string                      `json:"status"`
	SetupIntentID   string                      `json:"setup_intent_id,omitempty"`
	ClientSecret    string                      `json:"client_secret,omitempty"`
	PaymentMethodID *openrails.PaymentMethodID  `json:"payment_method_id,omitempty"`
}

func stripeSetupPrincipal(ctx context.Context, p billingauth.DelegatedPrincipal) (merchant.ID, uuid.UUID, error) {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return mid, uuid.Nil, err
	}
	customer, err := uuid.Parse(p.SubjectID)
	if err != nil || customer == uuid.Nil || p.Validate() != nil || p.CredentialClass != billingauth.CredentialClassUserSession || p.Invoker != "" || p.MerchantID != mid.String() {
		return mid, uuid.Nil, apperr.New(403, "customer_session_required", "saved card setup requires its interactive customer session")
	}
	return mid, customer, nil
}
func (s *CheckoutService) CreateStripeMethodSetup(ctx context.Context, psp uuid.UUID, key string, principal billingauth.DelegatedPrincipal, resolver intents.StripeEngineServiceResolver) (StripeMethodSetupResponse, error) {
	mid, customer, err := stripeSetupPrincipal(ctx, principal)
	if err != nil {
		return StripeMethodSetupResponse{}, err
	}
	if s == nil || s.SubscriptionService == nil || s.Config == nil || s.Config.IsProviderReadOnly() || s.customerStore() == nil || resolver == nil {
		return StripeMethodSetupResponse{}, errors.New("Stripe method setup unavailable")
	}
	if psp == uuid.Nil || key == "" || len(key) > 255 || strings.TrimSpace(key) != key || cardguard.ContainsPAN(key) {
		return StripeMethodSetupResponse{}, ErrCheckoutSessionValidation
	}
	d := s.SubscriptionService.Database()
	repo := NewCheckoutSessionRepo(d)
	id := captureSessionID(mid, customer, "stripe:"+key)
	session, err := repo.GetByID(ctx, id)
	if err == nil {
		if session.PspID != psp || session.CustomerID != customer || session.Mode != models.CheckoutSessionModePaymentMethod || session.Rail != models.RailStripe {
			return StripeMethodSetupResponse{}, ErrCheckoutSessionConflict
		}
		if session.Status == models.CheckoutSessionStatusCreated {
			return s.submitStripeMethodSetup(ctx, session, principal, resolver)
		}
		return s.StripeMethodSetup(ctx, id, principal, resolver)
	}
	if !db.IsNotFound(err) {
		return StripeMethodSetupResponse{}, err
	}
	account, err := d.Gen(ctx).GetPSP(ctx, gen.GetPSPParams{MerchantID: mid.UUID(), ID: psp})
	if err != nil || account.Archived || account.Rail != "stripe" || account.Environment != config.ExpectedProviderEnvironment(s.Config.IsTestMode()) {
		return StripeMethodSetupResponse{}, ErrCheckoutSessionValidation
	}
	service, found, err := resolver.ResolveStripeEngineService(ctx, mid.UUID(), &psp)
	if err != nil {
		return StripeMethodSetupResponse{}, err
	}
	if !found || service == nil {
		return StripeMethodSetupResponse{}, errors.New("Stripe setup account unavailable")
	}
	// Both mapping and remote customer are scoped to the immutable selected PSP.
	scoped := db.WithPSPID(ctx, psp)
	customerRef, err := resolveStripeCustomerWith(scoped, s.customerStore(), service, &UserIdentity{ID: customer.String()})
	if err != nil {
		return StripeMethodSetupResponse{}, err
	}
	if customerRef == "" {
		return StripeMethodSetupResponse{}, errors.New("Stripe customer mapping unavailable")
	}
	now := s.now().UTC().Truncate(time.Microsecond)
	expiry := now.Add(defaultCheckoutSessionTTL)
	session = &models.CheckoutSession{ID: id, CustomerID: customer, PspID: psp, Mode: models.CheckoutSessionModePaymentMethod, Rail: models.RailStripe, Status: models.CheckoutSessionStatusCreated, ExpiresAt: &expiry, RailState: map[string]any{"kind": "stripe_engine_setup", "customer_ref": customerRef, "consent": "save_for_agreed_off_session_payments_v1"}, CreatedAt: now, UpdatedAt: now}
	if err := repo.Create(ctx, session); err != nil {
		existing, getErr := repo.GetByID(ctx, id)
		if getErr != nil || existing.CustomerID != customer || existing.PspID != psp {
			return StripeMethodSetupResponse{}, err
		}
		session = existing
	}
	return s.submitStripeMethodSetup(ctx, session, principal, resolver)
}
func (s *CheckoutService) submitStripeMethodSetup(ctx context.Context, session *models.CheckoutSession, principal billingauth.DelegatedPrincipal, resolver intents.StripeEngineServiceResolver) (StripeMethodSetupResponse, error) {
	_, p, err := s.stripeSetupSession(ctx, session.ID, principal)
	if err != nil {
		return StripeMethodSetupResponse{}, err
	}
	service, found, err := resolver.ResolveStripeEngineService(ctx, p.MerchantID, &p.PSPID)
	if err != nil {
		return StripeMethodSetupResponse{}, err
	}
	if !found || service == nil {
		return StripeMethodSetupResponse{}, ErrCheckoutSessionConflict
	}
	d := s.SubscriptionService.Database()
	first, err := d.Gen(ctx).BeginStripeMethodSetup(ctx, gen.BeginStripeMethodSetupParams{MerchantID: p.MerchantID, ID: p.SessionID, Now: s.now().UTC()})
	if err != nil {
		return StripeMethodSetupResponse{}, err
	}
	if first == 1 {
		result, err := service.CreateEngineSetup(ctx, p)
		if err != nil {
			return StripeMethodSetupResponse{ID: openrails.CheckoutSessionID(p.SessionID), Status: "processing"}, nil
		}
		if _, err = d.Gen(ctx).RetainStripeMethodSetup(ctx, gen.RetainStripeMethodSetupParams{MerchantID: p.MerchantID, ID: p.SessionID, Reference: &result.ID, Now: s.now().UTC()}); err != nil {
			return StripeMethodSetupResponse{}, err
		}
	}
	return s.StripeMethodSetup(ctx, p.SessionID, principal, resolver)
}

func (s *CheckoutService) stripeSetupSession(ctx context.Context, id uuid.UUID, principal billingauth.DelegatedPrincipal) (*models.CheckoutSession, subscriptions.StripeEngineSetupParams, error) {
	var params subscriptions.StripeEngineSetupParams
	mid, customer, err := stripeSetupPrincipal(ctx, principal)
	if err != nil {
		return nil, params, err
	}
	if s == nil || s.SubscriptionService == nil {
		return nil, params, errors.New("Stripe setup unavailable")
	}
	session, err := NewCheckoutSessionRepo(s.SubscriptionService.Database()).GetByID(ctx, id)
	if err != nil || session.CustomerID != customer || session.Mode != models.CheckoutSessionModePaymentMethod || session.Rail != models.RailStripe || session.RailState["kind"] != "stripe_engine_setup" {
		return nil, params, ErrCheckoutSessionNotFound
	}
	ref, ok := session.RailState["customer_ref"].(string)
	if !ok || ref == "" {
		return nil, params, ErrCheckoutSessionConflict
	}
	params = subscriptions.StripeEngineSetupParams{MerchantID: mid.UUID(), PSPID: session.PspID, CustomerID: customer, SessionID: id, CustomerRef: ref}
	return session, params, nil
}
func (s *CheckoutService) StripeMethodSetup(ctx context.Context, id uuid.UUID, principal billingauth.DelegatedPrincipal, resolver intents.StripeEngineServiceResolver) (StripeMethodSetupResponse, error) {
	session, p, err := s.stripeSetupSession(ctx, id, principal)
	if err != nil {
		return StripeMethodSetupResponse{}, err
	}
	out := StripeMethodSetupResponse{ID: openrails.CheckoutSessionID(id), Status: string(session.Status)}
	if session.Status == models.CheckoutSessionStatusSucceeded {
		ref, _ := session.RailState["payment_method_id"].(string)
		method, err := uuid.Parse(ref)
		if err != nil {
			return out, ErrCheckoutSessionConflict
		}
		typed := openrails.PaymentMethodID(method)
		out.PaymentMethodID = &typed
		return out, nil
	}
	if session.ExpiresAt == nil || !session.ExpiresAt.After(s.now()) {
		return out, ErrCheckoutSessionExpired
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
		return out, ErrCheckoutSessionConflict
	}
	out.SetupIntentID = result.ID
	out.ClientSecret = result.ClientSecret
	out.Status = result.Status
	return out, nil
}
func (s *CheckoutService) ConfirmStripeMethodSetup(ctx context.Context, id uuid.UUID, principal billingauth.DelegatedPrincipal, resolver intents.StripeEngineServiceResolver) (StripeMethodSetupResponse, error) {
	session, p, err := s.stripeSetupSession(ctx, id, principal)
	if err != nil {
		return StripeMethodSetupResponse{}, err
	}
	if session.Status == models.CheckoutSessionStatusSucceeded {
		return s.StripeMethodSetup(ctx, id, principal, resolver)
	}
	// Read always recovers/retains the original identity; a browser can provide no
	// alternate SetupIntent, method, customer, or claimed successful outcome.
	if _, err = s.StripeMethodSetup(ctx, id, principal, resolver); err != nil {
		return StripeMethodSetupResponse{}, err
	}
	session, p, err = s.stripeSetupSession(ctx, id, principal)
	if err != nil {
		return StripeMethodSetupResponse{}, err
	}
	if session.Reference == nil {
		return StripeMethodSetupResponse{}, ErrCheckoutSessionPending
	}
	service, found, err := resolver.ResolveStripeEngineService(ctx, p.MerchantID, &p.PSPID)
	if err != nil {
		return StripeMethodSetupResponse{}, err
	}
	if !found || service == nil {
		return StripeMethodSetupResponse{}, ErrCheckoutSessionConflict
	}
	setup, found, err := service.ReadEngineSetup(ctx, p, *session.Reference)
	if err != nil {
		return StripeMethodSetupResponse{}, err
	}
	if !found || setup.Status != "succeeded" {
		return StripeMethodSetupResponse{}, apperr.Conflictf("card setup has not completed")
	}
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
			return ErrCheckoutSessionConflict
		}
		if row.Status == string(models.CheckoutSessionStatusSucceeded) {
			return nil
		}
		if row.Status != string(models.CheckoutSessionStatusRequiresAction) || row.ExpiresAt == nil || !row.ExpiresAt.After(s.now()) {
			return ErrCheckoutSessionExpired
		}
		existing, err := q.GetPaymentMethodByRailMethodRefForPSP(ctx, gen.GetPaymentMethodByRailMethodRefForPSPParams{MerchantID: p.MerchantID, Rail: "stripe", PspID: p.PSPID, RailMethodRef: setup.MethodRef})
		methodID := uuid.NewSHA1(id, []byte(setup.MethodRef))
		if err == nil {
			// ReadEngineSetup already verified both the exact SetupIntent and
			// attached PaymentMethod against this accepted customer/account.
			// Older webhook mirrors omitted only this reference; fill an empty
			// binding conditionally, never replace a different bound customer.
			if existing.CustomerID == p.CustomerID && existing.RailCustomerRef == "" && existing.ParkReason == "" {
				rows, err := q.BindMissingStripeCustomerReference(ctx, gen.BindMissingStripeCustomerReferenceParams{MerchantID: p.MerchantID, ID: existing.ID, CustomerID: p.CustomerID, PspID: p.PSPID, RailMethodRef: setup.MethodRef, RailCustomerRef: p.CustomerRef, Now: s.now().UTC()})
				if err != nil {
					return err
				}
				if rows != 1 {
					return ErrCheckoutSessionConflict
				}
				existing.RailCustomerRef = p.CustomerRef
			}
			if existing.CustomerID != p.CustomerID || existing.RailCustomerRef != p.CustomerRef || existing.ParkReason != "" {
				return ErrCheckoutSessionConflict
			}
			methodID = existing.ID
		} else if db.IsNotFound(err) {
			now := s.now().UTC()
			method := models.PaymentMethod{ID: methodID, CustomerID: p.CustomerID, PspID: p.PSPID, Rail: models.RailStripe, Custodian: models.CustodianPSP, RailCustomerRef: p.CustomerRef, RailMethodRef: setup.MethodRef, LastFour: &setup.LastFour, CardType: &setup.Brand, ExpiryDate: new(fmt.Sprintf("%02d/%02d", setup.ExpMonth, setup.ExpYear%100)), CreatedAt: now, UpdatedAt: now}
			if err := paymentmethods.NewPaymentMethodRepo(td).Create(ctx, &method); err != nil {
				return err
			}
		} else {
			return err
		}
		rows, err := q.CompleteStripeMethodSetup(ctx, gen.CompleteStripeMethodSetupParams{MerchantID: p.MerchantID, ID: id, Reference: &setup.ID, PaymentMethodID: methodID, Now: s.now().UTC()})
		if err != nil {
			return err
		}
		if rows != 1 {
			return ErrCheckoutSessionConflict
		}
		return nil
	})
	if err != nil {
		return StripeMethodSetupResponse{}, err
	}
	return s.StripeMethodSetup(ctx, id, principal, resolver)
}
