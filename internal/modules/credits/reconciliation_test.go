package credits

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestMultiReconciliationSink_FansOutAndCollectsFirstError(t *testing.T) {
	var seen []string
	boom := errors.New("boom")

	a := ReconciliationSinkFunc(func(_ context.Context, ev ReconciliationEvent) error {
		seen = append(seen, "a")
		return boom
	})
	b := ReconciliationSinkFunc(func(_ context.Context, ev ReconciliationEvent) error {
		seen = append(seen, "b")
		return nil
	})

	multi := MultiReconciliationSink{a, nil, b}
	err := multi.Handle(context.Background(), ReconciliationEvent{Kind: ReconOrphanHold})
	require.ErrorIs(t, err, boom)
	// Every non-nil sink runs even though an earlier one errored.
	require.Equal(t, []string{"a", "b"}, seen)
}
