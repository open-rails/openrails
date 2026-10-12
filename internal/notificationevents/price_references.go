package notificationevents

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"

	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
)

// projectPriceReferences changes only known top-level selectors in retained
// pre-cutover facts. Unknown kinds, extension fields and application metadata
// remain opaque; the immutable source row is never rewritten.
func projectPriceReferences(ctx context.Context, q *gen.Queries, event gen.BillingNotificationEvent) ([]byte, error) {
	switch event.EventType {
	case "subscription_changed", "subscription_reprice_scheduled", "subscription_plan_change_scheduled":
	default:
		return event.Data, nil
	}
	if !bytes.Contains(event.Data, []byte("price_")) {
		return event.Data, nil
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(event.Data, &fields); err != nil {
		return event.Data, nil // A future payload shape remains consumable.
	}
	refs := make(map[string]json.RawMessage)
	for _, key := range []string{"from_price_id", "to_price_id"} {
		var id string
		if json.Unmarshal(fields[key], &id) == nil && strings.HasPrefix(id, "price_") {
			refs[key] = fields[key]
		}
	}
	if len(refs) == 0 {
		return event.Data, nil
	}
	selectors, err := json.Marshal(refs)
	if err != nil {
		return nil, err
	}
	projected, err := db.StoredPriceReferences(ctx, q, event.MerchantID, selectors)
	if err != nil {
		return nil, err
	}
	if bytes.Equal(projected, selectors) {
		return event.Data, nil
	}
	if err := json.Unmarshal(projected, &refs); err != nil {
		return nil, err
	}
	for key, value := range refs {
		fields[key] = value
	}
	return json.Marshal(fields)
}
