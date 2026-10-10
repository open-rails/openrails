package money

import (
	"context"
	"fmt"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db/gen"
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
// settings in one read each, then the held total of each balance account.
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
		rows := int32(len(customers) * len(billing.Currencies())) // #nosec G115 -- customers is at most one page (MaxPageLimit), times the currency registry
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
			case ledger.CustomerBalance:
				balance[k], hasBalance[k] = a.Balance, true
			case ledger.ArrearsLiability:
				if a.Balance < 0 {
					owed[k] = -a.Balance
				}
			}
		}
		settings, err := q.ListCustomersMoneySettings(ctx, gen.ListCustomersMoneySettingsParams{MerchantID: mid.UUID(), CustomerIds: customers, RowLimit: rows})
		if err != nil {
			return err
		}
		mode := map[key]string{}
		for _, row := range settings {
			mode[key{row.CustomerID, row.Currency}] = row.BillingMode
			if row.DefaultPaymentMethodID != nil {
				m := out[row.CustomerID]
				m.Defaults = append(m.Defaults, billing.DefaultPaymentMethod{Currency: row.Currency, PaymentMethodID: billing.PaymentMethodID(*row.DefaultPaymentMethodID)})
			}
		}
		for _, c := range currencies {
			k := key{c.CustomerID, c.Currency}
			var held int64
			if hasBalance[k] {
				if held, err = s.heldAmount(ctx, q, mid.UUID(), k.customer, k.currency); err != nil {
					return err
				}
			}
			billingMode := mode[k]
			if billingMode == "" {
				billingMode = BillingModePrepaid
			}
			m := out[k.customer]
			m.Balances = append(m.Balances, billing.Balance{
				CustomerID: billing.CustomerID(k.customer), Currency: k.currency, BillingMode: billing.BillingMode(billingMode),
				BalanceAmount: balance[k], HeldAmount: held, AvailableAmount: balance[k] - held, OwedAmount: owed[k],
			})
		}
		return nil
	})
	return out, err
}
