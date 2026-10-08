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
		{[]string{"premium", "pro"}, []string{"legacy", "premium", "pro"}, 2, 2, 0},
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

func TestStripeFeatureLookupKeepsOpaqueKeys(t *testing.T) {
	fake := newFakeStripe(t)
	service := fake.service()
	keys := []string{"post:101", "premium", " private key "}
	require.NoError(t, service.SyncProductFeatures(t.Context(), "prod_opaque", keys))
	require.ElementsMatch(t, keys, fake.attachedKeys("prod_opaque"))
	require.NoError(t, service.SyncProductFeatures(t.Context(), "prod_opaque", keys))
	require.Equal(t, 3, fake.featureCreates)
	require.Error(t, service.SyncProductFeatures(t.Context(), "prod_opaque", []string{" "}))
}
