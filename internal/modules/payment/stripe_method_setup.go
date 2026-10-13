package payment

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/intents"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/modules/mandates"
	"github.com/open-rails/openrails/internal/modules/paymentmethods"
	"github.com/open-rails/openrails/internal/modules/payments/charge"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/internal/writeposture"
)

// A Stripe card is saved in one call: the page's Stripe fields make a pm_,
// and OpenRails creates and confirms a SetupIntent with it. When the bank
// asks for 3-D Secure the method is requires_action until the customer
// completes it in the page and confirms.

// SetupAbandonAfter is how long a card being saved waits for the customer.
const SetupAbandonAfter = 24 * time.Hour

// SavedCard is a saved card or the next action needed to finish storing it.
type SavedCard struct {
	Method *models.PaymentMethod
	// NextAction is the bank's authentication while the method requires
	// action; its client secret is never stored.
	NextAction *billing.NextAction
}

// stripeCardSave is the account and customer one Stripe card is saved to.
type stripeCardSave struct {
	service  *subscriptions.StripeService
	params   subscriptions.StripeEngineSetupParams
	customer uuid.UUID
	psp      uuid.UUID
}

func (s *PurchaseService) stripeCardAccount(ctx context.Context, psp uuid.UUID, user *UserIdentity, resolver intents.StripeEngineServiceResolver) (*stripeCardSave, error) {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	customer, err := uuid.Parse(user.ID)
	if err != nil || customer == uuid.Nil {
		return nil, errors.New("Stripe card save needs its customer")
	}
	if s == nil || s.SubscriptionService == nil || s.Config == nil || resolver == nil {
		return nil, errors.New("Stripe card save unavailable")
	}
	account, err := s.SubscriptionService.Database().Gen(ctx).GetPSP(ctx, gen.GetPSPParams{MerchantID: mid.UUID(), ID: psp})
	if err != nil || account.Rail != string(models.RailStripe) {
		return nil, ErrOrderAttemptValidation
	}
	service, found, err := resolver.ResolveStripeEngineService(ctx, mid.UUID(), &psp)
	if err != nil {
		return nil, err
	}
	if !found || service == nil {
		return nil, errors.New("Stripe account unavailable")
	}
	return &stripeCardSave{service: service, customer: customer, psp: psp,
		params: subscriptions.StripeEngineSetupParams{MerchantID: mid.UUID(), PSPID: psp, CustomerID: customer}}, nil
}

// StripeCardID is the method a customer's token becomes: a retry of the same
// save finds it.
func StripeCardID(merchantID, customer uuid.UUID, token string) uuid.UUID {
	return uuid.NewSHA1(uuid.NameSpaceURL, []byte("openrails:stripe-card:"+merchantID.String()+":"+customer.String()+":"+token))
}

// SaveStripeCard saves the card behind token, a pm_ the page's Stripe fields
// made, to the customer.
func (s *PurchaseService) SaveStripeCard(ctx context.Context, psp uuid.UUID, token string, user *UserIdentity, resolver intents.StripeEngineServiceResolver) (SavedCard, error) {
	token = strings.TrimSpace(token)
	account, err := s.stripeCardAccount(ctx, psp, user, resolver)
	if err != nil {
		return SavedCard{}, err
	}
	d := s.SubscriptionService.Database()
	id := StripeCardID(account.params.MerchantID, account.customer, token)
	if existing, err := d.Gen(ctx).GetPaymentMethodByID(ctx, gen.GetPaymentMethodByIDParams{MerchantID: account.params.MerchantID, ID: id}); err == nil {
		return s.ConfirmStripeCard(ctx, existing.ID, user, resolver)
	} else if !db.IsNotFound(err) {
		return SavedCard{}, err
	}
	if (writeposture.View{Config: s.Config, DB: d}).Posture(ctx, account.params.MerchantID).ReadOnly() {
		return SavedCard{}, errors.New("Stripe card save unavailable: provider writes are disabled")
	}
	account.params.CustomerRef, err = resolveStripeCustomerWith(db.WithPSPID(ctx, psp), s.customerStore(), account.service, user)
	if err != nil {
		return SavedCard{}, err
	}
	if account.params.CustomerRef == "" {
		return SavedCard{}, errors.New("Stripe customer mapping unavailable")
	}
	read, err := account.service.ReadPaymentMethodCard(ctx, token)
	if err != nil {
		return SavedCard{}, err
	}
	if read == nil || read.Card == nil || read.CustomerID != "" && read.CustomerID != account.params.CustomerRef {
		return SavedCard{}, fmt.Errorf("%w: Stripe has no card %s for this customer", ErrPaymentMethodStale, token)
	}
	account.params.SessionID = id
	setup, err := account.service.ConfirmEngineSetup(ctx, account.params, token)
	var declined *subscriptions.StripeSetupDeclined
	if errors.As(err, &declined) {
		return SavedCard{}, &paymentmethods.PaymentMethodError{Err: err, LocalizationID: declined.DeclineCode, Message: "card verification failed", Rail: string(models.RailStripe)}
	}
	if err != nil {
		return SavedCard{}, err
	}
	now := s.now().UTC()
	method := &models.PaymentMethod{ID: id, CustomerID: account.customer, PspID: &account.psp, Rail: models.RailStripe, Custodian: models.CustodianPSP,
		RailCustomerRef: account.params.CustomerRef, RailMethodRef: token, Card: *read.Card, Fingerprint: read.Fingerprint, CreatedAt: now, UpdatedAt: now}
	switch setup.Status {
	case "succeeded":
	case "requires_action":
		method.SetupRef = setup.ID
	default:
		return SavedCard{}, &paymentmethods.PaymentMethodError{Err: errors.New("the bank did not save the card"), Message: "card verification failed", Rail: string(models.RailStripe)}
	}
	err = d.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		td := d.NewWithPgxTx(tx)
		if err := paymentmethods.NewPaymentMethodRepo(td).Create(ctx, method); err != nil {
			return err
		}
		if method.SetupRef != "" {
			return nil
		}
		return recordStripeCardStored(ctx, td.Gen(ctx), method, setup.ID, now)
	})
	if err != nil {
		return SavedCard{}, err
	}
	out := SavedCard{Method: method}
	if method.SetupRef != "" {
		method.Status = paymentmethods.StatusRequiresAction
		out.NextAction = stripeSetupAction(account.psp, setup)
	} else {
		method.Status = paymentmethods.StatusActive
	}
	return out, nil
}

// ConfirmStripeCard reads the SetupIntent of a card the bank asked to
// authenticate: succeeded saves it; still waiting answers its next action; a
// refusal removes it.
func (s *PurchaseService) ConfirmStripeCard(ctx context.Context, methodID uuid.UUID, user *UserIdentity, resolver intents.StripeEngineServiceResolver) (SavedCard, error) {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return SavedCard{}, err
	}
	d := s.SubscriptionService.Database()
	row, err := d.Gen(ctx).GetPaymentMethodByID(ctx, gen.GetPaymentMethodByIDParams{MerchantID: mid.UUID(), ID: methodID})
	if db.IsNotFound(err) || err == nil && row.CustomerID.String() != user.ID {
		return SavedCard{}, paymentmethods.ErrPaymentMethodNotFound
	}
	if err != nil {
		return SavedCard{}, err
	}
	method, err := models.PaymentMethodFromGen(row)
	if err != nil {
		return SavedCard{}, err
	}
	if row.Status != paymentmethods.StatusRequiresAction || row.Rail != string(models.RailStripe) || row.PspID == nil {
		return SavedCard{Method: method}, nil
	}
	account, err := s.stripeCardAccount(ctx, *row.PspID, user, resolver)
	if err != nil {
		return SavedCard{}, err
	}
	account.params.SessionID = row.ID
	// Recovery uses the customer accepted when this card was saved. Resolving
	// a customer again could create one while provider writes are held.
	account.params.CustomerRef = method.RailCustomerRef
	setup, found, err := account.service.ReadEngineSetup(ctx, account.params, method.SetupRef)
	if err != nil {
		return SavedCard{}, err
	}
	now := s.now().UTC()
	switch {
	case found && setup.Status == "requires_action":
		return SavedCard{Method: method, NextAction: stripeSetupAction(account.psp, setup)}, nil
	case found && setup.Status == "succeeded":
		err = d.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
			q := gen.New(tx)
			n, err := q.CompletePaymentMethodSetup(ctx, gen.CompletePaymentMethodSetupParams{MerchantID: mid.UUID(), ID: row.ID, Now: now})
			if err != nil || n == 0 {
				return err
			}
			return recordStripeCardStored(ctx, q, method, setup.ID, now)
		})
		if err != nil {
			return SavedCard{}, err
		}
		method.Status, method.SetupRef = paymentmethods.StatusActive, ""
		return SavedCard{Method: method}, nil
	}
	if _, err := d.Gen(ctx).AbandonPaymentMethodSetup(ctx, gen.AbandonPaymentMethodSetupParams{MerchantID: mid.UUID(), ID: row.ID, Now: now}); err != nil {
		return SavedCard{}, err
	}
	return SavedCard{}, &paymentmethods.PaymentMethodError{Err: errors.New("the card's authentication failed"), LocalizationID: "authentication_required",
		Message: "card verification failed", Rail: string(models.RailStripe), Reason: billing.DeclineAuthenticationRequired}
}

// recordStripeCardStored records the card-on-file agreement the confirmed
// off-session SetupIntent stored.
func recordStripeCardStored(ctx context.Context, q *gen.Queries, method *models.PaymentMethod, setupID string, at time.Time) error {
	return mandates.RecordStored(ctx, q, mandates.Stored{MerchantID: merchantOf(ctx), CustomerID: method.CustomerID, PaymentMethodID: method.ID, PSPID: *method.PspID,
		Rail: string(models.RailStripe), Lineage: charge.Mandate{InitialTransactionID: setupID}, AcceptedAt: at})
}

func merchantOf(ctx context.Context) uuid.UUID {
	mid, _ := merchant.Require(ctx)
	return mid.UUID()
}

// stripeSetupAction is the bank's authentication of a card being saved,
// which Stripe.js runs in the page (handleNextAction).
func stripeSetupAction(psp uuid.UUID, setup subscriptions.StripeEngineSetup) *billing.NextAction {
	id := billing.PSPID(psp)
	return &billing.NextAction{Type: "authenticate", PSPID: &id, Transactions: []string{},
		Payload: map[string]string{"client_secret": setup.ClientSecret, "setup_intent_id": setup.ID}}
}
