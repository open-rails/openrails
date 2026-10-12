package db

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/db/gen"
)

// StoredPriceReferences projects pre-cutover receipts and notifications using
// current public identifiers. It never changes stored financial evidence and
// never accepts a legacy identifier from an API request.
func StoredPriceReferences(ctx context.Context, q *gen.Queries, merchantID uuid.UUID, raw []byte) ([]byte, error) {
	if !bytes.Contains(raw, []byte("price_")) {
		return raw, nil
	}
	var document any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&document); err != nil {
		return nil, err
	}
	ids := map[uuid.UUID]string{}
	walkStoredPrices(document, func(id uuid.UUID) string { ids[id] = ""; return "" })
	if len(ids) == 0 {
		return raw, nil
	}
	keys := make([]uuid.UUID, 0, len(ids))
	for id := range ids {
		keys = append(keys, id)
	}
	rows, err := q.ListPriceReferences(ctx, gen.ListPriceReferencesParams{MerchantID: merchantID, Ids: keys})
	if err != nil {
		return nil, err
	}
	for _, row := range rows {
		ids[row.ID] = row.PublicID
	}
	for id, public := range ids {
		if public == "" {
			return nil, fmt.Errorf("stored receipt refers to missing price row %s", id)
		}
	}
	walkStoredPrices(document, func(id uuid.UUID) string { return ids[id] })
	return json.Marshal(document)
}

// walkStoredPrices examines typed identifier fields, including the serialized
// HTTP response body. User-authored metadata is deliberately left verbatim.
func walkStoredPrices(value any, resolve func(uuid.UUID) string) {
	switch value := value.(type) {
	case []any:
		for _, child := range value {
			walkStoredPrices(child, resolve)
		}
	case map[string]any:
		for key, child := range value {
			if key == "metadata" {
				continue
			}
			if key == "id" || key == "price_id" || key == "from_price_id" || key == "to_price_id" {
				if spelling, ok := child.(string); ok && strings.HasPrefix(spelling, "price_") {
					if id, err := uuid.Parse(strings.TrimPrefix(spelling, "price_")); err == nil {
						if public := resolve(id); public != "" {
							value[key] = public
							if key == "id" {
								if version, ok := value["version"].(json.Number); ok {
									if n, err := version.Int64(); err == nil {
										value["version"] = json.Number(strconv.FormatInt(n+1, 10))
									}
								}
							}
						}
					}
				}
			}
			if key == "body" {
				if body, ok := child.(string); ok {
					var decoded any
					decoder := json.NewDecoder(strings.NewReader(body))
					decoder.UseNumber()
					if decoder.Decode(&decoded) == nil {
						walkStoredPrices(decoded, resolve)
						if encoded, err := json.Marshal(decoded); err == nil {
							value[key] = string(encoded)
						}
					}
				}
			} else {
				walkStoredPrices(child, resolve)
			}
		}
	}
}
