package reconcile

import (
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"time"
)

// stripeBillFact is the invoice obligation, separate from its real captures.
// A retry counter or paid flag without a verified invoice is not this fact.
func stripeBillFact(bill subscriptions.StripeBill) RemoteTransaction {
	var start, end time.Time
	for _, line := range bill.Lines {
		if !line.PeriodStart.IsZero() && line.PeriodEnd.After(line.PeriodStart) {
			if start.IsZero() || line.PeriodStart.Before(start) {
				start = line.PeriodStart
			}
			if line.PeriodEnd.After(end) {
				end = line.PeriodEnd
			}
		}
	}
	fact := RemoteTransaction{TransactionID: bill.ID, SubscriptionID: bill.SubscriptionID, Type: TransactionTypeSettlement,
		Success: bill.Status == "paid" && bill.AmountRemaining == 0, Currency: bill.Currency, OccurredAt: start, StripeBill: &bill,
		BillPeriodStart: &start, BillPeriodEnd: &end}
	// An adjustment never grants another subscription period.
	if bill.Reason != "subscription_create" && bill.Reason != "subscription_cycle" {
		fact.SubscriptionID = ""
	}
	return fact
}
