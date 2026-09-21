//go:build integration

package merchantarchive

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jonboulle/clockwork"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/archivewire"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/integrations/nmi"
	"github.com/open-rails/openrails/internal/intents"
	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/pkg/merchant"
	"github.com/stretchr/testify/require"
)

type recurringArchiveReader struct{ client *nmi.NMIClient }

func (r recurringArchiveReader) ResolveNMIClient(context.Context, uuid.UUID, *uuid.UUID) (*nmi.NMIClient, bool, error) {
	return r.client, true, nil
}

// The starting subscription is an explicitly internal engine fixture, not a
// browser enrollment claim. Admission, receipt qualification, renewal effects
// and both archive directions use their production implementations.
func TestEngineRecurringArchiveHistory(t *testing.T) {
	for _, mode := range []string{"paid", "cancelled_before_completion", "late_receipt", "declined"} {
		t.Run(mode, func(t *testing.T) {
			source, target := archiveDB(t, "openrails"), archiveDB(t, "archive_engine_"+mode)
			mid := merchant.ID(uuid.New())
			provision(t, source, mid)
			provision(t, target, mid)
			customer, product, price, psp, method, custodian, sub := uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()
			now := time.Date(2025, 1, 2, 0, 0, 0, 0, time.UTC)
			clock := clockwork.NewFakeClockAt(now)
			ctx, release, err := source.WithMerchantConn(merchant.WithID(t.Context(), mid))
			require.NoError(t, err)
			defer release()
			ctx = db.WithPSPID(ctx, psp)
			exec := func(sql string, args ...any) {
				t.Helper()
				_, err := source.Qx(ctx).Exec(ctx, sql, args...)
				require.NoError(t, err)
			}
			exec(`INSERT INTO openrails.customers(merchant_id,id,issuer) VALUES($1,$2,'https://engine-archive.example')`, mid.UUID(), customer)
			exec(`INSERT INTO openrails.products(merchant_id,id,key,display_name,entitlements_spec) VALUES($1,$2,'engine','Engine','{"engine":null}')`, mid.UUID(), product)
			exec(`INSERT INTO openrails.prices(merchant_id,id,product_id,amount,currency,auto_renew,access_duration_hours) VALUES($1,$2,$3,9990000,'USD',true,720)`, mid.UUID(), price, product)
			exec(`INSERT INTO openrails.psps(merchant_id,id,key,rail,account_id,environment,evidence) VALUES($1,$2,'engine','nmi',$2::uuid::text,'test','{"settings":{}}')`, mid.UUID(), psp)
			exec(`INSERT INTO openrails.custodians(merchant_id,id,key,kind,account_id,environment,settings) VALUES($1,$2,'engine','hyperswitch',$2::uuid::text,'test','{"profile_id":"engine","public_api_key":"synthetic"}')`, mid.UUID(), custodian)
			exec(`INSERT INTO openrails.payment_methods(merchant_id,id,customer_id,psp_id,rail,custodian,custodian_id,rail_customer_ref,rail_method_ref,stored_credential_recurring_ref,initial_transaction_id) VALUES($1,$2,$3,$4,'nmi','hyperswitch',$5,'customer','method','recurring-anchor','initial-fixture')`, mid.UUID(), method, customer, psp, custodian)
			exec(`INSERT INTO openrails.subscriptions(merchant_id,id,customer_id,psp_id,product_id,price_id,payment_method_id,rail,rail_subscription_id,collection_policy,status,current_period_starts_at,current_period_ends_at,entitlements_spec_snapshot) VALUES($1,$2,$3,$4,$5,$6,$7,'nmi','','engine','active',$8,$9,'{"engine":null}')`, mid.UUID(), sub, customer, psp, product, price, method, now.Add(-30*24*time.Hour), now)
			svc := money.NewMoneyService(source, clock)
			require.NoError(t, svc.SetHyperSwitchDeployment("http://127.0.0.1:1"))
			op, err := svc.AdmitDueSubscriptionCollection(ctx, sub, now)
			require.NoError(t, err)
			store := intents.NewStore(source)
			op, claimed, err := store.ClaimByID(ctx, op.ID, now, now.Add(time.Minute))
			require.NoError(t, err)
			require.True(t, claimed)
			p, err := subscriptions.DecodeSubscriptionCollectionPayload(op)
			require.NoError(t, err)
			lifecycle := newPurchaseArchiveServices(source, clock).lifecycle
			if mode == "cancelled_before_completion" {
				require.NoError(t, lifecycle.CancelMembership(ctx, &subscriptions.CancelMembershipParams{SubscriptionID: &sub, CancelType: models.CancelTypeChargeback, RevokeAccess: true}))
			}
			if mode == "late_receipt" {
				clock.Advance(90 * 24 * time.Hour)
			}
			if mode == "declined" {
				require.NoError(t, store.RetainRecurringDecline(ctx, op, 200, ""))
			} else {
				transaction := "engine-archive-" + op.ID.String()
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.Method == http.MethodGet {
						require.Equal(t, "/payments/"+transaction, r.URL.Path)
						json.NewEncoder(w).Encode(map[string]any{"object": "transaction", "id": transaction, "amount": "9.99", "currency": "USD", "response": "1", "actions": []map[string]any{{"id": transaction + "-a", "type": "sale", "amount": "9.99", "success": true, "response": "1"}}})
						return
					}
					require.NoError(t, r.ParseForm())
					require.Equal(t, p.OrderReference, r.Form.Get("order_id"))
					require.Empty(t, r.Form.Get("type"))
					fmt.Fprintf(w, `<nm_response><transaction><transaction_id>%s</transaction_id><order_id>%s</order_id><action><action_type>sale</action_type><success>1</success></action></transaction></nm_response>`, transaction, p.OrderReference)
				}))
				defer server.Close()
				client, err := nmi.NewAccountClient(mid.UUID(), psp, "engine", &config.NMIProviderSettings{SecurityKey: "synthetic", WebhookSecret: "synthetic"}, true)
				require.NoError(t, err)
				client.QueryURL, client.V5BaseURL = server.URL, server.URL
				receipt, found, err := intents.ReadNMICollectionReceipt(ctx, op, recurringArchiveReader{client}, transaction)
				require.NoError(t, err)
				require.True(t, found)
				_, err = store.RetainCollectedReceipt(ctx, op, receipt)
				require.NoError(t, err)
			}
			cfg := &config.Config{TestMode: config.CredentialPostureSandbox, ProviderWriteMode: config.ProviderWriteModeFull}
			handler := money.NewSubscriptionCollectionHandler(source, nil, cfg, clock)
			outcome := handler.Verify(ctx, op)
			if mode == "declined" {
				require.Equal(t, intents.OutcomeTerminal, outcome.Class, outcome.Reason)
			} else {
				require.Equal(t, intents.OutcomeSucceeded, outcome.Class, outcome.Reason)
			}
			exec(`UPDATE openrails.host_outbox SET delivered_at=now() WHERE merchant_id=$1`, mid.UUID())
			var clean bytes.Buffer
			require.NoError(t, Export(ctx, source, mid, &clean))
			// Exact receipt/payment cross-row mismatches must fail export and a coherent
			// wire restore, with the destination transaction completely rolled back.
			original := "engine-archive-" + op.ID.String()
			if mode == "declined" {
				original = "engine_declined:" + op.ID.String()
			}
			for _, corruption := range []string{"missing_payment", "wrong_amount"} {
				t.Run(corruption, func(t *testing.T) {
					if corruption == "missing_payment" {
						exec(`UPDATE openrails.payments SET transaction_id='wrong' WHERE subscription_id=$1`, sub)
					} else {
						exec(`UPDATE openrails.payments SET amount=amount+10000 WHERE subscription_id=$1`, sub)
					}
					var rejected bytes.Buffer
					require.ErrorContains(t, Export(ctx, source, mid, &rejected), "rail_intents")
					raw := encodeUncheckedInitialArchive(t, ctx, source, mid)
					_, err = archivewire.CopyVerified(io.Discard, bytes.NewReader(raw))
					require.NoError(t, err)
					_, err = Restore(t.Context(), target, mid, bytes.NewReader(raw))
					require.Error(t, err)
					assertEmptyBook(t, target, mid)
					if corruption == "missing_payment" {
						exec(`UPDATE openrails.payments SET transaction_id=$2 WHERE subscription_id=$1`, sub, original)
					} else {
						exec(`UPDATE openrails.payments SET amount=amount-10000 WHERE subscription_id=$1`, sub)
					}
				})
			}
			if mode != "paid" {
				grantID := uuid.New()
				exec(`INSERT INTO openrails.grants(merchant_id,customer_id,kind,source_type,source_id,event,spec_snapshot,starts_at,ends_at,id) VALUES($1,$2,'entitlement','subscription',$3,'grant','{"entitlements":["engine"]}',$4,$5,$6)`, mid.UUID(), customer, sub.String(), p.Renewal.PeriodStart, p.Renewal.PeriodEnd, grantID)
				exec(`INSERT INTO openrails.entitlements(merchant_id,customer_id,entitlement,start_at,end_at,source_id,source_type,grant_id) VALUES($1,$2,'engine',$3,$4,$5,'subscription',$6)`, mid.UUID(), customer, p.Renewal.PeriodStart, p.Renewal.PeriodEnd, sub, grantID)
				var rejected bytes.Buffer
				require.Error(t, Export(ctx, source, mid, &rejected))
				raw := encodeUncheckedInitialArchive(t, ctx, source, mid)
				_, err = archivewire.CopyVerified(io.Discard, bytes.NewReader(raw))
				require.NoError(t, err)
				_, err = Restore(t.Context(), target, mid, bytes.NewReader(raw))
				require.Error(t, err)
				assertEmptyBook(t, target, mid)
				// Restore the clean artifact: corruption is intentionally left only in this
				// disposable source fixture, never repaired by the production archive path.
			} else {
				require.NoError(t, lifecycle.CancelMembership(ctx, &subscriptions.CancelMembershipParams{SubscriptionID: &sub, CancelType: models.CancelTypeUser}))
				exec(`UPDATE openrails.prices SET amount=1230000 WHERE id=$1`, price)
				exec(`DELETE FROM openrails.payment_methods WHERE id=$1`, method)
				clean.Reset()
				require.NoError(t, Export(ctx, source, mid, &clean), "later cancellation, reprice and erasure preserve original paid history")
				altered := alteredArchive(t, clean.Bytes(), "grants", "starts_at", func() *string { s := now.Add(time.Hour).Format(time.RFC3339); return &s }())
				_, err = Restore(t.Context(), target, mid, bytes.NewReader(altered))
				require.Error(t, err)
				assertEmptyBook(t, target, mid)
			}
			_, err = Restore(t.Context(), target, mid, bytes.NewReader(clean.Bytes()))
			require.NoError(t, err)
			targetCtx, targetRelease, err := target.WithMerchantConn(merchant.WithID(t.Context(), mid))
			require.NoError(t, err)
			defer targetRelease()
			restored, err := intents.NewStore(target).Get(targetCtx, op.ID)
			require.NoError(t, err)
			var beforeReplay bytes.Buffer
			require.NoError(t, Export(targetCtx, target, mid, &beforeReplay))
			replay := money.NewSubscriptionCollectionHandler(target, nil, cfg, clock).Verify(targetCtx, restored)
			require.Equal(t, outcome.Class, replay.Class, replay.Reason)
			var after bytes.Buffer
			require.NoError(t, Export(targetCtx, target, mid, &after), "offline replay may not manufacture a second payment or access")
			require.Equal(t, beforeReplay.Bytes(), after.Bytes(), "terminal offline replay preserves the complete archived book")
		})
	}
}
