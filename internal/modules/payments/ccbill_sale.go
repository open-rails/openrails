package payments

import (
	"time"
)

// CCBillSale is a CCBill NewSaleSuccess for an order attempt, as CCBill
// reported it: the subscription CCBill now bills, its first transaction,
// the FlexForm sold and the amounts in rail minor units.
type CCBillSale struct {
	SubscriptionID           string
	TransactionID            string
	Username                 string
	FlexID                   string
	RecurringBillingOptionID string
	// Currency is the ISO alpha code of the billed amounts.
	Currency       string
	InitialMinor   int64
	RecurringMinor *int64
	SoldAt         *time.Time
	NextRenewalAt  *time.Time
}

// CCBillSaleUnhonored is a CCBill sale OpenRails keeps no record of: the
// order it names was paid otherwise or no longer sells, or the sale
// contradicts the attempt. CCBill holds the money, so the merchant refunds
// and cancels it there.
type CCBillSaleUnhonored struct {
	Reason string
	Detail string
}

func (e *CCBillSaleUnhonored) Error() string {
	return "ccbill sale unhonored (" + e.Reason + "): " + e.Detail
}
