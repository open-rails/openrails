package db

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
)

func TestRequirePSPIDOnlyUsesPinnedContext(t *testing.T) {
	if _, err := RequirePSPID(context.Background()); !errors.Is(err, ErrNoPSPInContext) {
		t.Fatalf("unpinned PSP err = %v, want ErrNoPSPInContext", err)
	}

	id := uuid.New()
	got, err := RequirePSPID(WithPSPID(context.Background(), id))
	if err != nil || got != id {
		t.Fatalf("pinned PSP = %v, %v; want %s, nil", got, err, id)
	}
}

// or#893: a nil id must not become a pin — an unattributed write has to fail,
// not silently stamp the zero uuid (which no psps row can ever have).
func TestWithPSPIDRefusesTheNilUUID(t *testing.T) {
	ctx := WithPSPID(context.Background(), uuid.Nil)
	if _, err := RequirePSPID(ctx); !errors.Is(err, ErrNoPSPInContext) {
		t.Fatalf("nil-pinned PSP err = %v, want ErrNoPSPInContext", err)
	}
}
