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
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/modules/catalog"
	"github.com/open-rails/openrails/internal/modules/entitlements"
	"github.com/open-rails/openrails/internal/modules/grants"
	"github.com/open-rails/openrails/internal/modules/payments"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/internal/shared/uuidutil"
)

const acceptedPurchaseTermsKey = "accepted_purchase"

// Stored in the existing checkout attempt, alongside its buyer and PSP. Money
// stays a decimal string even when rail_state is decoded through map[string]any.
type acceptedPurchaseTerms struct {
	LegacyEntitlements  map[string]*int              `json:"legacy_entitlements,omitzero"`
	PriceID             uuid.UUID                    `json:"price_id"`
	ProductID           uuid.UUID                    `json:"product_id"`
	PaymentID           uuid.UUID                    `json:"payment_id"`
	ProductKey          string                       `json:"product_key"`
	ProductName         string                       `json:"product_name"`
	Amount              int64                        `json:"amount,string"`
	Currency            string                       `json:"currency"`
	AccessDurationHours *int                         `json:"access_duration_hours"`
	CreditGrant         *models.CreditGrantSnapshot  `json:"credit_grant"`
	Entitlements        []string                     `json:"entitlements"`
	PSPLinks            map[string]map[string]string `json:"psp_links,omitempty"`
	AcceptedAt          time.Time                    `json:"accepted_at"`
	EntitlementStart    time.Time                    `json:"entitlement_start"`
}

func (t acceptedPurchaseTerms) catalog(merchantID uuid.UUID) (*models.Price, *models.Product) {
	return &models.Price{ID: t.PriceID, MerchantID: merchantID, ProductID: t.ProductID, Amount: t.Amount, Currency: t.Currency, AccessDurationHours: t.AccessDurationHours, PSPLinks: t.PSPLinks},
		&models.Product{ID: t.ProductID, MerchantID: merchantID, Key: t.ProductKey, DisplayName: t.ProductName, Entitlements: models.CloneEntitlements(t.Entitlements)}
}

func purchaseTerms(session *models.CheckoutAttempt) (*acceptedPurchaseTerms, error) {
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
	if session.Mode != models.CheckoutAttemptModeOneOff || session.PriceID == nil || terms.PriceID != *session.PriceID || terms.ProductID == uuid.Nil || terms.PaymentID == uuid.Nil || session.Amount == nil || terms.Amount != *session.Amount || session.Currency == nil || terms.Currency != *session.Currency || terms.AcceptedAt.IsZero() || terms.EntitlementStart.Before(terms.AcceptedAt) || terms.AccessDurationHours != nil && *terms.AccessDurationHours <= 0 {
		return nil, errors.New("checkout accepted purchase terms contradict session")
	}
	if err := terms.CreditGrant.Validate(); err != nil {
		return nil, err
	}
	return &terms, nil
}

func permanentPurchase(price *models.Price) bool {
	return price != nil && !price.IsRecurring() && price.AccessDurationHours == nil
}

func (s *CheckoutPurchaseService) permanentCoverage(ctx context.Context, user string, product *models.Product, pending bool, except uuid.UUID) (*CoverageInfo, error) {
	keys := product.Entitlements
	coverage := &CoverageInfo{}
	if len(keys) == 0 {
		return coverage, nil
	}
	mid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	customer, err := customerIDFromUser(user)
	if err != nil {
		return nil, err
	}
	covered, err := s.database.Gen(ctx).PermanentBenefitsCovered(ctx, gen.PermanentBenefitsCoveredParams{MerchantID: mid.UUID(), CustomerID: customer, Entitlements: keys, AtTime: s.now(), IncludePending: pending, ExceptSessionID: except})
	if err != nil {
		return nil, err
	}
	if covered != nil && *covered {
		coverage.HasCoverage = true
		coverage.IsIndefinite = true
		coverage.SourceType = "entitlement"
	}
	return coverage, nil
}

func (s *CheckoutPurchaseService) purchaseCoverage(ctx context.Context, user string, price *models.Price, product *models.Product) (*CoverageInfo, error) {
	if product.CreditGrant != nil {
		return &CoverageInfo{}, nil
	}
	if permanentPurchase(price) && s.database != nil {
		return s.permanentCoverage(ctx, user, product, false, uuid.Nil)
	}
	return s.GetUserProductCoverage(ctx, user, product)
}

func (s *CheckoutPurchaseService) checkPermanentOwnership(ctx context.Context, user string, price *models.Price, product *models.Product) error {
	if !permanentPurchase(price) || product.CreditGrant != nil || s.database == nil {
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
		return fmt.Errorf("%w: customer already owns this product", ErrCheckoutAttemptConflict)
	}
	return nil
}

// admitPurchaseSession publishes the immutable terms and the exclusion together.
// No provider request runs while the customer or catalog rows are locked.
func (s *CheckoutAttemptService) admitPurchaseSession(ctx context.Context, session *models.CheckoutAttempt) error {
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
				return fmt.Errorf("%w: offer is no longer available", ErrCheckoutAttemptValidation)
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
		if price.CustomerAmount != nil {
			price, err = CheckoutPriceForAmount(price, session.Amount)
			if err != nil {
				return err
			}
		}
		if price.IsRecurring() || session.Amount == nil || *session.Amount != price.Amount || session.Currency == nil || *session.Currency != price.Currency {
			return fmt.Errorf("%w: purchase terms changed", ErrCheckoutAttemptConflict)
		}
		key, _ := session.RailState["requested_entitlement"].(string)
		kind, _ := session.RailState["requested_offer_kind"].(string)
		if err := validateOfferAssertion(price, product, key, billing.OfferKind(kind)); err != nil {
			return err
		}
		purchase := NewCheckoutPurchaseService(catalog.NewPriceService(d), catalog.NewProductService(d), payments.NewPaymentService(d, s.clock), entitlements.NewEntitlementService(d, s.clock), nil, s.clock)
		purchase.SubscriptionService = subscriptions.NewSubscriptionService(d, purchase.PriceService, purchase.ProductService, nil, s.clock)
		eligibility, err := purchase.CheckPurchaseEligibility(ctx, session.CustomerID.String(), price.ID)
		if err != nil {
			return err
		}
		if eligibility.Status != EligibilityAllowed {
			return fmt.Errorf("%w: %s", ErrCheckoutAttemptConflict, eligibility.Reason)
		}
		if permanentPurchase(price) && product.CreditGrant == nil {
			coverage, err := purchase.permanentCoverage(ctx, session.CustomerID.String(), product, true, session.ID)
			if err != nil {
				return err
			}
			if coverage.HasCoverage {
				return fmt.Errorf("%w: all permanent benefits are already owned or reserved", ErrCheckoutAttemptConflict)
			}
			pending, err := d.Gen(ctx).HasUnresolvedProductCheckout(ctx, gen.HasUnresolvedProductCheckoutParams{MerchantID: mid.UUID(), CustomerID: session.CustomerID, ProductID: product.ID, ExceptSessionID: session.ID})
			if err != nil {
				return err
			}
			if pending {
				return fmt.Errorf("%w: another checkout for this product is unresolved", ErrCheckoutAttemptConflict)
			}
			_, err = d.Gen(ctx).GetUnresolvedSaleForCustomerProduct(ctx, gen.GetUnresolvedSaleForCustomerProductParams{MerchantID: mid.UUID(), CustomerID: session.CustomerID.String(), ProductID: product.ID.String()})
			if err == nil {
				return fmt.Errorf("%w: another purchase for this product is unresolved", ErrCheckoutAttemptConflict)
			}
			if !db.IsNotFound(err) {
				return err
			}
		}
		now := s.now().UTC().Truncate(time.Microsecond)
		terms := acceptedPurchaseTerms{PriceID: price.ID, ProductID: product.ID, PaymentID: uuidutil.NewV7(), ProductKey: product.Key, ProductName: product.DisplayName, Amount: price.Amount, Currency: price.Currency, AccessDurationHours: price.AccessDurationHours, Entitlements: models.CloneEntitlements(product.Entitlements), PSPLinks: price.PSPLinks, AcceptedAt: now, EntitlementStart: now}
		terms.CreditGrant, err = acceptedCreditGrant(product, price)
		if err != nil {
			return err
		}
		if eligibility.Coverage != nil && eligibility.Coverage.EndDate != nil && eligibility.Coverage.EndDate.After(now) {
			terms.EntitlementStart = eligibility.Coverage.EndDate.UTC().Truncate(time.Microsecond)
		}
		if session.RailState == nil {
			session.RailState = map[string]any{}
		}
		session.RailState[acceptedPurchaseTermsKey] = terms
		return NewCheckoutAttemptRepo(d).Create(ctx, session)
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
		session, err := NewCheckoutAttemptRepo(d).GetByID(ctx, req.CheckoutAttemptID)
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
			legacy.CheckoutAttemptID = uuid.Nil
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
		result, err = bound.applyPurchase(ctx, req, price, product, &EligibilityResult{Status: EligibilityAllowed, Coverage: coverage}, terms.AcceptedAt, terms.PaymentID, terms.CreditGrant, grants.HistoricalEntitlementHours(terms.LegacyEntitlements))
		if err != nil {
			return err
		}
		session.Status = models.CheckoutAttemptStatusSucceeded
		session.PaymentID = &result.PaymentID
		session.TransactionID = &req.TransactionID
		session.UpdatedAt = s.now()
		return NewCheckoutAttemptRepo(d).Update(ctx, session)
	})
	return result, err
}

// MarkProviderCheckoutClosed records authoritative Stripe closure. Local TTL
// expiry uses MarkExpired and never releases an uncertain provider purchase.
func (s *CheckoutAttemptService) MarkProviderCheckoutClosed(ctx context.Context, id uuid.UUID, status models.CheckoutAttemptStatus) error {
	if status != models.CheckoutAttemptStatusExpired && status != models.CheckoutAttemptStatusFailed {
		return ErrCheckoutAttemptValidation
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
		return ErrCheckoutAttemptNotFound
	}
	return nil
}

func (s *CheckoutAttemptService) saveInitializedSession(ctx context.Context, session *models.CheckoutAttempt) (*CheckoutAttemptResponse, error) {
	if err := s.repo.Update(ctx, session); err != nil {
		// A fast provider webhook can commit payment/closure before the create
		// request stores its redirect. The guarded update must not roll that back.
		current, readErr := s.repo.GetByID(ctx, session.ID)
		if readErr == nil && current.CustomerID == session.CustomerID && (current.Status == models.CheckoutAttemptStatusSucceeded || current.RailState["provider_closed"] == true) {
			return s.sessionToResponse(current), nil
		}
		return nil, fmt.Errorf("failed to update checkout attempt: %w", err)
	}
	return s.sessionToResponse(session), nil
}

func (p *acceptedPurchaseTerms) UnmarshalJSON(data []byte) error {
	type plain acceptedPurchaseTerms
	var decoded plain
	wire := struct {
		*plain
		Entitlements json.RawMessage `json:"entitlements"`
	}{plain: &decoded}
	if err := json.Unmarshal(data, &wire); err != nil {
		return err
	}
	var err error
	decoded.Entitlements, decoded.LegacyEntitlements, err = grants.DecodeAcceptedEntitlements(wire.Entitlements, decoded.LegacyEntitlements)
	if err != nil {
		return err
	}
	*p = acceptedPurchaseTerms(decoded)
	return nil
}
