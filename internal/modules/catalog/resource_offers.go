package catalog

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/shared/apperr"
)

type offerCursor struct {
	Scope    string    `json:"scope"`
	Currency string    `json:"currency"`
	PriceID  uuid.UUID `json:"price_id"`
}

// ListOffersForEntitlement is a bounded exact reverse catalog lookup. Access is
// checked separately against retained grants, never against this live catalog.
func ListOffersForEntitlement(ctx context.Context, database *db.DB, key string, params openrails.OfferListParams) (*openrails.OfferList, error) {
	if strings.TrimSpace(key) == "" || len(key) > 256 || !utf8.ValidString(key) || strings.ContainsRune(key, 0) {
		return nil, apperr.Invalidf("invalid entitlement key")
	}
	if params.Kind != openrails.OfferPermanent && params.Kind != openrails.OfferFinite && params.Kind != openrails.OfferRecurring {
		return nil, apperr.Invalidf("kind must be permanent, finite or recurring")
	}
	if params.Limit < 0 || params.Limit > 100 {
		return nil, apperr.Invalidf("limit must be between 1 and 100")
	}
	if params.Limit == 0 {
		params.Limit = 20
	}
	preferred := strings.ToUpper(strings.TrimSpace(params.PreferredCurrency))
	if len(preferred) > 16 || !utf8.ValidString(preferred) || strings.ContainsRune(preferred, 0) {
		return nil, apperr.Invalidf("invalid preferred_currency")
	}
	mid, catalogID, err := queryCatalogScope(ctx)
	if err != nil {
		return nil, err
	}
	rawScope, _ := json.Marshal([]any{mid.String(), catalogID, key, params.Kind, preferred})
	digest := sha256.Sum256(rawScope)
	scope := hex.EncodeToString(digest[:])
	var after *uuid.UUID
	afterCurrency := ""
	if params.Cursor != "" {
		if len(params.Cursor) > 1024 {
			return nil, apperr.Invalidf("invalid cursor")
		}
		raw, err := base64.RawURLEncoding.DecodeString(params.Cursor)
		var cursor offerCursor
		if err != nil || json.Unmarshal(raw, &cursor) != nil || cursor.Scope != scope || cursor.PriceID == uuid.Nil || cursor.Currency == "" {
			return nil, apperr.Invalidf("cursor does not match this offer query")
		}
		after, afterCurrency = &cursor.PriceID, cursor.Currency
	}
	rows, err := database.Gen(ctx).ListOffersForEntitlement(ctx, gen.ListOffersForEntitlementParams{MerchantID: mid.UUID(), CatalogID: catalogID, Entitlement: key, Kind: string(params.Kind), PreferredCurrency: preferred, AfterID: after, AfterCurrency: afterCurrency, PageLimit: int32(params.Limit + 1)})
	if err != nil {
		return nil, err
	}
	result := &openrails.OfferList{Data: make([]openrails.CatalogOffer, 0, min(len(rows), params.Limit)), HasMore: len(rows) > params.Limit}
	if result.HasMore {
		rows = rows[:params.Limit]
	}
	for _, row := range rows {
		offer := openrails.CatalogOffer{Kind: params.Kind, ProductID: openrails.ProductID(row.ProductID).String(), ProductKey: row.ProductKey, ProductName: row.ProductName, PriceID: openrails.PriceID(row.PriceID).String(), PriceKey: row.PriceKey, UnitAmount: row.UnitAmount, Currency: row.Currency, AutoRenew: row.AutoRenew}
		if row.AccessDurationHours != nil {
			value := int(*row.AccessDurationHours)
			offer.AccessDurationHours = &value
		}
		if len(row.EntitlementsSpec) > 0 {
			if err := json.Unmarshal(row.EntitlementsSpec, &offer.EntitlementsSpec); err != nil {
				return nil, err
			}
		}
		result.Data = append(result.Data, offer)
	}
	if result.HasMore {
		last := rows[len(rows)-1]
		raw, _ := json.Marshal(offerCursor{Scope: scope, Currency: last.Currency, PriceID: last.PriceID})
		result.NextCursor = base64.RawURLEncoding.EncodeToString(raw)
	}
	return result, nil
}
