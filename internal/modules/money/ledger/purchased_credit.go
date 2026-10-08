package ledger

import (
	"context"
	"fmt"

	"github.com/google/uuid"
)

// PurchasedDeposit funds a credit lot from actual payment proceeds and, when
// its face value exceeds the price, an explicit merchant promotional expense.
// The caller's transaction owns every leg. Revenue beyond the credit value is
// recognized separately; it must not appear in the customer's prepaid balance.
func (l *Ledger) PurchasedDeposit(ctx context.Context, customer uuid.UUID, currency string, face, paid int64, coord Coord, grantID uuid.UUID) error {
	if face <= 0 || paid <= 0 || grantID == uuid.Nil {
		return fmt.Errorf("purchased credit requires positive face and paid amounts and a grant")
	}
	if _, err := l.Deposit(ctx, customer, currency, min(face, paid), coord, grantID); err != nil {
		return err
	}
	if face == paid {
		return nil
	}
	var debit, credit uuid.UUID
	var err error
	amount, kind := face-paid, DepositBonus
	var balanceCustomer *uuid.UUID
	if face > paid {
		debit, err = l.EnsureSystemAccount(ctx, PromotionalFunding, currency)
		if err == nil {
			credit, err = l.EnsureCustomerBalance(ctx, customer, currency)
		}
		balanceCustomer = &customer
	} else {
		amount, kind = paid-face, CreditPurchaseRevenue
		debit, err = l.EnsureSystemAccount(ctx, RailClearing, currency)
		if err == nil {
			credit, err = l.EnsureSystemAccount(ctx, PlatformRevenue, currency)
		}
	}
	if err != nil {
		return err
	}
	_, err = l.Apply(ctx, Transfer{Debit: debit, Credit: credit, Amount: amount,
		Currency: currency, Type: kind, Coord: coord, GrantID: &grantID, Customer: balanceCustomer})
	return err
}
