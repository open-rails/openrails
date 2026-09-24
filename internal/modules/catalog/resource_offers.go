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

// ListOffersForEntitlements is a bounded exact reverse catalog lookup: one
// query returns a page of offers for every requested key. Access is checked
// separately against retained grants, never against this live catalog.
func ListOffersForEntitlements(ctx context.Context, database *db.DB, keys []string, params openrails.OfferListParams) (map[string]openrails.OfferList, error) {
	if len(keys) > openrails.MaxEntitlementChecks {
		return nil, apperr.Invalidf("at most 100 entitlements are allowed")
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
	result := make(map[string]openrails.OfferList, len(keys))
	scopes := make(map[string]string, len(keys))
	arg := gen.ListOffersForEntitlementsParams{MerchantID: mid.UUID(), CatalogID: catalogID, Kind: string(params.Kind), PreferredCurrency: preferred, PageLimit: int32(params.Limit + 1)}
	for _, key := range keys {
		if strings.TrimSpace(key) == "" || len(key) > 256 || !utf8.ValidString(key) || strings.ContainsRune(key, 0) {
			return nil, apperr.Invalidf("invalid entitlement key")
		}
		if _, seen := result[key]; seen {
			continue
		}
		result[key] = openrails.OfferList{Data: []openrails.CatalogOffer{}}
		rawScope, _ := json.Marshal([]any{mid.String(), catalogID, key, params.Kind, preferred})
		digest := sha256.Sum256(rawScope)
		scopes[key] = hex.EncodeToString(digest[:])
		after, afterCurrency := uuid.Nil, ""
		if raw, ok := params.Cursors[key]; ok {
			cursor, err := decodeOfferCursor(raw, scopes[key])
			if err != nil {
				return nil, err
			}
			after, afterCurrency = cursor.PriceID, cursor.Currency
		}
		arg.Entitlements = append(arg.Entitlements, key)
		arg.AfterIds = append(arg.AfterIds, after)
		arg.AfterCurrencies = append(arg.AfterCurrencies, afterCurrency)
	}
	for key := range params.Cursors {
		if _, ok := result[key]; !ok {
			return nil, apperr.Invalidf("cursor names an entitlement that was not requested")
		}
	}
	if len(arg.Entitlements) == 0 {
		return result, nil
	}
	rows, err := database.Gen(ctx).ListOffersForEntitlements(ctx, arg)
	if err != nil {
		return nil, err
	}
	last := make(map[string]offerCursor, len(arg.Entitlements))
	for _, row := range rows {
		page := result[row.Entitlement]
		if len(page.Data) == params.Limit {
			page.HasMore = true
			raw, _ := json.Marshal(last[row.Entitlement])
			page.NextCursor = base64.RawURLEncoding.EncodeToString(raw)
			result[row.Entitlement] = page
			continue
		}
		last[row.Entitlement] = offerCursor{Scope: scopes[row.Entitlement], Currency: row.Currency, PriceID: row.PriceID}
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
		page.Data = append(page.Data, offer)
		result[row.Entitlement] = page
	}
	return result, nil
}

func decodeOfferCursor(raw, scope string) (offerCursor, error) {
	var cursor offerCursor
	if len(raw) > 1024 {
		return cursor, apperr.Invalidf("invalid cursor")
	}
	decoded, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil || json.Unmarshal(decoded, &cursor) != nil || cursor.Scope != scope || cursor.PriceID == uuid.Nil || cursor.Currency == "" {
		return offerCursor{}, apperr.Invalidf("cursor does not match this offer query")
	}
	return cursor, nil
}
