package service

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/modules/orders"
)

// orderCreationDigest keeps the established request digest independent of a
// price's public spelling. Private UUIDs never changed in the cutover, so this
// reproduces pre-cutover hashes without rewriting durable receipts. Only the
// known price_id selectors in creation params change; field order, quantities,
// amounts, purchase/payment details and all other request bytes stay intact.
func (s *Service) orderCreationDigest(ctx context.Context, params any, lines []billing.OrderLineParams) ([]byte, error) {
	raw, err := json.Marshal(params)
	if err != nil {
		return nil, err
	}
	if len(lines) > 0 {
		mid, err := merchant.Require(ctx)
		if err != nil {
			return nil, err
		}
		ids := make([]string, len(lines))
		for i, line := range lines {
			ids[i] = line.PriceID.String()
		}
		rows, err := s.rt.DB.Gen(ctx).ListPriceStorageIDs(ctx, gen.ListPriceStorageIDsParams{MerchantID: mid.UUID(), Ids: ids})
		if err != nil {
			return nil, err
		}
		for _, row := range rows {
			// These request types contain price_id only in their typed lines.
			// Replace the JSON string token, preserving the original byte layout
			// rather than re-encoding a map in a different key order.
			public, _ := json.Marshal(row.PublicID)
			stored, _ := json.Marshal("price_" + row.ID.String())
			raw = bytes.ReplaceAll(raw, append([]byte(`"price_id":`), public...), append([]byte(`"price_id":`), stored...))
		}
	}
	sum := sha256.Sum256(raw)
	return sum[:], nil
}

// orderLinesDigest names lines for finding the customer's open order of the
// same lines, whatever the spelling of each price; nil when a price is
// unknown (the order's quote refuses it).
func (s *Service) orderLinesDigest(ctx context.Context, lines []billing.OrderLineParams) ([]byte, error) {
	if len(lines) == 0 {
		return nil, nil
	}
	mid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	ids := make([]string, len(lines))
	for i, line := range lines {
		ids[i] = line.PriceID.String()
	}
	rows, err := s.rt.DB.Gen(ctx).ListPriceStorageIDs(ctx, gen.ListPriceStorageIDsParams{MerchantID: mid.UUID(), Ids: ids})
	if err != nil {
		return nil, err
	}
	stored := make(map[string]uuid.UUID, len(rows))
	for _, row := range rows {
		stored[row.PublicID] = row.ID
	}
	keys := make([]orders.LineKey, len(lines))
	for i, line := range lines {
		id, ok := stored[line.PriceID.String()]
		if !ok {
			return nil, nil
		}
		keys[i] = orders.LineKey{Price: id, Quantity: line.Quantity, Amount: line.Amount}
	}
	return orders.LinesDigest(keys), nil
}
