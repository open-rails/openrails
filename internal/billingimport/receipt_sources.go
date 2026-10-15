package billingimport

import (
	"strings"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/reconcile"
	"github.com/open-rails/openrails/internal/shared/apperr"
)

func validateReceiptSources(book billing.DeclaredBilling, history *reconcile.DeclaredHistory) error {
	sources := map[string]bool{}
	for _, s := range book.Subscriptions {
		sources[s.SourceID] = true
	}
	for _, p := range book.Purchases {
		sources[p.SourceID] = true
	}
	for _, p := range book.HistoricalPayments {
		sources[p.SourceID] = true
	}
	for _, g := range book.AdminGrants {
		sources[g.SourceID] = true
	}
	for _, t := range book.Transactions {
		if t.SourceID == "" && t.OriginalTransactionID == "" {
			continue
		}
		var matches int
		for _, s := range book.Subscriptions {
			if s.RailSubscriptionID == t.RailSubscriptionID && s.Rail == "nmi" {
				matches++
			}
			if s.RailSubscriptionID == t.RailSubscriptionID && s.Rail != "nmi" {
				return apperr.Invalidf("named receipts and original transaction links require NMI")
			}
		}
		if matches != 1 {
			return apperr.Invalidf("receipt %s requires one unambiguous NMI subscription", t.TransactionID)
		}
		if t.SourceID != "" && t.Type == "refund" && t.Success && t.OriginalTransactionID == "" {
			return apperr.Invalidf("named refund %s requires its original payment ID", t.TransactionID)
		}
		if t.OriginalTransactionID != "" {
			if t.Success {
				if !providerTransactionID("nmi", t.TransactionID) || !providerTransactionID("nmi", t.OriginalTransactionID) || t.TransactionID == t.OriginalTransactionID {
					return apperr.Invalidf("refund requires distinct NMI provider transaction IDs")
				}
				matched := false
				for _, charge := range book.Transactions {
					if charge.TransactionID == t.OriginalTransactionID && charge.RailSubscriptionID == t.RailSubscriptionID && charge.Success && (charge.Type == "" || charge.Type == "sale") && charge.Amount > 0 {
						matched = true
					}
				}
				if !matched {
					return apperr.Invalidf("refund %s requires its original successful payment in the same subscription group", t.TransactionID)
				}
			}
			if t.Type != "refund" {
				return apperr.Invalidf("original_transaction_id is supported only for refund receipts")
			}
			history.Track(t.RailSubscriptionID, t.OriginalTransactionID)
			history.Track(t.RailSubscriptionID, t.TransactionID)
		}
		if t.SourceID != "" {
			if strings.TrimSpace(t.SourceID) != t.SourceID || sources[t.SourceID] {
				return apperr.Invalidf("receipt source_id must be unique and unpadded")
			}
			sources[t.SourceID] = true
			history.Track(t.RailSubscriptionID, t.TransactionID)
		}
	}
	if history.HasReceipts() {
		seen := map[string]bool{}
		for _, t := range book.Transactions {
			key := t.RailSubscriptionID + "\x1f" + t.TransactionID
			if seen[key] {
				return apperr.Invalidf("duplicate subscription receipt %s", t.TransactionID)
			}
			seen[key] = true
		}
	}
	return nil
}
