//go:build e2e && integration

package subscriptions_test

import (
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/config"
)

func TestOrderChargebackCancelsSubscriptions(t *testing.T) {
	for _, rail := range []string{"stripe", "nmi"} {
		for _, scenario := range []string{"single", "mixed", "retry", "held"} {
			t.Run(rail+"/"+scenario, func(t *testing.T) {
				t.Parallel()
				w := newWorld(t)
				ctx := t.Context()
				c := w.newCustomer()
				card := c.saveCard(rail, card{Brand: "visa", Last4: "5100"})
				lines := []any{line(w.membership("dispute:first", 10_000_000), 0)}
				benefits := []string{"dispute:first"}
				total := int64(10_000_000)
				if scenario != "single" {
					lines = append(lines, line(w.membership("dispute:second", 20_000_000), 0), line(w.lifetime("dispute:oneoff", 5_000_000), 0))
					benefits = append(benefits, "dispute:second", "dispute:oneoff")
					total = 35_000_000
				}
				bought := c.order(http.MethodPost, "/orders", "disputed-order", obj{
					"lines": lines, "expected_total": micros(total), "payment": obj{"payment_method_id": card},
				})
				require.Equal(t, http.StatusCreated, bought.status, "%v", bought.body)
				require.Equal(t, "complete", bought.body["status"])
				for _, benefit := range benefits {
					assert.True(t, c.entitled(benefit), "%s was granted by the purchase", benefit)
				}
				var subs []billing.SubscriptionID
				for _, l := range orderLines(bought) {
					if id, ok := l["subscription_id"].(string); ok {
						sub, err := billing.ParseSubscriptionID(id)
						require.NoError(t, err)
						subs = append(subs, sub)
					}
				}
				require.NotEmpty(t, subs)
				due := *w.subscription(embedded, subs[0]).CurrentPeriodEndsAt
				paid := completed(w.payments(embedded, c.id))
				require.Len(t, paid, 1)
				payment := paid[0]
				if scenario == "single" {
					require.NotNil(t, payment.SubscriptionID, "the direct reference duplicates the order line")
				} else {
					require.Nil(t, payment.SubscriptionID, "mixed orders identify subscriptions through their lines")
					require.Len(t, subs, 2)
				}
				notice := func() obj {
					if rail == "stripe" {
						return stripeDisputeEvent("charge.dispute.created", "dp_order", "needs_response", payment)
					}
					return nmiEvent("chargeback.batch.complete", obj{"count": 1, "chargebacks": []obj{{
						"id": "cb_order", "date": w.clock.Now().UTC().Format("2006-01-02"), "customer_name": "E2E Payer",
						"cc_number": "4xxxxxxxxxxx5100", "amount": decimalCents(total / 10_000), "reason_code": "10", "reason": "Fraud",
					}}})
				}

				if scenario == "held" {
					w.cfg = func(cfg *config.Config) { cfg.ProviderWriteMode = config.ProviderWriteModeReadOnly }
					w.restart()
					retry := notice()
					require.Equal(t, http.StatusInternalServerError, w.deliver(rail, retry))
					for _, id := range subs {
						assert.Equal(t, billing.SubscriptionActive, w.subscription(embedded, id).Status, "the recovery hold still applies")
					}
					got, err := w.client[embedded].GetPayment(ctx, payment.ID)
					require.NoError(t, err)
					assert.Equal(t, total, got.AmountRefunded, "record the financial fact while cancellation is held")
					w.cfg = func(cfg *config.Config) { cfg.ProviderWriteMode = config.ProviderWriteModeFull }
					w.restart()
					require.Equal(t, http.StatusOK, w.deliver(rail, retry))
				}

				if scenario == "retry" {
					// Fail the second cancellation after the first has committed.
					_, err := w.pool.Exec(ctx, w.q(fmt.Sprintf(`CREATE FUNCTION billing.reject_dispute_cancel() RETURNS trigger LANGUAGE plpgsql AS $$
					BEGIN IF NEW.id = '%s'::uuid AND NEW.status = 'canceled' THEN RAISE EXCEPTION 'cancel unavailable'; END IF; RETURN NEW; END $$;
					CREATE TRIGGER reject_dispute_cancel BEFORE UPDATE ON billing.subscriptions FOR EACH ROW EXECUTE FUNCTION billing.reject_dispute_cancel();`, subs[1].UUID())))
					require.NoError(t, err)
					retry := notice()
					require.Equal(t, http.StatusInternalServerError, w.deliverNow(rail, retry))
					assert.Equal(t, billing.SubscriptionCanceled, w.subscription(embedded, subs[0]).Status)
					assert.Equal(t, billing.SubscriptionActive, w.subscription(embedded, subs[1]).Status)
					_, err = w.pool.Exec(ctx, w.q(`DROP TRIGGER reject_dispute_cancel ON billing.subscriptions`))
					require.NoError(t, err)
					require.Equal(t, http.StatusOK, w.deliver(rail, retry), "retry the same partially applied callback")
				}

				for range 2 {
					require.Equal(t, http.StatusOK, w.deliver(rail, notice()))
					for _, id := range subs {
						sub := w.subscription(embedded, id)
						assert.Equal(t, billing.SubscriptionCanceled, sub.Status)
						require.NotNil(t, sub.CancelType)
						assert.Equal(t, "chargeback", *sub.CancelType)
					}
					for _, benefit := range benefits {
						assert.False(t, c.entitled(benefit), "%s stays revoked", benefit)
					}
					got, err := w.client[remote].GetPayment(ctx, payment.ID)
					require.NoError(t, err)
					assert.Equal(t, total, got.AmountRefunded, "the reversal is counted once")
				}
				var notices int
				require.NoError(t, w.pool.QueryRow(ctx, w.q(`SELECT count(*) FROM billing.notification_events WHERE customer_id = $1 AND event_type = 'premium_ended'`), c.cid().UUID()).Scan(&notices))
				assert.Equal(t, len(subs), notices, "each subscription has one cancellation")

				charges := w.engineCharges()
				w.converge()
				w.advanceHealthyTo(due.Add(time.Hour))
				w.runRenewals()
				assert.Equal(t, charges, w.engineCharges(), "the disputed subscriptions never renew")
				for _, id := range subs {
					assert.Equal(t, billing.SubscriptionCanceled, w.subscription(remote, id).Status)
				}
				for _, benefit := range benefits {
					assert.False(t, c.entitled(benefit), "%s stays revoked", benefit)
				}
				if rail == "stripe" {
					require.Equal(t, http.StatusOK, w.deliver(rail, stripeDisputeEvent("charge.dispute.closed", "dp_order", "won", payment)))
					for _, id := range subs {
						assert.Equal(t, billing.SubscriptionCanceled, w.subscription(remote, id).Status, "winning restores money, not permission to restart engine billing")
					}
					for _, benefit := range benefits {
						assert.False(t, c.entitled(benefit), "%s stays revoked", benefit)
					}
					w.runRenewals()
					assert.Equal(t, charges, w.engineCharges())
				}
			})
		}
	}
}
