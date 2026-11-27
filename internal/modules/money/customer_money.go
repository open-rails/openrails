package money

import (
	"context"
	"fmt"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/catalog"
	identity "github.com/open-rails/openrails/internal/billingidentity"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/modules/money/ledger"
)

// CustomerMoney is a customer's money per currency: its balance in every
// currency it holds money, settings or debt in, and the card that collects
// each currency's invoices.
type CustomerMoney struct {
	Balances []billing.Balance
	Defaults []billing.DefaultPaymentMethod
}

// CustomersMoney reads the named customers' money: the ledger counters and
// settings in one read each, then each balance account's pending usage and
// accrual, and the run-out of each that a running charge draws.
func (s *MoneyService) CustomersMoney(ctx context.Context, customers []uuid.UUID) (map[uuid.UUID]*CustomerMoney, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("money service not initialized")
	}
	mid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	out := make(map[uuid.UUID]*CustomerMoney, len(customers))
	for _, id := range customers {
		out[id] = &CustomerMoney{Balances: []billing.Balance{}, Defaults: []billing.DefaultPaymentMethod{}}
	}
	if len(customers) == 0 {
		return out, nil
	}
	err = s.db.RunInMerchantConn(ctx, func(ctx context.Context) error {
		q := s.db.Gen(ctx)
		type key struct {
			customer uuid.UUID
			currency string
		}
		rows := int32(len(customers) * len(catalog.Currencies())) // #nosec G115 -- customers is at most one page (MaxPageLimit), times the currency registry
		currencies, err := q.ListCustomersBalanceCurrencies(ctx, gen.ListCustomersBalanceCurrenciesParams{MerchantID: mid.UUID(), CustomerIds: customers, RowLimit: rows})
		if err != nil {
			return err
		}
		accounts, err := q.ListCustomersMoneyAccounts(ctx, gen.ListCustomersMoneyAccountsParams{MerchantID: mid.UUID(), CustomerIds: customers, RowLimit: 2 * rows})
		if err != nil {
			return err
		}
		balance, owed, hasBalance := map[key]int64{}, map[key]int64{}, map[key]bool{}
		for _, a := range accounts {
			k := key{a.CustomerID, a.Currency}
			switch ledger.AccountType(a.AccountType) {
			case ledger.CustomerUnits:
				balance[k], hasBalance[k] = a.Balance, true
			case ledger.CustomerOwed:
				if a.Balance < 0 {
					owed[k] = -a.Balance
				}
			}
		}
		settings, err := q.ListCustomersMoneySettings(ctx, gen.ListCustomersMoneySettingsParams{MerchantID: mid.UUID(), CustomerIds: customers, RowLimit: rows})
		if err != nil {
			return err
		}
		settingsByKey := map[key]gen.BillingMoneySetting{}
		for _, row := range settings {
			settingsByKey[key{row.CustomerID, row.Currency}] = row
			if row.DefaultPaymentMethodID != nil {
				m := out[row.CustomerID]
				m.Defaults = append(m.Defaults, billing.DefaultPaymentMethod{Currency: catalog.Currency(row.Currency), PaymentMethodID: billing.PaymentMethodID(*row.DefaultPaymentMethodID)})
			}
		}
		now := instant(s.now())
		for _, c := range currencies {
			k := key{c.CustomerID, c.Currency}
			bal := &models.MoneyBalance{MerchantID: mid.UUID(), CustomerID: k.customer, Currency: k.currency, Balance: balance[k], AsOf: now}
			if hasBalance[k] {
				row, err := q.GetUnpostedAmounts(ctx, gen.GetUnpostedAmountsParams{MerchantID: mid.UUID(), CustomerID: k.customer, Currency: k.currency, AsOf: now})
				if err != nil {
					return err
				}
				bal.Pending, bal.Accrued = row.Pending, row.Accrued
			}
			billingMode := BillingModePrepaid
			if !IsMerchantUnit(k.currency) {
				row, ok := settingsByKey[k]
				if !ok {
					row = gen.BillingMoneySetting{MerchantID: mid.UUID(), CustomerID: k.customer, Currency: k.currency, BillingMode: BillingModePrepaid}
				}
				billingMode, err = s.usageAdmissionBillingMode(ctx, row)
				if err != nil {
					return err
				}
			}
			b, err := s.PublicBalance(ctx, bal, billing.BillingMode(billingMode), owed[k])
			if err != nil {
				return err
			}
			m := out[k.customer]
			m.Balances = append(m.Balances, *b)
		}
		return nil
	})
	return out, err
}

// BalanceLimits are the customer's built-in limit balance in each currency
// it holds a balance in, as admission sees it: capacity is the balance plus
// the credit line usage billing gives (none when prepaid), used what it owes,
// and held its pending usage and running charges' accrual. Display values: a
// billing policy's own cap is not applied.
func (s *MoneyService) BalanceLimits(ctx context.Context, q *gen.Queries, customer uuid.UUID) ([]billing.CustomerLimit, error) {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	accounts, err := q.ListCustomersMoneyAccounts(ctx, gen.ListCustomersMoneyAccountsParams{MerchantID: mid.UUID(), CustomerIds: []uuid.UUID{customer}, RowLimit: int32(2 * len(catalog.Currencies()))}) // #nosec G115 -- twice the currency registry
	if err != nil {
		return nil, err
	}
	now := s.now()
	out := []billing.CustomerLimit{}
	for _, a := range accounts {
		if ledger.AccountType(a.AccountType) != ledger.CustomerUnits {
			continue
		}
		row, err := q.GetAdmissionCapacity(ctx, gen.GetAdmissionCapacityParams{MerchantID: mid.UUID(), CustomerID: customer, Currency: a.Currency, AsOf: now})
		if err != nil {
			return nil, err
		}
		capacity, err := s.usageAdmissionCapacity(ctx, q, identity.CustomerID(customer), a.Currency, admissionCapacityFromRow(row))
		if err != nil {
			return nil, err
		}
		line := billing.CustomerLimit{CustomerID: billing.CustomerID(customer), Limit: catalog.BalanceLimit, Unit: a.Currency,
			Capacity: capacity.Balance, Used: capacity.OutstandingOwed, Held: capacity.Unposted()}
		if capacity.BillingMode == BillingModeArrears && capacity.CreditLimit > 0 {
			line.Capacity += capacity.CreditLimit
		}
		line.Remaining = line.Capacity - line.Used - line.Held
		out = append(out, line)
	}
	return out, nil
}
