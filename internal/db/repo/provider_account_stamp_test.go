package repo

import (
	"context"
	"testing"

	"github.com/google/uuid"
)

func TestResolveRailMerchantAccountIDForStampOnlyUsesPinnedContext(t *testing.T) {
	if got := resolveRailMerchantAccountIDForStamp(context.Background()); got != nil {
		t.Fatalf("unpinned provider account = %v, want nil", *got)
	}

	id := uuid.New()
	got := resolveRailMerchantAccountIDForStamp(WithRailMerchantAccountID(context.Background(), id))
	if got == nil || *got != id {
		t.Fatalf("pinned provider account = %v, want %s", got, id)
	}
}
