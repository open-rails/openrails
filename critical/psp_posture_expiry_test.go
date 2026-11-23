//go:build integration

package critical_test

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jonboulle/clockwork"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/catalog"
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/integrations/nmi"
	"github.com/open-rails/openrails/internal/nmimock"
	"github.com/open-rails/openrails/internal/providerposture"
	"github.com/stretchr/testify/require"
)

// A PSP's environment can change after it was proven (NMI's test mode is a
// dashboard toggle), so a proof stands for an hour and is proven again before
// the next write.
func TestPSPPostureExpiry(t *testing.T) {
	t.Run("OR-J06-S08/an-armed-verdict-expires-and-is-proven-again-before-the-next-write", func(t *testing.T) {
		t.Run("test-account-leaves-test-mode", func(t *testing.T) {
			a, journal := newNMIWorld(t, func(w *world) {
				t.Cleanup(providerposture.Process().SetClock(w.clock.Now))
			})
			var down atomic.Bool
			var probes atomic.Int32
			journal.core.FailRequests(func(r *http.Request) bool {
				body, _ := io.ReadAll(r.Body)
				r.Body = io.NopCloser(bytes.NewReader(body))
				if !strings.Contains(string(body), "report_type=test_mode_status") {
					return false
				}
				probes.Add(1)
				return down.Load()
			}, http.StatusServiceUnavailable, 1_000_000)
			product, err := a.alpha.CreateProduct(t.Context(), billing.CreateProductParams{Key: "posture-download", DisplayName: "Download", Ownership: catalog.OwnershipConsumable, Entitlements: []string{"download:owned"}})
			require.NoError(t, err)
			// One open order per customer and lines: each concurrent sale buys its
			// own price, so each is its own order and reaches the write gate.
			prices := make([]billing.PriceID, 4)
			for i := range prices {
				price, err := a.alpha.CreatePrice(t.Context(), billing.CreatePriceParams{ProductID: product.ID, Key: "posture-download-" + string(rune('a'+i)), UnitAmount: 9_990_000, Currency: "USD"})
				require.NoError(t, err)
				prices[i] = price.ID
			}
			buy := func(ctx context.Context, price billing.PriceID, key string) (int, []byte, error) {
				input := billing.CreateMyOrderParams{Lines: []billing.OrderLineParams{{PriceID: price}}, ExpectedTotal: ptr(int64(9_990_000)), Reusable: ptr(false),
					Payment: &billing.OrderPaymentParams{Token: journal.core.Tokenize(nmimock.Card{Brand: "mastercard", Last4: "5454", Exp: "1235"})}}
				return a.do(ctx, http.MethodPost, "/v1/me/orders", a.aliceToken, input, map[string]string{"Idempotency-Key": key})
			}
			paid := func(key string) {
				t.Helper()
				status, raw, err := buy(t.Context(), prices[0], key)
				require.NoError(t, err)
				require.Equal(t, http.StatusCreated, status, string(raw))
			}
			refused := func(key string, want int) {
				t.Helper()
				status, raw, err := buy(t.Context(), prices[0], key)
				require.NoError(t, err)
				require.Equal(t, want, status, string(raw))
			}

			// The start-up verification has armed the PSP before counting.
			require.Eventually(t, func() bool {
				for _, probe := range a.engine.Probes() {
					if probe.Name == "openrails_psp_posture" {
						return probe.Check(t.Context()) == nil
					}
				}
				return false
			}, 10*time.Second, 10*time.Millisecond)
			paid("proven")
			proven := probes.Load()
			a.clock.Advance(30 * time.Minute)
			paid("within-the-hour")
			require.Equal(t, proven, probes.Load(), "a proof younger than an hour is not proven again per payment")
			journal.money(t, 2, 1998)

			// An hour after the proof, concurrent sales share one proof; the
			// provider not answering refuses them rather than trusting the old one.
			a.clock.Advance(30 * time.Minute)
			down.Store(true)
			var wg sync.WaitGroup
			statuses := make([]int, 3)
			raws := make([][]byte, 3)
			errs := make([]error, 3)
			for i := range statuses {
				wg.Go(func() { statuses[i], raws[i], errs[i] = buy(t.Context(), prices[i+1], "stale-"+string(rune('a'+i))) })
			}
			wg.Wait()
			for i := range statuses {
				require.NoError(t, errs[i])
				require.Equal(t, http.StatusPaymentRequired, statuses[i], "a stale proof never arms a write: %s", raws[i])
			}
			require.Equal(t, proven+1, probes.Load(), "concurrent sales share one proof")
			journal.money(t, 2, 1998)

			// The provider answers again: once the retry is due, the next sale
			// proves the account and pays.
			down.Store(false)
			a.clock.Advance(time.Second)
			paid("answered-again")
			require.Equal(t, proven+2, probes.Load())
			journal.money(t, 3, 2997)

			// The account leaves test mode: within the hour of the last proof
			// nothing asks; the next sale after it is refused before any bytes.
			journal.core.SetTestMode(false)
			a.clock.Advance(time.Hour)
			refused("left-test-mode", http.StatusPaymentRequired)
			require.Equal(t, proven+3, probes.Load())
			// The disarmed PSP is no longer offered.
			refused("still-live", http.StatusUnprocessableEntity)
			require.Equal(t, proven+3, probes.Load(), "a disagreeing answer holds until the PSP's configuration changes")
			journal.money(t, 3, 2997)
			require.Len(t, journal.core.Sales(), 3)
		})
		t.Run("live-account-enters-test-mode", func(t *testing.T) {
			// A live PSP never runs beside a fake provider in an engine, so its
			// write gate is driven directly against the gateway fixture.
			clock := clockwork.NewFakeClock()
			t.Cleanup(providerposture.Process().SetClock(clock.Now))
			gateway := nmimock.NewUnstarted(nmimock.Options{Clock: clock.Now})
			gateway.SetTestMode(false)
			client, err := nmi.NewAccountClient(slog.Default(), uuid.New(), uuid.New(), "critical-live-gateway", &config.NMIProviderSettings{SecurityKey: "critical-live-key", Environment: config.EnvironmentLive})
			require.NoError(t, err)
			client.UseTransport(gateway)
			sale := func() error {
				_, err := client.RunSale(t.Context(), nmi.SaleParams{PaymentToken: gateway.Tokenize(nmimock.Card{Brand: "visa", Last4: "4242"}), Amount: 999, Currency: "USD"})
				return err
			}
			require.NoError(t, sale())
			gateway.SetTestMode(true)
			clock.Advance(time.Hour)
			err = sale()
			require.ErrorIs(t, err, providerposture.ErrDisarmed, "a live PSP whose account entered test mode would approve without moving money")
			require.ErrorIs(t, err, nmi.ErrNotDispatched)
			require.Len(t, gateway.Sales(), 1)
			status, known := providerposture.Process().Lookup(client.PostureKey())
			require.True(t, known)
			require.Equal(t, providerposture.Simulated, status.Verdict)
			require.Equal(t, 2, gateway.Reads()["query:test_mode_status"])
			require.Empty(t, gateway.Unexpected())
		})
	})
}
