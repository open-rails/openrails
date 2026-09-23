package checkout

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/modules/catalog"
	"github.com/open-rails/openrails/internal/modules/entitlements"
	"github.com/open-rails/openrails/internal/modules/payments"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/internal/shared/uuidutil"
	"github.com/open-rails/openrails/pkg/merchant"
)

const acceptedPurchaseTermsKey = "accepted_purchase"

// Stored in the existing checkout session, alongside its buyer and PSP. Money
// stays a decimal string even when rail_state is decoded through map[string]any.
type acceptedPurchaseTerms struct {
	PriceID             uuid.UUID                    `json:"price_id"`
	ProductID           uuid.UUID                    `json:"product_id"`
	PaymentID           uuid.UUID                    `json:"payment_id"`
	ProductKey          string                       `json:"product_key"`
	ProductName         string                       `json:"product_name"`
	Amount              int64                        `json:"amount,string"`
	Currency            string                       `json:"currency"`
	AccessDurationHours *int                         `json:"access_duration_hours"`
	Entitlements        map[string]*int              `json:"entitlements"`
	PSPLinks            map[string]map[string]string `json:"psp_links,omitempty"`
	AcceptedAt          time.Time                    `json:"accepted_at"`
	EntitlementStart    time.Time                    `json:"entitlement_start"`
}

func (t acceptedPurchaseTerms) catalog(merchantID uuid.UUID) (*models.Price, *models.Product) {
	return &models.Price{ID: t.PriceID, MerchantID: merchantID, ProductID: t.ProductID, Amount: t.Amount, Currency: t.Currency, AccessDurationHours: t.AccessDurationHours, PSPLinks: t.PSPLinks},
		&models.Product{ID: t.ProductID, MerchantID: merchantID, Key: t.ProductKey, DisplayName: t.ProductName, EntitlementsSpec: models.CloneEntitlementsSpec(t.Entitlements)}
}

func purchaseTerms(session *models.CheckoutSession) (*acceptedPurchaseTerms, error) {
	value, found := session.RailState[acceptedPurchaseTermsKey]
	if !found {
		return nil, nil // Historical sessions predate the admission snapshot.
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	var terms acceptedPurchaseTerms
	if err := json.Unmarshal(raw, &terms); err != nil {
		return nil, err
	}
	if session.Mode != models.CheckoutSessionModeOneOff || session.PriceID == nil || terms.PriceID != *session.PriceID || terms.ProductID == uuid.Nil || terms.PaymentID == uuid.Nil || session.Amount == nil || terms.Amount != *session.Amount || session.Currency == nil || terms.Currency != *session.Currency || terms.AcceptedAt.IsZero() || terms.EntitlementStart.Before(terms.AcceptedAt) || terms.AccessDurationHours != nil && *terms.AccessDurationHours <= 0 {
		return nil, errors.New("checkout accepted purchase terms contradict session")
	}
	return &terms, nil
}

func permanentPurchase(price *models.Price) bool {
	return price != nil && !price.AutoRenew && price.AccessDurationHours == nil
}

func (s *CheckoutPurchaseService) checkPermanentOwnership(ctx context.Context, user string, price *models.Price, product *models.Product) error {
	if !permanentPurchase(price) || s.database == nil {
		return nil
	}
	mid, err := merchant.Require(ctx)
	if err != nil {
		return err
	}
	customer, err := customerIDFromUser(user)
	if err != nil {
		return err
	}
	owned, err := s.database.Gen(ctx).HasPermanentProductOwnership(ctx, gen.HasPermanentProductOwnershipParams{MerchantID: mid.UUID(), CustomerID: customer, ProductID: product.ID, AtTime: s.now()})
	if err != nil {
		return err
	}
	if owned {
		return fmt.Errorf("%w: customer already owns this product", ErrCheckoutSessionConflict)
	}
	return nil
}

// admitPurchaseSession publishes the immutable terms and the exclusion together.
// No provider request runs while the customer or catalog rows are locked.
func (s *CheckoutSessionService) admitPurchaseSession(ctx context.Context, session *models.CheckoutSession) error {
	return s.db.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		d := s.db.NewWithPgxTx(tx)
		mid, err := merchant.Require(ctx)
		if err != nil {
			return err
		}
		if err := db.EnsureCustomerRow(ctx, d.Qx(ctx), uuid.Nil, session.CustomerID); err != nil {
			return err
		}
		if _, err = d.Gen(ctx).LockCustomerForSpend(ctx, gen.LockCustomerForSpendParams{MerchantID: mid.UUID(), ID: session.CustomerID}); err != nil {
			return err
		}
		if _, err = d.Gen(ctx).LockPurchasableCheckoutPrice(ctx, gen.LockPurchasableCheckoutPriceParams{MerchantID: mid.UUID(), PriceID: *session.PriceID}); err != nil {
			if db.IsNotFound(err) {
				return fmt.Errorf("%w: offer is no longer available", ErrCheckoutSessionValidation)
			}
			return err
		}
		price, err := catalog.NewPriceService(d).GetByID(ctx, *session.PriceID)
		if err != nil {
			return err
		}
		product, err := catalog.NewProductService(d).GetByID(ctx, price.ProductID)
		if err != nil {
			return err
		}
		if price.AutoRenew || session.Amount == nil || *session.Amount != price.Amount || session.Currency == nil || *session.Currency != price.Currency {
			return fmt.Errorf("%w: purchase terms changed", ErrCheckoutSessionConflict)
		}
		purchase := NewCheckoutPurchaseService(catalog.NewPriceService(d), catalog.NewProductService(d), payments.NewPaymentService(d, s.clock), entitlements.NewEntitlementService(d, s.clock), nil, s.clock)
		purchase.SubscriptionService = subscriptions.NewSubscriptionService(d, purchase.PriceService, purchase.ProductService, nil, s.clock)
		eligibility, err := purchase.CheckPurchaseEligibility(ctx, session.CustomerID.String(), price.ID)
		if err != nil {
			return err
		}
		if eligibility.Status != EligibilityAllowed {
			return fmt.Errorf("%w: %s", ErrCheckoutSessionConflict, eligibility.Reason)
		}
		if permanentPurchase(price) {
			pending, err := d.Gen(ctx).HasUnresolvedProductCheckout(ctx, gen.HasUnresolvedProductCheckoutParams{MerchantID: mid.UUID(), CustomerID: session.CustomerID, ProductID: product.ID, ExceptSessionID: session.ID})
			if err != nil {
				return err
			}
			if pending {
				return fmt.Errorf("%w: another checkout for this product is unresolved", ErrCheckoutSessionConflict)
			}
			_, err = d.Gen(ctx).GetUnresolvedSaleForCustomerProduct(ctx, gen.GetUnresolvedSaleForCustomerProductParams{MerchantID: mid.UUID(), CustomerID: session.CustomerID.String(), ProductID: product.ID.String()})
			if err == nil {
				return fmt.Errorf("%w: another purchase for this product is unresolved", ErrCheckoutSessionConflict)
			}
			if !db.IsNotFound(err) {
				return err
			}
		}
		now := s.now().UTC().Truncate(time.Microsecond)
		terms := acceptedPurchaseTerms{PriceID: price.ID, ProductID: product.ID, PaymentID: uuidutil.NewV7(), ProductKey: product.Key, ProductName: product.DisplayName, Amount: price.Amount, Currency: price.Currency, AccessDurationHours: price.AccessDurationHours, Entitlements: models.CloneEntitlementsSpec(product.EntitlementsSpec), PSPLinks: price.PSPLinks, AcceptedAt: now, EntitlementStart: now}
		if eligibility.Coverage != nil && eligibility.Coverage.EndDate != nil && eligibility.Coverage.EndDate.After(now) {
			terms.EntitlementStart = eligibility.Coverage.EndDate.UTC().Truncate(time.Microsecond)
		}
		if session.RailState == nil {
			session.RailState = map[string]any{}
		}
		session.RailState[acceptedPurchaseTermsKey] = terms
		return NewCheckoutSessionRepo(d).Create(ctx, session)
	})
}

// registerSessionPurchase consumes the provider's observation, but derives the
// buyer, product, money and benefits from the durable local agreement. Existing
// payment and grant writers commit in this same transaction.
func (s *CheckoutPurchaseService) registerSessionPurchase(ctx context.Context, req *payments.RegisterPurchaseRequest) (*payments.RegisterPurchaseResponse, error) {
	if s.database == nil {
		return nil, errors.New("checkout settlement database unavailable")
	}
	var result *payments.RegisterPurchaseResponse
	err := s.database.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		d := s.database.NewWithPgxTx(tx)
		mid, err := merchant.Require(ctx)
		if err != nil {
			return err
		}
		customer, err := customerIDFromUser(req.UserID)
		if err != nil {
			return err
		}
		if _, err = d.Gen(ctx).LockCustomerForSpend(ctx, gen.LockCustomerForSpendParams{MerchantID: mid.UUID(), ID: customer}); err != nil {
			return err
		}
		session, err := NewCheckoutSessionRepo(d).GetByID(ctx, req.CheckoutSessionID)
		if err != nil {
			return err
		}
		if session.CustomerID != customer || session.PriceID == nil || *session.PriceID != req.PriceID || string(session.Rail) != req.Rail || session.PspID != db.PSPIDFromContext(ctx) || req.SubscriptionID != nil {
			return errors.New("provider purchase does not match checkout session")
		}
		terms, err := purchaseTerms(session)
		if err != nil {
			return err
		}
		bound := s.transactionBound(d)
		if terms == nil {
			legacy := *req
			legacy.CheckoutSessionID = uuid.Nil
			result, err = bound.RegisterPurchase(ctx, &legacy)
			return err
		}
		if req.Amount != terms.Amount || !strings.EqualFold(req.Currency, terms.Currency) || strings.TrimSpace(req.TransactionID) == "" {
			return errors.New("provider purchase contradicts accepted checkout amount")
		}
		price, product := terms.catalog(mid.UUID())
		coverage := &CoverageInfo{}
		if terms.EntitlementStart.After(terms.AcceptedAt) {
			coverage.HasCoverage = true
			coverage.EndDate = &terms.EntitlementStart
		}
		result, err = bound.applyPurchase(ctx, req, price, product, &EligibilityResult{Status: EligibilityAllowed, Coverage: coverage}, terms.AcceptedAt, terms.PaymentID)
		if err != nil {
			return err
		}
		session.Status = models.CheckoutSessionStatusSucceeded
		session.PaymentID = &result.PaymentID
		session.TransactionID = &req.TransactionID
		session.UpdatedAt = s.now()
		return NewCheckoutSessionRepo(d).Update(ctx, session)
	})
	return result, err
}

// MarkProviderCheckoutClosed records authoritative Stripe closure. Local TTL
// expiry uses MarkExpired and never releases an uncertain provider purchase.
func (s *CheckoutSessionService) MarkProviderCheckoutClosed(ctx context.Context, id uuid.UUID, status models.CheckoutSessionStatus) error {
	if status != models.CheckoutSessionStatusExpired && status != models.CheckoutSessionStatusFailed {
		return ErrCheckoutSessionValidation
	}
	mid, err := merchant.Require(ctx)
	if err != nil {
		return err
	}
	psp, err := db.RequirePSPID(ctx)
	if err != nil {
		return err
	}
	rows, err := s.db.Gen(ctx).CloseHostedCheckoutFromProvider(ctx, gen.CloseHostedCheckoutFromProviderParams{MerchantID: mid.UUID(), PspID: psp, ID: id, Status: string(status), Now: s.now()})
	if err != nil {
		return err
	}
	if rows == 0 {
		return ErrCheckoutSessionNotFound
	}
	return nil
}

func (s *CheckoutSessionService) markInitializationFailed(ctx context.Context, session *models.CheckoutSession, failure error) error {
	if _, accepted := session.RailState[acceptedPurchaseTermsKey]; accepted && session.Rail == models.RailStripe {
		mid, err := merchant.Require(ctx)
		if err != nil {
			return err
		}
		_, err = s.db.Gen(ctx).FailHostedPurchaseInitialization(ctx, gen.FailHostedPurchaseInitializationParams{MerchantID: mid.UUID(), ID: session.ID, Reason: failure.Error(), Now: s.now()})
		return err
	}
	return s.MarkFailed(ctx, session.ID, failure.Error(), "")
}

func (s *CheckoutSessionService) saveInitializedSession(ctx context.Context, session *models.CheckoutSession) (*CheckoutSessionResponse, error) {
	if err := s.repo.Update(ctx, session); err != nil {
		// A fast provider webhook can commit payment/closure before the create
		// request stores its redirect. The guarded update must not roll that back.
		current, readErr := s.repo.GetByID(ctx, session.ID)
		if readErr == nil && current.CustomerID == session.CustomerID && (current.Status == models.CheckoutSessionStatusSucceeded || current.RailState["provider_closed"] == true) {
			return s.sessionToResponse(current), nil
		}
		return nil, fmt.Errorf("failed to update checkout session: %w", err)
	}
	return s.sessionToResponse(session), nil
}
