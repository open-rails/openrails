package purchasedcredits

import (
	"context"
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
	"github.com/open-rails/openrails/internal/currency"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/modules/money/credits"
	"github.com/open-rails/openrails/internal/modules/money/ledger"
	"github.com/open-rails/openrails/internal/modules/money/owed"
	"github.com/open-rails/openrails/internal/shared/moneyutil"
	"github.com/open-rails/openrails/internal/shared/timeutil"
)

// Params contains the accepted payment's immutable benefit and
// cash funding. Expiry is frozen by the purchase, never recomputed on replay.
type Params struct {
	CustomerID identity.CustomerID
	PaymentID  uuid.UUID
	ProductID  uuid.UUID
	// InvoiceLineID is the accepted invoice line that funded the credit.
	InvoiceLineID uuid.UUID
	Currency      string
	Amount        int64
	PaidAmount    int64
	StartsAt      time.Time
	ExpiresAt     *time.Time
	Description   string
}

// Fund appends one credit lot per successful payment. A DB bound
// to the payment transaction keeps settlement, grant and funding atomic.
func (s *Service) Fund(ctx context.Context, p Params) (uuid.UUID, error) {
	if s == nil || s.db == nil {
		return uuid.Nil, fmt.Errorf("money service not initialized")
	}
	if p.InvoiceLineID == uuid.Nil || p.PaymentID == uuid.Nil || p.ProductID == uuid.Nil || p.CustomerID.IsZero() || p.Amount <= 0 || p.PaidAmount <= 0 || p.StartsAt.IsZero() {
		return uuid.Nil, fmt.Errorf("purchased credit requires payment, product, customer, positive amounts and accepted start time")
	}
	p.Currency = strings.ToUpper(strings.TrimSpace(p.Currency))
	if err := moneyutil.ValidateBalanceCurrency(p.Currency); err != nil {
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
		// A lot is in its payment's currency, or is a pack of a merchant's
		// unit that payment bought.
		inUnit := payment.Currency != p.Currency
		if payment.CustomerID != p.CustomerID.UUID() || inUnit && !currency.IsUnitCode(p.Currency) || payment.Status != "succeeded" || payment.RefundedPaymentID != nil {
			return fmt.Errorf("credit funding does not match completed payment")
		}
		line, err := q.GetInvoiceLine(ctx, gen.GetInvoiceLineParams{MerchantID: mid.UUID(), ID: p.InvoiceLineID})
		if err != nil {
			return fmt.Errorf("load credit invoice line: %w", err)
		}
		if payment.InvoiceID == nil || line.InvoiceID != *payment.InvoiceID || line.ProductID == nil || *line.ProductID != p.ProductID || line.Amount != p.PaidAmount {
			return fmt.Errorf("credit funding does not match its invoice line")
		}
		existing, err := q.GetPurchasedCreditGrant(ctx, gen.GetPurchasedCreditGrantParams{MerchantID: mid.UUID(), PaymentID: p.PaymentID})
		if err == nil {
			if existing.CustomerID != p.CustomerID.UUID() || existing.ProductID == nil || *existing.ProductID != p.ProductID || existing.Units != p.Amount || existing.Unit != p.Currency || !existing.StartsAt.Equal(p.StartsAt) || !sameExpiry(existing.ExpiresAt, p.ExpiresAt) || intValue(existing.PaidAmount) != p.PaidAmount {
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
		// A recurring price's line delivers its cycle's credit.
		funding := credits.Purchase
		if line.BillingIntervalHours != nil || line.BillingIntervalMonths != nil {
			funding = credits.Recurring
		}
		var description *string
		if p.Description != "" {
			description = &p.Description
		}
		grant, err := credits.New(q, mid.UUID(), s.now).Issue(ctx, credits.Issue{
			Customer: p.CustomerID.UUID(), Unit: p.Currency, Units: p.Amount, Funding: funding,
			PaidAmount: &p.PaidAmount, PaidCurrency: &payment.Currency,
			Source: "payment", SourceID: p.PaymentID.String(), Invoker: p.CustomerID.String(),
			Product: &p.ProductID, InvoiceLine: &p.InvoiceLineID, Payment: &p.PaymentID, Description: description,
			StartsAt: p.StartsAt, ExpiresAt: p.ExpiresAt,
		})
		if err != nil {
			return err
		}
		// Only the paid part repays debt; promotional value never does. Nothing
		// is owed in a merchant's unit.
		if !inUnit {
			if _, err := owed.Repay(ctx, s.db.NewWithPgxTx(tx), mid.UUID(), grant, min(p.Amount, p.PaidAmount), s.now()); err != nil {
				return err
			}
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
func sameExpiry(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Equal(*b)
}

// lockBalance takes the customer's spend lock and reads its balance in the
// currency and what the ledger has not taken of it: pending usage and
// running charges' accrual.
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
	unposted, err := q.GetUnpostedAmounts(ctx, gen.GetUnpostedAmountsParams{MerchantID: mid, CustomerID: customer, Currency: currency, AsOf: s.now()})
	if err != nil {
		return 0, 0, err
	}
	return balance, unposted.Pending + unposted.Accrued, nil
}
