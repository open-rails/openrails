package db

import (
	"context"
	"testing"

	"github.com/google/uuid"
)

func TestResolveRailMerchantAccountIDForStampOnlyUsesPinnedContext(t *testing.T) {
	if got := ResolveRailMerchantAccountIDForStamp(context.Background()); got != nil {
		t.Fatalf("unpinned provider account = %v, want nil", *got)
	}

	id := uuid.New()
	got := ResolveRailMerchantAccountIDForStamp(WithPSPID(context.Background(), id))
	if got == nil || *got != id {
		t.Fatalf("pinned provider account = %v, want %s", got, id)
	}
}
