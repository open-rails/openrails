package parking

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/internal/pagination"
	"github.com/open-rails/openrails/internal/shared/apperr"
)

// List reads the merchant's parks, newest first.
func List(ctx context.Context, d *db.DB, psps *merchants.Service, p billing.ParkedSubscriptionListParams) (billing.ListPage[billing.ParkedSubscription], error) {
	var out billing.ListPage[billing.ParkedSubscription]
	mid, err := merchant.Require(ctx)
	if err != nil {
		return out, err
	}
	params := gen.ListParkedSubscriptionsParams{MerchantID: mid.UUID(), Released: p.Released}
	if ref := strings.TrimSpace(p.PSP); ref != "" {
		var scope merchants.PSPScope
		found := false
		if id, perr := billing.ParsePSPID(ref); perr == nil {
			scope, found, err = psps.PSPScopeByID(ctx, mid, id.UUID())
		} else {
			scope, found, err = psps.PSPScopeByKey(ctx, mid, ref)
		}
		if err != nil {
			return out, err
		}
		if !found {
			return out, apperr.New(404, "psp_not_found", fmt.Sprintf("PSP %s not found", ref)).WithParam("psp")
		}
		params.PspID = &scope.ID
	}
	switch p.Reason {
	case "":
	case billing.ParkPriceUnmapped, billing.ParkCustomerUnlinked, billing.ParkProductHeld:
		params.Reason = new(string(p.Reason))
	default:
		return out, apperr.Invalidf("reason is price_unmapped, customer_unlinked or product_held").WithParam("reason")
	}
	limit, err := pagination.Limit(p.PageRequest)
	if err != nil {
		return out, err
	}
	if params.AfterAt, params.AfterID, err = pagination.After(p.Cursor); err != nil {
		return out, err
	}
	params.RowLimit = pagination.Fetch(limit)
	rows, err := d.Gen(ctx).ListParkedSubscriptions(ctx, params)
	if err != nil {
		return out, err
	}
	items := make([]billing.ParkedSubscription, 0, len(rows))
	for _, r := range rows {
		item := billing.ParkedSubscription{ID: billing.ParkedSubscriptionID(r.ID), PSP: billing.PSPID(r.PspID), Rail: r.Rail, SubscriptionRef: r.SubscriptionRef,
			Reason: billing.ParkReason(r.Reason), PriceRef: r.PriceRef, CustomerRef: r.CustomerRef, ImportOwned: r.ImportOwned, Evidence: r.Evidence,
			ParkedAt: r.ParkedAt.UTC(), LastSeenAt: r.LastSeenAt.UTC()}
		if r.ReleasedAt != nil {
			item.ReleasedAt = new(r.ReleasedAt.UTC())
		}
		if r.ReleasedBy != nil {
			item.ReleasedBy = new(billing.ParkRelease(*r.ReleasedBy))
		}
		if r.SubscriptionID != nil {
			item.Subscription = new(billing.SubscriptionID(*r.SubscriptionID))
		}
		items = append(items, item)
	}
	return pagination.Cut(items, limit, func(v billing.ParkedSubscription) any {
		return pagination.TimeID{At: v.ParkedAt, ID: uuid.UUID(v.ID)}
	}), nil
}
