package reconcile

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/pkg/merchant"
)

// A panicking provider read ends that read, never the process: batch, bulk
// and listen loops run their reads through recovered, and a failed read still
// drains the merchant.
func TestVerifierWorkersRecoverPanics(t *testing.T) {
	require.ErrorContains(t, recovered(func() error { panic("malformed provider response") }), "malformed provider response")

	v := &Verifier{Retries: 1, Coalesce: time.Millisecond, BulkThreshold: 2}
	t.Cleanup(v.Close)
	mid := merchant.ID(uuid.New())
	v.Enqueue(mid, uuid.New())
	v.Enqueue(mid, uuid.New(), uuid.New(), uuid.New())
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	require.NoError(t, v.Drain(ctx, mid))
}
