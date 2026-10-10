package reconcile

import "strings"

// Stripe checkout.session.completed records the PaymentIntent, while native
// renewal collection records its charge. Alias only Stripe's explicit relation
// within this account's snapshot, with consistent financial terms. A customer's
// identity alone never turns a native payment into a subscription backfill.
func stripePaymentAliases(snapshot *RemoteSnapshot, payments map[string]*LocalPayment) map[string]bool {
	conflicts := map[string]bool{}
	charges := map[string]string{}
	for _, transaction := range snapshot.Transactions {
		if transaction.Type != TransactionTypeSale || !transaction.Success {
			continue
		}
		intent := decodeBreadcrumbs(transaction.Raw).PaymentIntent
		if intent == "" {
			continue
		}
		if prior := charges[intent]; prior != "" && prior != transaction.TransactionID {
			conflicts[intent] = true
		}
		charges[intent] = transaction.TransactionID
	}
	for _, transaction := range snapshot.Transactions {
		if transaction.Type != TransactionTypeSale || !transaction.Success {
			continue
		}
		intent := decodeBreadcrumbs(transaction.Raw).PaymentIntent
		payment := payments[intent]
		if payment == nil {
			continue
		}
		charge := payments[transaction.TransactionID]
		if conflicts[intent] || (charge != nil && charge.ID != payment.ID) ||
			payment.AmountCents != transaction.AmountCents || !strings.EqualFold(payment.Currency, transaction.Currency) ||
			(payment.Status != "succeeded" && payment.Status != "refunded") {
			conflicts[transaction.TransactionID], conflicts[intent] = true, true
			continue
		}
		payments[transaction.TransactionID] = payment
	}
	return conflicts
}
