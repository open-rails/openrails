//go:build e2e && integration

package subscriptions_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/writeposture"
)

// A customer ordering while the merchant's provider writes are off is told
// so: 503 provider_writes_disabled, its message naming the cause and its fix,
// for a readonly deployment and for the merchant's own write posture.
func TestProviderWritesOffSaysWhy(t *testing.T) {
	for name, row := range map[string]struct {
		mode string
		hold bool
		want string
	}{
		"readonly deployment": {config.ProviderWriteModeReadOnly, false, "provider_write_mode=readonly blocks all provider writes: set provider_write_mode to full (or limited)"},
		"merchant posture":    {config.ProviderWriteModeFull, true, "the merchant's write posture is readonly (operator): `openrails merchant arm` restores it"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			w := prepareWorld(t, 12)
			w.cfg = func(cfg *config.Config) { cfg.ProviderWriteMode = row.mode }
			w.start()
			key, err := w.applyCatalog(`  "{key}-once":
    currency: usd
    unit_amount: 9990000
    billing_interval_hours: null
    access_duration_hours: 720
    psps: {nmi: {}}
`)
			require.NoError(t, err)
			if row.hold {
				mid := w.client[embedded].MerchantID()
				database, err := db.NewWithPGXPool(w.pool, w.schema)
				require.NoError(t, err)
				require.NoError(t, database.RunInMerchantScope(t.Context(), mid, "hold", func(ctx context.Context) error {
					return writeposture.Set(ctx, database.Gen(ctx), mid.UUID(), writeposture.ReadOnly, writeposture.ReasonOperator, "e2e", w.clock.Now())
				}))
			}
			created := w.newCustomer().order(http.MethodPost, "/orders", "", map[string]any{"lines": []any{map[string]any{"price_id": priceID(t, w, key, key+"-once")}}})
			require.Equal(t, http.StatusServiceUnavailable, created.status, "%v", created.body)
			refusal := created.body["error"].(map[string]any)
			require.Equal(t, billing.CodeProviderWritesDisabled, refusal["code"])
			require.Contains(t, refusal["message"], row.want)
		})
	}
}
