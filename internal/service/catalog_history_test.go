package service

import (
	"encoding/json"
	"testing"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/history"
)

// moveState is a catalog with one product and its price key, each archived or
// not, the key's current version named by its public id.
func moveState(productArchived, priceArchived bool, price billing.PriceID) catalogState {
	return catalogState{
		productObject("solo"): {fields: map[string]json.RawMessage{"archived": catalogJSON(productArchived), "entitlements": catalogJSON([]string{})}},
		priceObject("solo", "monthly"): {priceID: uuid.NewSHA1(uuid.Nil, []byte(price)), publicID: price,
			fields: map[string]json.RawMessage{"archived": catalogJSON(priceArchived)}},
	}
}

func movedTo(t *testing.T, before, after catalogState) (history.Move, bool) {
	t.Helper()
	return priceMove(priceObject("solo", "monthly"), before, after)
}

func TestAPriceKeysFirstVersionMovesTheKeyToItsPublicID(t *testing.T) {
	entries, err := catalogHistory(catalogState{}, moveState(false, false, "solo:monthly:v1"), nil, nil, "")
	if err != nil {
		t.Fatalf("a new price key records its history: %v", err)
	}
	var prices []history.Entry
	for _, e := range entries {
		if e.ObjectType == billing.LogObjectPrice {
			prices = append(prices, e)
		}
	}
	if len(prices) != 1 || prices[0].Action != billing.LogActionCreate {
		t.Fatalf("one entry creates the price and moves its key: %+v", prices)
	}
	payload := prices[0].Changes.(history.Fields)
	if payload.Move == nil || payload.Move.PriceID != "solo:monthly:v1" || payload.Move.Archived {
		t.Fatalf("the key moves to its first version: %+v", payload)
	}
}

func TestAKeyThatStopsNamingAPriceNamesTheOneItNamed(t *testing.T) {
	// The product is archived as the key gets a new version: from then the
	// key names nothing, having named v1.
	move, moved := movedTo(t, moveState(false, false, "solo:monthly:v1"), moveState(true, false, "solo:monthly:v2"))
	if !moved || move.PriceID != "solo:monthly:v1" || !move.Archived {
		t.Fatalf("the move names v1, archived: %+v", move)
	}
	// A version created while the product is archived moves nothing: the key
	// named nothing before and names nothing after.
	if move, moved := movedTo(t, moveState(true, false, "solo:monthly:v1"), moveState(true, false, "solo:monthly:v2")); moved {
		t.Fatalf("no move while the key names nothing: %+v", move)
	}
	move, moved = movedTo(t, moveState(true, false, "solo:monthly:v2"), moveState(false, false, "solo:monthly:v2"))
	if !moved || move.PriceID != "solo:monthly:v2" || move.Archived {
		t.Fatalf("unarchiving the product moves the key back to v2: %+v", move)
	}
	move, moved = movedTo(t, moveState(false, false, "solo:monthly:v2"), catalogState{productObject("solo"): moveState(false, false, "")[productObject("solo")]})
	if !moved || move.PriceID != "solo:monthly:v2" || !move.Archived {
		t.Fatalf("a deleted price leaves its key naming nothing: %+v", move)
	}
}

func TestCatalogHistoryFoldsPriceMovesIntoObjectChanges(t *testing.T) {
	active := moveState(false, false, "solo:monthly:v1")
	updated := moveState(false, false, "solo:monthly:v2")
	active[priceObject("solo", "monthly")].fields["unit_amount"] = catalogJSON("1000000")
	updated[priceObject("solo", "monthly")].fields["unit_amount"] = catalogJSON("2000000")
	deleted := catalogState{productObject("solo"): active[productObject("solo")]}
	for _, test := range []struct {
		name          string
		before, after catalogState
		action        billing.LogAction
		price         billing.PriceID
		archived      bool
	}{
		{"update", active, updated, billing.LogActionUpdate, "solo:monthly:v2", false},
		{"archive", moveState(false, false, "solo:monthly:v1"), moveState(false, true, "solo:monthly:v1"), billing.LogActionArchive, "solo:monthly:v1", true},
		{"unarchive", moveState(false, true, "solo:monthly:v1"), moveState(false, false, "solo:monthly:v1"), billing.LogActionUnarchive, "solo:monthly:v1", false},
		{"delete", active, deleted, billing.LogActionDelete, "solo:monthly:v1", true},
		{"product archive", moveState(false, false, "solo:monthly:v1"), moveState(true, false, "solo:monthly:v1"), billing.LogActionMove, "solo:monthly:v1", true},
		{"product unarchive", moveState(true, false, "solo:monthly:v1"), moveState(false, false, "solo:monthly:v1"), billing.LogActionMove, "solo:monthly:v1", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			entries, err := catalogHistory(test.before, test.after, nil, nil, "")
			if err != nil {
				t.Fatal(err)
			}
			var prices []history.Entry
			for _, entry := range entries {
				if entry.ObjectType == billing.LogObjectPrice {
					prices = append(prices, entry)
				}
			}
			if len(prices) != 1 || prices[0].Action != test.action || prices[0].ObjectID != "solo:monthly" {
				t.Fatalf("one price entry: %+v", prices)
			}
			var move history.Move
			if test.action == billing.LogActionMove {
				move = prices[0].Changes.(history.Move)
			} else {
				payload := prices[0].Changes.(history.Fields)
				if payload.Move == nil {
					t.Fatal("price change lacks its move")
				}
				move = *payload.Move
				raw, err := json.Marshal(move)
				if err != nil {
					t.Fatal(err)
				}
				var fields map[string]json.RawMessage
				if err := json.Unmarshal(raw, &fields); err != nil {
					t.Fatal(err)
				}
				if len(fields) != 2 {
					t.Fatalf("folded move carries only price_id and archived: %s", raw)
				}
			}
			if move.PriceID != test.price || move.Archived != test.archived {
				t.Fatalf("wrong movement: %+v", move)
			}
		})
	}
}
