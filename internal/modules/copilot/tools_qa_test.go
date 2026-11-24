package copilot

import (
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/internal/db/models"
)

func TestAKeyWithoutRecordedMovesReadsItsVersions(t *testing.T) {
	at := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	v1 := &models.Price{ID: uuid.New(), Archived: true, CreatedAt: at}
	v2 := &models.Price{ID: uuid.New(), CreatedAt: at.Add(24 * time.Hour)}
	points := priceHistoryPoints(nil, []*models.Price{v1, v2})
	if len(points) != 2 || points[0].PriceID != v2.ID || points[1].PriceID != v1.ID || !points[0].EffectiveAt.Equal(v2.CreatedAt) {
		t.Fatalf("a restored key reads its versions, newest first: %+v", points)
	}
	moves := []*models.PriceKeyMovement{{ID: uuid.New(), PriceID: v1.ID, EffectiveAt: at}}
	if got := priceHistoryPoints(moves, []*models.Price{v1, v2}); len(got) != 1 || got[0] != moves[0] {
		t.Fatalf("recorded moves are the history: %+v", got)
	}
	if got := priceHistoryPoints(nil, nil); len(got) != 0 {
		t.Fatalf("an unknown key has none: %+v", got)
	}
}
