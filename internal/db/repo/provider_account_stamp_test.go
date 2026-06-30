package repo

import (
	"context"
	"testing"

	"github.com/google/uuid"
)

func TestResolveProviderAccountIDForStampOnlyUsesPinnedContext(t *testing.T) {
	if got := resolveProviderAccountIDForStamp(context.Background()); got != nil {
		t.Fatalf("unpinned provider account = %v, want nil", *got)
	}

	id := uuid.New()
	got := resolveProviderAccountIDForStamp(WithProviderAccountID(context.Background(), id))
	if got == nil || *got != id {
		t.Fatalf("pinned provider account = %v, want %s", got, id)
	}
}
