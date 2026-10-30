package billingimport

import (
	"context"
	"slices"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db/gen"
)

// respellHeld names each subscription of the book that OpenRails holds as its
// row spells it, in the book's subscriptions and charges. Rows match by
// identity (a CCBill id is a number), and a row keeps the provider's spelling
// it took on contact where a legacy book lost a leading zero. The caller's
// book is not changed.
func respellHeld(ctx context.Context, q *gen.Queries, merchantID uuid.UUID, book billing.DeclaredBilling) (billing.DeclaredBilling, error) {
	psps, err := newPSPResolver(ctx, q, merchantID, book.DefaultPSP)
	if err != nil {
		return book, err
	}
	var p gen.ListRespelledSubscriptionsParams
	for _, s := range book.Subscriptions {
		if s.RailSubscriptionID == "" || s.Rail == "" {
			continue
		}
		psp, err := psps.resolve(s.PSP, s.Rail, "subscription "+s.SourceID)
		if err != nil {
			continue // the import refuses it with its reason
		}
		p.PspIds, p.Rails, p.Refs = append(p.PspIds, psp), append(p.Rails, s.Rail), append(p.Refs, s.RailSubscriptionID)
	}
	if len(p.Refs) == 0 {
		return book, nil
	}
	p.MerchantID = merchantID
	rows, err := q.ListRespelledSubscriptions(ctx, p)
	if err != nil || len(rows) == 0 {
		return book, err
	}
	spelled := make(map[string]string, len(rows))
	for _, r := range rows {
		spelled[r.Ref] = r.Spelled
	}
	book.Subscriptions = slices.Clone(book.Subscriptions)
	for i, s := range book.Subscriptions {
		if stored, ok := spelled[s.RailSubscriptionID]; ok {
			book.Subscriptions[i].RailSubscriptionID = stored
		}
	}
	book.Transactions = slices.Clone(book.Transactions)
	for i, t := range book.Transactions {
		if stored, ok := spelled[t.RailSubscriptionID]; ok {
			book.Transactions[i].RailSubscriptionID = stored
		}
	}
	return book, nil
}
