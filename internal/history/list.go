package history

import (
	"context"
	"strings"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/pagination"
	"github.com/open-rails/openrails/internal/shared/apperr"
)

// List reads a merchant's configuration history, newest first.
func List(ctx context.Context, q *gen.Queries, merchantID uuid.UUID, p billing.LogListParams) (billing.ListPage[billing.LogEntry], error) {
	var out billing.ListPage[billing.LogEntry]
	params := gen.ListChangeLogParams{MerchantID: merchantID}
	switch p.ObjectType {
	case "":
	case billing.LogObjectProduct, billing.LogObjectPrice, billing.LogObjectMeter, billing.LogObjectLimit,
		billing.LogObjectMerchant, billing.LogObjectPSP, billing.LogObjectCustodian:
		params.ObjectType = new(string(p.ObjectType))
	default:
		return out, apperr.Invalidf("object_type is product, price, meter, limit, merchant, psp or custodian").WithParam("object_type")
	}
	if id := strings.TrimSpace(p.ObjectID); id != "" {
		if params.ObjectType == nil {
			return out, apperr.Invalidf("object_id needs object_type").WithParam("object_id")
		}
		params.ObjectID = &id
	}
	if !p.Since.IsZero() {
		params.Since = new(p.Since.UTC())
	}
	if !p.Until.IsZero() {
		params.Until = new(p.Until.UTC())
	}
	if params.Since != nil && params.Until != nil && !params.Until.After(*params.Since) {
		return out, apperr.Invalidf("until must be after since").WithParam("until")
	}
	limit, err := pagination.Limit(p.PageRequest)
	if err != nil {
		return out, err
	}
	if params.AfterAt, params.AfterID, err = pagination.After(p.Cursor); err != nil {
		return out, err
	}
	params.RowLimit = pagination.Fetch(limit)
	rows, err := q.ListChangeLog(ctx, params)
	if err != nil {
		return out, err
	}
	items := make([]billing.LogEntry, 0, len(rows))
	for _, r := range rows {
		item := billing.LogEntry{ID: billing.LogEntryID(r.ID), OccurredAt: r.OccurredAt.UTC(), Actor: billing.LogActor(r.Actor), Source: billing.LogSource(r.Source),
			ObjectType: billing.LogObjectType(r.ObjectType), ObjectID: r.ObjectID, Action: billing.LogAction(r.Action), Changes: r.Changes,
			Reason: r.Reason, RequestID: r.RequestID}
		if r.OperationID != nil {
			item.OperationID = new(r.OperationID.String())
		}
		items = append(items, item)
	}
	return pagination.Cut(items, limit, func(v billing.LogEntry) any {
		return pagination.TimeID{At: v.OccurredAt, ID: uuid.UUID(v.ID)}
	}), nil
}
