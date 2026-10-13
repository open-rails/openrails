//go:build e2e && integration

package subscriptions_test

import (
	"errors"
	"net/http"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/engine"
	"github.com/open-rails/openrails/internal/intents"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/stretchr/testify/require"
)

func TestPlatformCardRemoval(t *testing.T) {
	for _, scenario := range []string{"saved", "used", "local completion failed", "changed vault", "response lost", "delete unavailable", "read forbidden", "already absent", "closed during removal"} {
		t.Run(scenario, func(t *testing.T) {
			t.Parallel()
			w, bt := platformSubscriptionWorld(t)
			c := w.newCustomer()
			token := bt.collect("4111111111111111")
			req := map[string]any{"psp_id": w.psp["nmi"].String(), "token": token}
			saved := unwrap(c.must(http.MethodPost, "/payment-methods", "", req))
			id := saved["id"].(string)
			methodID, err := billing.ParsePaymentMethodID(id)
			require.NoError(t, err)
			// Seed a retained default binding: the public default setter currently
			// does not offer platform cards. Removal must still clear an existing binding.
			_, err = w.pool.Exec(t.Context(), w.q(`INSERT INTO billing.money_settings (merchant_id,customer_id,currency,billing_mode,default_payment_method_id) VALUES ($1,$2,'USD','prepaid',$3) ON CONFLICT (merchant_id,customer_id,currency) DO UPDATE SET default_payment_method_id=$3`), w.client[remote].MerchantID().UUID(), c.cid(), methodID.UUID())
			require.NoError(t, err)
			var paid *billing.Order
			if scenario == "used" {
				purchase := c.mustPurchase(remote, order{price: w.permanent("platform:remove").ID, rail: "nmi", method: id})
				paid = &purchase.Order
			}
			if scenario == "changed vault" {
				_, err := w.client[remote].UpdateMerchantConfiguration(t.Context(), setCustody(billing.CardCustodyMerchant))
				require.NoError(t, err)
			}
			if scenario == "local completion failed" {
				_, err := w.pool.Exec(t.Context(), w.q(`CREATE FUNCTION billing.reject_card_removal() RETURNS trigger LANGUAGE plpgsql AS $$
 BEGIN IF NEW.status='removed' THEN RAISE EXCEPTION 'local removal unavailable'; END IF; RETURN NEW; END $$;
 CREATE TRIGGER reject_card_removal BEFORE UPDATE ON billing.payment_methods FOR EACH ROW EXECUTE FUNCTION billing.reject_card_removal();`))
				require.NoError(t, err)
			}
			bt.mu.Lock()
			bt.loseDeleteResponse = scenario == "response lost"
			bt.failDelete = scenario == "delete unavailable"
			if scenario == "read forbidden" {
				bt.tokenReadStatus = http.StatusForbidden
			}
			if scenario == "already absent" {
				delete(bt.tokens, methodID.UUID().String())
			}
			if scenario == "closed during removal" {
				bt.beforeTokenDelete = func() {
					_, err := w.pool.Exec(t.Context(), w.q(`UPDATE billing.payment_methods SET status='closed' WHERE id=$1`), methodID.UUID())
					require.NoError(t, err)
				}
			}
			bt.mu.Unlock()
			status, out := c.call(http.MethodDelete, "/payment-methods/"+id, "", nil)
			pending := scenario == "local completion failed" || scenario == "response lost" || scenario == "delete unavailable" || scenario == "read forbidden"
			if pending {
				// The worker might already have verified a lost response.
				require.Contains(t, []int{http.StatusAccepted, http.StatusNoContent}, status, "%v", out)
				if scenario != "response lost" {
					require.Equal(t, http.StatusAccepted, status, "%v", out)
					status, _ := c.call(http.MethodPost, "/payment-methods", "", req)
					require.GreaterOrEqual(t, status, 400, "accepted deletion blocks save replay")
				}
				bt.mu.Lock()
				bt.failDelete, bt.tokenReadStatus = false, 0
				bt.mu.Unlock()
				if scenario == "local completion failed" {
					var state string
					require.NoError(t, w.pool.QueryRow(t.Context(), w.q(`SELECT status FROM billing.payment_methods WHERE id=$1`), methodID.UUID()).Scan(&state))
					require.Equal(t, "active", state, "failed local completion rolls back while the deletion fence remains")
					_, err := w.pool.Exec(t.Context(), w.q(`DROP TRIGGER reject_card_removal ON billing.payment_methods; DROP FUNCTION billing.reject_card_removal();`))
					require.NoError(t, err)
				}
				w.until(func() bool {
					var done bool
					require.NoError(t, w.pool.QueryRow(t.Context(), w.q(`SELECT status='succeeded' FROM billing.provider_intents WHERE idempotency_key=$1`), intents.TypePlatformMethodDelete+":"+methodID.UUID().String()).Scan(&done))
					return done
				}, "the existing worker completes card removal")
			} else {
				require.Equal(t, http.StatusNoContent, status, "%v", out)
			}
			var methodStatus string
			var versions, liveMandates, defaults, operations int
			require.NoError(t, w.pool.QueryRow(t.Context(), w.q(`SELECT status FROM billing.payment_methods WHERE id=$1`), methodID.UUID()).Scan(&methodStatus))
			if scenario == "closed during removal" {
				require.Equal(t, "closed", methodStatus)
				archiveErr := w.exportArchive()
				require.NoError(t, archiveErr, "%v", errors.Unwrap(archiveErr))
			} else {
				require.Equal(t, "removed", methodStatus)
			}
			require.NoError(t, w.pool.QueryRow(t.Context(), w.q(`SELECT count(*) FROM billing.payment_method_versions WHERE payment_method_id=$1`), methodID.UUID()).Scan(&versions))
			require.Positive(t, versions)
			require.NoError(t, w.pool.QueryRow(t.Context(), w.q(`SELECT count(*) FROM billing.mandates WHERE payment_method_id=$1 AND status='active'`), methodID.UUID()).Scan(&liveMandates))
			require.Zero(t, liveMandates)
			require.NoError(t, w.pool.QueryRow(t.Context(), w.q(`SELECT count(*) FROM billing.money_settings WHERE default_payment_method_id=$1`), methodID.UUID()).Scan(&defaults))
			require.Zero(t, defaults)
			list := c.must(http.MethodGet, "/payment-methods", "", nil)
			require.Empty(t, list["data"], "%v", list)
			status, out = c.call(http.MethodDelete, "/payment-methods/"+id, "", nil)
			require.Equal(t, http.StatusNoContent, status, "%v", out)
			require.NoError(t, w.pool.QueryRow(t.Context(), w.q(`SELECT count(*) FROM billing.provider_intents WHERE idempotency_key=$1`), intents.TypePlatformMethodDelete+":"+methodID.UUID().String()).Scan(&operations))
			require.Equal(t, 1, operations)
			if scenario != "changed vault" {
				status, _ = c.call(http.MethodPost, "/payment-methods", "", req)
				require.GreaterOrEqual(t, status, 400, "removed cards cannot be restored by replay")
			}
			bt.mu.Lock()
			require.NotContains(t, bt.tokens, methodID.UUID().String())
			if scenario != "already absent" {
				require.Equal(t, []string{methodID.UUID().String()}, bt.deletedTokens)
			}
			bt.mu.Unlock()
			if paid != nil {
				read, err := w.client[remote].GetOrder(t.Context(), paid.ID)
				require.NoError(t, err)
				require.Equal(t, paid.Payment.PaymentMethodID, read.Payment.PaymentMethodID)
				_, err = w.client[remote].RefundPayment(t.Context(), completed(w.payments(remote, c.id))[0].ID, billing.RefundPaymentParams{Full: true, Reason: "requested_by_customer", IdempotencyKey: "removed-card-refund"})
				require.NoError(t, err)
			}
		})
	}
}

func TestPlatformCardRemovalRefusesOwnershipAndSubscriptions(t *testing.T) {
	for _, subStatus := range []string{"active", "past_due"} {
		t.Run(subStatus, func(t *testing.T) {
			t.Parallel()
			w, bt := platformSubscriptionWorld(t)
			c := w.newCustomer()
			paid := c.mustPurchase(remote, order{price: w.membership("platform:keep", 9_990_000).ID, rail: "nmi", token: bt.collect("4111111111111111")})
			id := paid.Order.Payment.PaymentMethodID.String()
			if subStatus == "past_due" {
				bt.mu.Lock()
				bt.decline = true
				bt.mu.Unlock()
				end := *w.subscription(remote, *paid.SubscriptionID).CurrentPeriodEndsAt
				w.advanceHealthyTo(end)
				w.runRenewals()
				w.until(func() bool { return w.subscription(remote, *paid.SubscriptionID).Status == billing.SubscriptionPastDue }, "renewal refusal leaves the card in use")
			}
			stranger := w.newCustomer()
			status, _ := stranger.call(http.MethodDelete, "/payment-methods/"+id, "", nil)
			require.Equal(t, http.StatusNotFound, status)
			status, out := c.call(http.MethodDelete, "/payment-methods/"+id, "", nil)
			require.Equal(t, http.StatusConflict, status, "%v", out)
			bt.mu.Lock()
			require.Empty(t, bt.deletedTokens)
			bt.mu.Unlock()
			var count int
			require.NoError(t, w.pool.QueryRow(t.Context(), w.q(`SELECT count(*) FROM billing.provider_intents WHERE intent_type=$1`), intents.TypePlatformMethodDelete).Scan(&count))
			require.Zero(t, count)
		})
	}
}

func TestPlatformCardRemovalRetainsReceipt(t *testing.T) {
	t.Parallel()
	w, bt := platformSubscriptionWorld(t)
	c := w.newCustomer()
	saved := unwrap(c.must(http.MethodPost, "/payment-methods", "", map[string]any{"psp_id": w.psp["nmi"].String(), "token": bt.collect("4111111111111111")}))
	id := saved["id"].(string)
	methodID, err := billing.ParsePaymentMethodID(id)
	require.NoError(t, err)
	c.must(http.MethodDelete, "/payment-methods/"+id, "", nil)
	ctx := merchant.WithID(t.Context(), w.client[remote].MerchantID())
	store := intents.NewStore(engine.Graph(w.rt).Runtime.DB)
	row, err := store.GetByIdempotencyKey(ctx, intents.TypePlatformMethodDelete+":"+methodID.UUID().String())
	require.NoError(t, err)
	method, payer, err := intents.DeletedMethod(row)
	require.NoError(t, err)
	require.Equal(t, methodID.UUID(), method)
	require.Equal(t, c.cid().UUID(), payer)
	require.NotEmpty(t, row.Payload)
	require.NotEmpty(t, row.ResultEvidence)
	archiveErr := w.exportArchive()
	require.NoError(t, archiveErr, "%v", errors.Unwrap(archiveErr))
}

func TestPlatformCardRemovalDuringPayment(t *testing.T) {
	t.Parallel()
	w, bt := platformSubscriptionWorld(t)
	c := w.newCustomer()
	saved := unwrap(c.must(http.MethodPost, "/payment-methods", "", map[string]any{"psp_id": w.psp["nmi"].String(), "token": bt.collect("4111111111111111")}))
	id := saved["id"].(string)
	reached, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	t.Cleanup(unblock)
	bt.mu.Lock()
	gateway := bt.gateway
	bt.gateway = func(form url.Values, number string) string { close(reached); <-release; return gateway(form, number) }
	bt.mu.Unlock()
	o := order{price: w.permanent("platform:inflight").ID, rail: "nmi", method: id}
	sale, err := c.sell(remote, o)
	require.NoError(t, err)
	done := make(chan error, 1)
	go func() { _, err := sale.buy(c, o); done <- err }()
	select {
	case <-reached:
	case <-time.After(10 * time.Second):
		unblock()
		t.Fatal("payment did not reach the provider")
	}
	status, out := c.call(http.MethodDelete, "/payment-methods/"+id, "", nil)
	unblock()
	require.Equal(t, http.StatusConflict, status, "%v", out)
	require.NoError(t, <-done)
	bt.mu.Lock()
	require.Empty(t, bt.deletedTokens)
	bt.mu.Unlock()
	c.must(http.MethodDelete, "/payment-methods/"+id, "", nil)
}

func TestPlatformCardRemovalRejectsDelayedSave(t *testing.T) {
	t.Parallel()
	w, bt := platformSubscriptionWorld(t)
	c := w.newCustomer()
	req := map[string]any{"psp_id": w.psp["nmi"].String(), "token": bt.collect("4111111111111111")}
	converted, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	t.Cleanup(unblock)
	bt.mu.Lock()
	bt.afterConversion = func() { close(converted); <-release }
	bt.mu.Unlock()
	done := make(chan int, 1)
	go func() { status, _ := c.call(http.MethodPost, "/payment-methods", "", req); done <- status }()
	select {
	case <-converted:
	case <-time.After(10 * time.Second):
		unblock()
		t.Fatal("save did not convert the card")
	}
	// Another request recovers the already converted card while the first
	// response is delayed. The customer then removes it.
	saved := unwrap(c.must(http.MethodPost, "/payment-methods", "", req))
	id := saved["id"].(string)
	c.must(http.MethodDelete, "/payment-methods/"+id, "", nil)
	unblock()
	require.GreaterOrEqual(t, <-done, 400, "the delayed conversion response cannot restore the removed card")
	require.Empty(t, c.must(http.MethodGet, "/payment-methods", "", nil)["data"])
	bt.mu.Lock()
	require.Empty(t, bt.tokens)
	require.Len(t, bt.deletedTokens, 1)
	bt.mu.Unlock()
}
