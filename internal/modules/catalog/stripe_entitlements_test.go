package catalog

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSyncProductFeatures(t *testing.T) {
	ctx, fake := t.Context(), newFakeStripe(t)
	svc := fake.service()
	const product = "prod_1"
	// An operator-owned feature (no openrails_managed marker) is never detached.
	fake.features["legacy"] = StripeFeature{ID: "feat_legacy", LookupKey: "legacy", Metadata: map[string]string{}}
	fake.attached[product] = map[string]string{"pf_legacy": "legacy"}

	for _, step := range []struct {
		desired                     []string
		attached                    []string
		creates, attaches, detaches int
	}{
		{[]string{"premium", " pro ", ""}, []string{"legacy", "premium", "pro"}, 2, 2, 0},
		{[]string{"pro", "premium"}, []string{"legacy", "premium", "pro"}, 2, 2, 0}, // idempotent
		{[]string{"premium"}, []string{"legacy", "premium"}, 2, 2, 1},
		{[]string{"premium", "pro"}, []string{"legacy", "premium", "pro"}, 2, 3, 1}, // reuse, never recreate
		{nil, []string{"legacy"}, 2, 3, 3},
	} {
		require.NoError(t, svc.SyncProductFeatures(ctx, product, step.desired))
		require.Equal(t, step.attached, fake.attachedKeys(product), "%q", step.desired)
		require.Equal(t, []int{step.creates, step.attaches, step.detaches}, []int{fake.featureCreates, fake.attaches, fake.detaches}, "%q", step.desired)
	}
	require.Error(t, svc.SyncProductFeatures(ctx, " ", nil))
}
