package purchasedcredits

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jonboulle/clockwork"
	"github.com/open-rails/openrails/billing"
	identity "github.com/open-rails/openrails/internal/billingidentity"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/modules/grants"
	"github.com/open-rails/openrails/internal/modules/money/ledger"
	"github.com/open-rails/openrails/internal/retention"
	"github.com/open-rails/openrails/internal/shared/moneyutil"
	"github.com/open-rails/openrails/internal/shared/timeutil"
)

// Params contains the accepted payment's immutable benefit and
// cash funding. Expiry is frozen by the checkout, never recomputed on replay.
type Params struct {
	CustomerID  identity.CustomerID
	PaymentID   uuid.UUID
	ProductID   uuid.UUID
	Currency    string
	Amount      int64
	PaidAmount  int64
	StartsAt    time.Time
	ExpiresAt   *time.Time
	Description string
}

// Fund appends one credit lot per successful payment. A DB bound
// to the payment transaction keeps settlement, grant and funding atomic.
func (s *Service) Fund(ctx context.Context, p Params) (uuid.UUID, error) {
	if s == nil || s.db == nil {
		return uuid.Nil, fmt.Errorf("money service not initialized")
	}
	if p.PaymentID == uuid.Nil || p.ProductID == uuid.Nil || p.CustomerID.IsZero() || p.Amount <= 0 || p.PaidAmount <= 0 || p.StartsAt.IsZero() {
		return uuid.Nil, fmt.Errorf("purchased credit requires payment, product, customer, positive amounts and accepted start time")
	}
	p.Currency = strings.ToUpper(strings.TrimSpace(p.Currency))
	if err := moneyutil.ValidateCurrency(p.Currency); err != nil {
		return uuid.Nil, err
	}
	p.StartsAt = p.StartsAt.UTC().Truncate(time.Microsecond)
	if p.ExpiresAt != nil {
		t := p.ExpiresAt.UTC().Truncate(time.Microsecond)
		p.ExpiresAt = &t
		if !t.After(p.StartsAt) {
			return uuid.Nil, fmt.Errorf("credit expiry must follow accepted start")
		}
	}
	mid, err := merchant.Require(ctx)
	if err != nil {
		return uuid.Nil, err
	}
	var result uuid.UUID
	err = s.db.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		q := gen.New(tx)
		balance, _, err := s.lockBalance(ctx, q, mid.UUID(), p.CustomerID.UUID(), p.Currency)
		if err != nil {
			return err
		}
		payment, err := q.GetPaymentByID(ctx, gen.GetPaymentByIDParams{MerchantID: mid.UUID(), ID: p.PaymentID})
		if err != nil {
			return fmt.Errorf("load credit funding payment: %w", err)
		}
		if payment.CustomerID != p.CustomerID.UUID() || payment.Amount != p.PaidAmount || payment.Currency != p.Currency || payment.Status != "completed" || payment.RefundedPaymentID != nil {
			return fmt.Errorf("credit funding does not match completed payment")
		}
		price, err := q.GetPriceByID(ctx, gen.GetPriceByIDParams{MerchantID: mid.UUID(), ID: payment.PriceID})
		if err != nil {
			return fmt.Errorf("load credit purchase price: %w", err)
		}
		if price.ProductID != p.ProductID {
			return fmt.Errorf("credit grant product does not own paid price")
		}
		existing, err := q.GetPurchasedCreditGrant(ctx, gen.GetPurchasedCreditGrantParams{MerchantID: mid.UUID(), PaymentID: p.PaymentID})
		if err == nil {
			var spec grants.Spec
			if err := json.Unmarshal(existing.SpecSnapshot, &spec); err != nil {
				return err
			}
			if existing.CustomerID != p.CustomerID.UUID() || existing.ProductID == nil || *existing.ProductID != p.ProductID || intValue(existing.Amount) != p.Amount || stringValue(existing.Currency) != p.Currency || !existing.StartsAt.Equal(p.StartsAt) || !sameExpiry(existing.EndsAt, p.ExpiresAt) || spec.Deposit == nil || spec.Deposit.PaidAmount == nil || *spec.Deposit.PaidAmount != p.PaidAmount {
				return fmt.Errorf("payment credit terms changed: %w", billing.ErrIdempotencyKeyReused)
			}
			result = existing.ID
			return nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if balance > math.MaxInt64-p.Amount {
			return fmt.Errorf("credit purchase would overflow balance")
		}
		gl := grants.New(q, mid.UUID())
		gl.SetClock(s.now)
		grant, err := gl.Grant(ctx, grants.GrantInput{Customer: p.CustomerID.UUID(), Product: &p.ProductID,
			Kind: grants.Credit, Source: grants.Purchase, SourceID: p.PaymentID.String(), Payment: &p.PaymentID,
			StartsAt: p.StartsAt, EndsAt: p.ExpiresAt, Amount: &p.Amount, Currency: &p.Currency, Reason: &p.Description,
			Spec: &grants.Spec{Deposit: &grants.DepositProvenance{Source: "payment", Invoker: p.CustomerID.String(), PaidAmount: &p.PaidAmount}}})
		if err != nil {
			return err
		}
		if err := gl.MaterializeGrant(ctx, grant); err != nil {
			return err
		}
		result = grant.ID
		return nil
	})
	return result, err
}

// Service posts purchased benefits and their reversals to the existing ledgers.
type Service struct {
	db    *db.DB
	clock clockwork.Clock
}

// New binds purchased-credit accounting to a database or an existing transaction.
func New(d *db.DB, clocks ...clockwork.Clock) *Service {
	return &Service{db: d, clock: timeutil.FirstClock(clocks...)}
}
func (s *Service) now() time.Time { return s.clock.Now().UTC() }
func intValue(p *int64) int64 {
	if p == nil {
		return 0
	}
	return *p
}
func stringValue(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}
func sameExpiry(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Equal(*b)
}
func (s *Service) lockBalance(ctx context.Context, q *gen.Queries, mid, customer uuid.UUID, currency string) (int64, int64, error) {
	if _, err := q.LockCustomerForSpend(ctx, gen.LockCustomerForSpendParams{MerchantID: mid, ID: customer}); err != nil {
		return 0, 0, err
	}
	l := ledger.New(q, mid)
	account, found, err := l.CustomerBalanceAccountID(ctx, customer, currency)
	if err != nil || !found {
		return 0, 0, err
	}
	balance, err := l.Balance(ctx, account)
	if err != nil {
		return 0, 0, err
	}
	now := s.now()
	held, err := q.GetFinancialHeldAmount(ctx, gen.GetFinancialHeldAmountParams{MerchantID: mid, CustomerID: customer, Currency: currency, AsOf: now, HeldSince: now.Add(-retention.AdmissionMaxHold)})
	return balance, held, err
}
