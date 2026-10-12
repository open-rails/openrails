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
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/modules/grants"
	"github.com/open-rails/openrails/internal/modules/payments"
)

const acceptedPurchaseTermsKey = "accepted_purchase"

// Stored in the existing checkout attempt, alongside its buyer and PSP. Money
// stays a decimal string even when rail_state is decoded through map[string]any.
type acceptedPurchaseTerms struct {
	LegacyEntitlements  map[string]*int             `json:"legacy_entitlements,omitzero"`
	PriceID             uuid.UUID                   `json:"price_id"`
	ProductID           uuid.UUID                   `json:"product_id"`
	PaymentID           uuid.UUID                   `json:"payment_id"`
	ProductKey          string                      `json:"product_key"`
	ProductName         string                      `json:"product_name"`
	Amount              int64                       `json:"amount,string"`
	Currency            string                      `json:"currency"`
	AccessDurationHours *int                        `json:"access_duration_hours"`
	CreditGrant         *models.CreditGrantSnapshot `json:"credit_grant"`
	// Entitlements is the keys a checkout admitted before product access, kept
	// only as accepted evidence. Settlement grants ProductID.
	Entitlements     json.RawMessage              `json:"entitlements,omitempty"`
	PSPLinks         map[string]map[string]string `json:"psp_links,omitempty"`
	AcceptedAt       time.Time                    `json:"accepted_at"`
	EntitlementStart time.Time                    `json:"entitlement_start"`
}

func (t acceptedPurchaseTerms) catalog(merchantID uuid.UUID) (*models.Price, *models.Product) {
	return &models.Price{ID: t.PriceID, MerchantID: merchantID, ProductID: t.ProductID, Amount: t.Amount, Currency: t.Currency, AccessDurationHours: t.AccessDurationHours, PSPLinks: t.PSPLinks},
		&models.Product{ID: t.ProductID, MerchantID: merchantID, Key: t.ProductKey, DisplayName: t.ProductName}
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
	if (session.OrderID != nil || session.AcceptedSubscription) || session.AcceptedPriceID == nil || terms.PriceID != *session.AcceptedPriceID || terms.ProductID == uuid.Nil || terms.PaymentID == uuid.Nil || session.Amount == nil || terms.Amount != *session.Amount || session.Currency == nil || terms.Currency != *session.Currency || terms.AcceptedAt.IsZero() || terms.EntitlementStart.Before(terms.AcceptedAt) || terms.AccessDurationHours != nil && *terms.AccessDurationHours <= 0 {
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

func (s *CheckoutPurchaseService) permanentCoverage(ctx context.Context, user string, product *models.Product, pending bool) (*CoverageInfo, error) {
	coverage := &CoverageInfo{}
	mid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	customer, err := customerIDFromUser(user)
	if err != nil {
		return nil, err
	}
	covered, err := s.database.Gen(ctx).PermanentBenefitsCovered(ctx, gen.PermanentBenefitsCoveredParams{MerchantID: mid.UUID(), CustomerID: customer, ProductID: product.ID, AtTime: s.now(), IncludePending: pending})
	if err != nil {
		return nil, err
	}
	if covered != nil && *covered {
		coverage.HasCoverage = true
		coverage.IsIndefinite = true
		coverage.SourceType = "product"
	}
	return coverage, nil
}

func (s *CheckoutPurchaseService) purchaseCoverage(ctx context.Context, user string, price *models.Price, product *models.Product) (*CoverageInfo, error) {
	if product.CreditGrant != nil {
		return &CoverageInfo{}, nil
	}
	if permanentPurchase(price) && s.database != nil {
		return s.permanentCoverage(ctx, user, product, false)
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
	owned, err := s.database.Gen(ctx).HasPermanentProductAccess(ctx, gen.HasPermanentProductAccessParams{MerchantID: mid.UUID(), CustomerID: customer, ProductID: product.ID, AtTime: s.now()})
	if err != nil {
		return err
	}
	if owned {
		return fmt.Errorf("%w: customer already owns this product", ErrCheckoutAttemptConflict)
	}
	return nil
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
		if session.CustomerID != customer || session.AcceptedPriceID == nil || *session.AcceptedPriceID != req.PriceID || string(session.Rail) != req.Rail || session.PspID != db.PSPIDFromContext(ctx) || req.SubscriptionID != nil {
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
		result, err = bound.applyPurchase(ctx, req, price, product, &EligibilityResult{Status: EligibilityAllowed, Coverage: coverage}, terms.AcceptedAt, terms.PaymentID, terms.CreditGrant)
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

func (p *acceptedPurchaseTerms) UnmarshalJSON(data []byte) error {
	type plain acceptedPurchaseTerms
	var decoded plain
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	if _, _, err := grants.DecodeAcceptedEntitlements(decoded.Entitlements, decoded.LegacyEntitlements); err != nil {
		return err
	}
	*p = acceptedPurchaseTerms(decoded)
	return nil
}
