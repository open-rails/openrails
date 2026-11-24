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
	revision := int64(3)
	entry, moved := priceMove(priceObject("solo", "monthly"), before, after, &revision)
	if !moved {
		return history.Move{}, false
	}
	if entry.ObjectType != billing.LogObjectPrice || entry.ObjectID != "solo:monthly" || entry.Action != billing.LogActionMove {
		t.Fatalf("a move is the price key's: %+v", entry)
	}
	return entry.Changes.(history.Move), true
}

func TestAPriceKeysFirstVersionMovesTheKeyToItsPublicID(t *testing.T) {
	entries, err := catalogHistory(catalogState{}, moveState(false, false, "solo:monthly:v1"), nil, nil, "")
	if err != nil {
		t.Fatalf("a new price key records its history: %v", err)
	}
	var moves []history.Move
	for _, e := range entries {
		if e.Action == billing.LogActionMove {
			moves = append(moves, e.Changes.(history.Move))
		}
	}
	if len(moves) != 1 || moves[0].PriceID != "solo:monthly:v1" || moves[0].Archived {
		t.Fatalf("the key moves to its first version, solo:monthly:v1: %+v", moves)
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
