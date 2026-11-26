//go:build integration

package critical_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/catalog"
	embedded "github.com/open-rails/openrails/internal/engine"
	"github.com/open-rails/openrails/internal/failpoint"
	"github.com/open-rails/openrails/internal/intents"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/internal/nmimock"
	"github.com/stretchr/testify/require"
)

func TestCancellationCustody(t *testing.T) {
	t.Run("OR-J17-S02/cancel-cannot-discard-an-accepted-renewal", func(t *testing.T) {
		a, p, sub := purchasedMembership(t)
		due := *sub.CurrentPeriodEndsAt
		gate := &wireGate{arrived: make(chan struct{}), release: make(chan struct{})}
		p.mu.Lock()
		p.responseGate = gate
		p.mu.Unlock()
		defer gate.unblock()
		a.clock.Advance(due.Sub(a.clock.Now()))
		fleet := startFleet(t, a.world)
		runRegistered(t, fleet, "openrails.billing_pass", 1)
		select {
		case <-gate.arrived:
		case <-time.After(15 * time.Second):
			t.Fatal("renewal did not reach the provider acceptance boundary")
		}
		require.Len(t, p.effects(t), 2, "the original purchase and admitted renewal have actually debited")
		status, raw := a.request(t, http.MethodPost, "/v1/me/subscriptions/"+sub.ID.String()+"/cancel", a.aliceToken, map[string]any{"reason": "Cancel during accepted payment"})
		require.Equal(t, http.StatusOK, status, string(raw))
		pending, err := a.alpha.GetSubscription(t.Context(), sub.ID)
		require.NoError(t, err)
		require.Equal(t, due, *pending.CurrentPeriodEndsAt)
		require.Equal(t, billing.SubscriptionCanceled, pending.Status)
		gate.unblock()
		require.Eventually(t, func() bool {
			current, err := a.alpha.GetSubscription(t.Context(), sub.ID)
			return err == nil && current.CurrentPeriodEndsAt.Equal(due.Add(24*time.Hour))
		}, 20*time.Second, 20*time.Millisecond)
		canceled, err := a.alpha.GetSubscription(t.Context(), sub.ID)
		require.NoError(t, err)
		require.Equal(t, billing.SubscriptionCanceled, canceled.Status)
		require.Equal(t, due.Add(24*time.Hour), *canceled.CurrentPeriodEndsAt)
		a.clock.Advance(25 * time.Hour)
		runRegistered(t, fleet, "openrails.billing_pass", 2)
		require.Len(t, p.effects(t), 2, "cancellation stops future work without losing the accepted payment")
		payments, err := a.alpha.ListPayments(t.Context(), billing.PaymentListParams{CustomerID: sub.CustomerID})
		require.NoError(t, err)
		require.Len(t, payments.Items, 2)
	})
	t.Run("OR-J17-S20/engine-cancel-locks-customer-before-subscription", func(t *testing.T) {
		for _, admin := range []bool{false, true} {
			t.Run(fmt.Sprint(admin), func(t *testing.T) {
				a, p, sub := nmiEngineMembership(t)
				p.core.HideSales(1)
				row := executeEngineRenewal(t, a, sub)
				require.Equal(t, intents.StatusUnknownNeedsVerify, row.Status)
				p.core.Reveal()
				arrived, release := make(chan struct{}), make(chan struct{})
				var open sync.Once
				defer open.Do(func() { close(release) })
				remove := failpoint.Set(func(ctx context.Context, site failpoint.Site) error {
					if site.Point == failpoint.BeforeComplete && site.Operation == row.ID {
						close(arrived)
						select {
						case <-release:
						case <-ctx.Done():
							return ctx.Err()
						}
					}
					return nil
				})
				defer remove()
				rt := embedded.Graph(a.engine).Runtime
				verified := make(chan error, 1)
				go func() {
					verified <- rt.DB.RunInMerchantConn(merchant.WithID(t.Context(), a.alpha.MerchantID()), func(ctx context.Context) error {
						completed, err := rt.IntentRunner().VerifyByID(ctx, row.ID)
						if err == nil && completed.Status != intents.StatusSucceeded {
							return fmt.Errorf("completion status: %s", completed.Status)
						}
						return err
					})
				}()
				select {
				case <-arrived:
				case <-time.After(15 * time.Second):
					t.Fatal("renewal did not reach completion")
				}
				holder, err := a.pool.Begin(t.Context())
				require.NoError(t, err)
				defer holder.Rollback(context.Background())
				_, err = holder.Exec(t.Context(), "SELECT id FROM "+pgx.Identifier{a.schema, "customers"}.Sanitize()+" WHERE id=$1 FOR NO KEY UPDATE", sub.CustomerID.UUID())
				require.NoError(t, err)
				var pid int
				require.NoError(t, holder.QueryRow(t.Context(), "SELECT pg_backend_pid()").Scan(&pid))
				canceled := make(chan error, 1)
				go func() {
					path, token := "/v1/me/subscriptions/", a.aliceToken
					if admin {
						path, token = "/v1/admin/subscriptions/", a.alphaKey.Secret
					}
					status, raw, err := a.doWith(t.Context(), a.server.Client(), http.MethodPost, path+sub.ID.String()+"/cancel", token, map[string]any{"reason": "Stop future renewals"})
					if err == nil && status != http.StatusOK {
						err = fmt.Errorf("cancel status %d: %s", status, raw)
					}
					canceled <- err
				}()
				require.Eventually(t, func() bool {
					var blocked bool
					err := a.pool.QueryRow(t.Context(), "SELECT EXISTS (SELECT FROM pg_stat_activity WHERE $1 = ANY(pg_blocking_pids(pid)))", pid).Scan(&blocked)
					return err == nil && blocked
				}, 3*time.Second, 10*time.Millisecond)
				// A cancel waiting for the customer must not hold its subscription.
				_, err = holder.Exec(t.Context(), "SELECT id FROM "+pgx.Identifier{a.schema, "subscriptions"}.Sanitize()+" WHERE id=$1 FOR UPDATE NOWAIT", sub.ID.UUID())
				require.NoError(t, err)
				require.NoError(t, holder.Commit(t.Context()))
				select {
				case err := <-canceled:
					require.NoError(t, err)
				case <-time.After(10 * time.Second):
					t.Fatal("cancel did not commit")
				}
				open.Do(func() { close(release) })
				select {
				case err := <-verified:
					require.NoError(t, err)
				case <-time.After(15 * time.Second):
					t.Fatal("renewal did not commit")
				}
				subscriptionState(t, a, sub.ID, "canceled")
			})
		}
	})
}

func TestCanceledUnknownRenewal(t *testing.T) {
	t.Run("OR-J17-S17/period-end-cancel-keeps-a-late-paid-renewal", func(t *testing.T) {
		for _, tc := range []struct {
			name        string
			admin       bool
			delay       time.Duration
			accessHours int
		}{
			{"customer", false, 0, 7 * 24},
			{"admin", true, 0, 7 * 24},
			{"customer-expired-period", false, 8 * 24 * time.Hour, 7 * 24},
			{"admin-expired-period", true, 8 * 24 * time.Hour, 7 * 24},
			{"customer-expired-long-access", false, 8 * 24 * time.Hour, 10 * 24},
		} {
			t.Run(tc.name, func(t *testing.T) {
				a, p, sub := nmiEngineMembershipFor(t, tc.accessHours)
				p.core.HideSales(1)
				row := executeEngineRenewal(t, a, sub)
				require.Equal(t, intents.StatusUnknownNeedsVerify, row.Status)
				accepted, err := subscriptions.DecodeSubscriptionCollectionPayload(row)
				require.NoError(t, err)
				a.clock.Advance(tc.delay)
				path, token := "/v1/me/subscriptions/", a.aliceToken
				if tc.admin {
					path, token = "/v1/admin/subscriptions/", a.alphaKey.Secret
				}
				status, raw := a.request(t, http.MethodPost, path+sub.ID.String()+"/cancel", token, map[string]any{"reason": "Stop future renewals"})
				require.Equal(t, http.StatusOK, status, string(raw))
				canceled := subscriptionState(t, a, sub.ID, "canceled")
				rt := embedded.Graph(a.engine).Runtime
				ctx := merchant.WithID(t.Context(), a.alpha.MerchantID())
				// A later billing pass cannot admit another collection while this
				// canceled subscription's accepted charge awaits verification.
				a.clock.Advance(time.Hour)
				fleet := startFleet(t, a.world)
				runRegistered(t, fleet, "openrails.billing_pass", 2)
				require.NoError(t, fleet.Stop(context.Background()))
				p.core.Reveal()
				require.NoError(t, rt.DB.RunInMerchantConn(ctx, func(ctx context.Context) error {
					var err error
					row, err = rt.IntentRunner().VerifyByID(ctx, row.ID)
					return err
				}))
				require.Equal(t, intents.StatusSucceeded, row.Status)
				require.Empty(t, intents.EvidenceString(row, "cancel_revoked_access_at"))
				current := subscriptionState(t, a, sub.ID, "canceled")
				require.Equal(t, accepted.Renewal.PeriodStart, *current.CurrentPeriodStartsAt)
				require.Equal(t, accepted.Renewal.PeriodEnd, *current.CurrentPeriodEndsAt)
				end := accepted.Renewal.PeriodEnd
				if canceled.CanceledAt.After(end) {
					end = *canceled.CanceledAt
				}
				require.Equal(t, end, *current.EndedAt)
				require.Equal(t, canceled.CanceledAt, current.CanceledAt)
				require.Equal(t, canceled.CancelType, current.CancelType)
				require.Equal(t, canceled.CancelFeedback, current.CancelFeedback)
				require.Nil(t, current.Dunning)
				access, err := a.alpha.CheckEntitlements(t.Context(), billing.CheckEntitlementsParams{CustomerID: sub.CustomerID, Entitlements: []string{"nmi:engine"}})
				require.NoError(t, err)
				accessEnd := accepted.Renewal.PeriodStart.Add(time.Duration(tc.accessHours) * time.Hour)
				require.Equal(t, a.clock.Now().Before(accessEnd), access.Entitlements["nmi:engine"])
				invoice, err := a.alpha.GetInvoice(t.Context(), billing.InvoiceID(accepted.InvoiceID))
				require.NoError(t, err)
				require.Equal(t, billing.InvoiceComplete, invoice.Status)
				require.Equal(t, billing.InvoicePaymentSucceeded, invoice.Payment.Status)
				for _, at := range []time.Time{accepted.Renewal.PeriodEnd.Add(-time.Microsecond), accepted.Renewal.PeriodEnd, accepted.Renewal.PeriodEnd.Add(time.Hour), accessEnd.Add(-time.Microsecond), accessEnd, accessEnd.Add(time.Hour)} {
					access, err := a.alpha.CheckEntitlements(t.Context(), billing.CheckEntitlementsParams{CustomerID: sub.CustomerID, Entitlements: []string{"nmi:engine"}, At: at})
					require.NoError(t, err)
					require.Equal(t, at.Before(accessEnd), access.Entitlements["nmi:engine"])
				}
				if a.clock.Now().Before(accessEnd) {
					a.clock.Advance(accessEnd.Sub(a.clock.Now()) + time.Hour)
				}
				require.NoError(t, rt.DB.RunInMerchantConn(ctx, func(ctx context.Context) error {
					_, err := rt.IntentRunner().VerifyByID(ctx, row.ID)
					return err
				}))
				access, err = a.alpha.CheckEntitlements(t.Context(), billing.CheckEntitlementsParams{CustomerID: sub.CustomerID, Entitlements: []string{"nmi:engine"}})
				require.NoError(t, err)
				require.False(t, access.Entitlements["nmi:engine"])
				var collections, refunds int
				require.NoError(t, a.pool.QueryRow(t.Context(), "SELECT count(*) FILTER (WHERE intent_type='subscription_collection'),count(*) FILTER (WHERE intent_type='nmi_refund') FROM "+pgx.Identifier{a.schema, "provider_intents"}.Sanitize()+" WHERE subscription_id=$1", sub.ID.UUID()).Scan(&collections, &refunds))
				require.Equal(t, 1, collections)
				require.Zero(t, refunds)
				findings, err := a.alpha.ListFindings(t.Context(), billing.FindingListParams{Type: intents.ChargeAfterCancelFindingType})
				require.NoError(t, err)
				require.Empty(t, findings.Items)
				p.money(t, 2, 2000)
				if tc.name == "customer-expired-long-access" {
					archiveBytes(t, a.alpha)
				}
			})
		}
	})
	t.Run("OR-J17-S21/replacement-membership-refunds-a-late-paid-renewal", func(t *testing.T) {
		for _, slot := range []string{"same-product", "same-tier-group"} {
			t.Run(slot, func(t *testing.T) {
				a, p, sub := nmiEngineMembership(t)
				priceID := sub.PriceID
				entitlement := "nmi:engine"
				if slot == "same-tier-group" {
					_, err := a.alpha.UpdateProduct(t.Context(), sub.ProductID, billing.UpdateProductParams{TierGroup: catalog.Value("nmi-level"), TierRank: catalog.Value(1)})
					require.NoError(t, err)
					product, err := a.alpha.CreateProduct(t.Context(), billing.CreateProductParams{Key: "replacement", DisplayName: "Replacement membership", TierGroup: ptr("nmi-level"), TierRank: 2, Entitlements: []string{"nmi:replacement"}})
					require.NoError(t, err)
					price, err := a.alpha.CreatePrice(t.Context(), billing.CreatePriceParams{ProductID: product.ID, UnitAmount: 10_000_000, Currency: "USD", BillingIntervalHours: ptr(7 * 24), AccessDurationHours: ptr(7 * 24)})
					require.NoError(t, err)
					priceID, entitlement = price.ID, "nmi:replacement"
				}
				p.core.HideSales(1)
				row := executeEngineRenewal(t, a, sub)
				require.Equal(t, intents.StatusUnknownNeedsVerify, row.Status)
				accepted, err := subscriptions.DecodeSubscriptionCollectionPayload(row)
				require.NoError(t, err)
				status, raw := a.request(t, http.MethodPost, "/v1/me/subscriptions/"+sub.ID.String()+"/cancel", a.aliceToken, map[string]any{"reason": "Stop future renewals"})
				require.Equal(t, http.StatusOK, status, string(raw))
				canceled := subscriptionState(t, a, sub.ID, "canceled")
				a.clock.Advance(time.Hour)
				input := billing.CreateMyInvoiceParams{Lines: []billing.InvoiceLineParams{{PriceID: priceID}}, ExpectedTotal: ptr(int64(10_000_000)), Reusable: ptr(true), Payment: &billing.InvoicePaymentParams{Token: p.core.Tokenize(nmimock.Card{Brand: "visa", Last4: "4242"}), BillingDetails: billedTo("US", "10001")}}
				status, raw = a.request(t, http.MethodPost, "/v1/me/invoices", a.aliceToken, input, map[string]string{"Idempotency-Key": "replacement-membership"})
				require.Equal(t, http.StatusCreated, status, string(raw))
				var invoice billing.Invoice
				require.NoError(t, json.Unmarshal(raw, &invoice))
				require.Equal(t, billing.InvoiceComplete, invoice.Status)
				require.NotEqual(t, billing.InvoiceID(accepted.InvoiceID), invoice.ID)
				require.NotNil(t, invoice.Lines[0].SubscriptionID)
				replacement := subscriptionState(t, a, *invoice.Lines[0].SubscriptionID, "active")
				require.NotEqual(t, sub.ID, replacement.ID)
				p.core.Reveal()
				rt := embedded.Graph(a.engine).Runtime
				ctx := merchant.WithID(t.Context(), a.alpha.MerchantID())
				for range 2 {
					require.NoError(t, rt.DB.RunInMerchantConn(ctx, func(ctx context.Context) error {
						var err error
						row, err = rt.IntentRunner().VerifyByID(ctx, row.ID)
						return err
					}))
					require.Equal(t, intents.StatusSucceeded, row.Status)
				}
				current := subscriptionState(t, a, sub.ID, "canceled")
				require.Equal(t, canceled.CurrentPeriodStartsAt, current.CurrentPeriodStartsAt)
				require.Equal(t, canceled.CurrentPeriodEndsAt, current.CurrentPeriodEndsAt)
				require.Equal(t, canceled.EndedAt, current.EndedAt)
				var grants int
				require.NoError(t, a.pool.QueryRow(t.Context(), "SELECT count(*) FROM "+pgx.Identifier{a.schema, "grants"}.Sanitize()+" WHERE source_id=$1 AND source_type='subscription' AND starts_at=$2", sub.ID.String(), accepted.Renewal.PeriodStart).Scan(&grants))
				require.Zero(t, grants)
				oldInvoice, err := a.alpha.GetInvoice(t.Context(), billing.InvoiceID(accepted.InvoiceID))
				require.NoError(t, err)
				require.Equal(t, billing.InvoiceVoid, oldInvoice.Status)
				fleet := startFleet(t, a.world)
				require.Eventually(t, func() bool {
					wakeJobs(t, fleet, "openrails.provider_operation")
					var refunded int64
					err := p.pool.QueryRow(t.Context(), "SELECT refunded FROM "+pgx.Identifier{p.schema, "transactions"}.Sanitize()+" WHERE id=$1", intents.EvidenceString(row, "transaction_id")).Scan(&refunded)
					return err == nil && refunded == 1000
				}, 15*time.Second, 20*time.Millisecond)
				require.NoError(t, fleet.Stop(context.Background()))
				unchanged := subscriptionState(t, a, replacement.ID, "active")
				require.Equal(t, replacement, unchanged)
				paidInvoice, err := a.alpha.GetInvoice(t.Context(), invoice.ID)
				require.NoError(t, err)
				require.Equal(t, billing.InvoiceComplete, paidInvoice.Status)
				require.Equal(t, billing.InvoicePaymentSucceeded, paidInvoice.Payment.Status)
				access, err := a.alpha.CheckEntitlements(t.Context(), billing.CheckEntitlementsParams{CustomerID: sub.CustomerID, Entitlements: []string{entitlement}, At: accepted.Renewal.PeriodEnd})
				require.NoError(t, err)
				require.True(t, access.Entitlements[entitlement])
				p.money(t, 3, 2000)
			})
		}
	})
	t.Run("OR-J17-S18/revoke-access-cancel-refunds-a-late-paid-renewal", func(t *testing.T) {
		for _, tc := range []struct {
			name           string
			delay          time.Duration
			periodEndFirst bool
		}{
			{"0s", 0, false},
			{"192h0m0s", 8 * 24 * time.Hour, false},
			{"period-end-then-revoke", time.Hour, true},
		} {
			t.Run(tc.name, func(t *testing.T) {
				a, p, sub := nmiEngineMembership(t)
				p.core.HideSales(1)
				row := executeEngineRenewal(t, a, sub)
				require.Equal(t, intents.StatusUnknownNeedsVerify, row.Status)
				accepted, err := subscriptions.DecodeSubscriptionCollectionPayload(row)
				require.NoError(t, err)
				a.clock.Advance(tc.delay)
				if tc.periodEndFirst {
					status, raw := a.request(t, http.MethodPost, "/v1/admin/subscriptions/"+sub.ID.String()+"/cancel", a.alphaKey.Secret, map[string]any{"reason": "Stop future renewals"})
					require.Equal(t, http.StatusOK, status, string(raw))
					a.clock.Advance(time.Hour)
				}
				var canceled *billing.Subscription
				// Cancel after verification read the operation, before completion
				// locks the subscription and reloads its cancellation evidence.
				remove := failpoint.Set(func(ctx context.Context, site failpoint.Site) error {
					if site.Point == failpoint.BeforeComplete && site.Operation == row.ID {
						status, raw := a.request(t, http.MethodPost, "/v1/admin/subscriptions/"+sub.ID.String()+"/cancel", a.alphaKey.Secret, map[string]any{"reason": "Cut access immediately", "revoke_access": true})
						require.Equal(t, http.StatusOK, status, string(raw))
						canceled = subscriptionState(t, a, sub.ID, "canceled")
						if !tc.periodEndFirst {
							require.Equal(t, a.clock.Now(), *canceled.EndedAt)
						}
						access, err := a.alpha.CheckEntitlements(t.Context(), billing.CheckEntitlementsParams{CustomerID: sub.CustomerID, Entitlements: []string{"nmi:engine"}})
						require.NoError(t, err)
						require.False(t, access.Entitlements["nmi:engine"])
					}
					return nil
				})
				t.Cleanup(remove)
				p.core.Reveal()
				rt := embedded.Graph(a.engine).Runtime
				require.NoError(t, rt.DB.RunInMerchantConn(merchant.WithID(t.Context(), a.alpha.MerchantID()), func(ctx context.Context) error {
					var err error
					row, err = rt.IntentRunner().VerifyByID(ctx, row.ID)
					return err
				}))
				remove()
				require.Equal(t, intents.StatusSucceeded, row.Status)
				require.Equal(t, a.clock.Now().UTC().Format(time.RFC3339Nano), intents.EvidenceString(row, "cancel_revoked_access_at"))
				require.NoError(t, intents.ValidateSubscriptionCollectionTerminal(row))
				current := subscriptionState(t, a, sub.ID, "canceled")
				require.Equal(t, canceled.CurrentPeriodEndsAt, current.CurrentPeriodEndsAt)
				require.Equal(t, canceled.EndedAt, current.EndedAt)
				require.Equal(t, canceled.CanceledAt, current.CanceledAt)
				invoice, err := a.alpha.GetInvoice(t.Context(), billing.InvoiceID(accepted.InvoiceID))
				require.NoError(t, err)
				require.Equal(t, billing.InvoiceVoid, invoice.Status)
				findings, err := a.alpha.ListFindings(t.Context(), billing.FindingListParams{Type: intents.ChargeAfterCancelFindingType})
				require.NoError(t, err)
				require.Len(t, findings.Items, 1)
				fleet := startFleet(t, a.world)
				require.Eventually(t, func() bool {
					wakeJobs(t, fleet, "openrails.provider_operation")
					var refunded int64
					err := p.pool.QueryRow(t.Context(), "SELECT COALESCE(sum(refunded),0) FROM "+pgx.Identifier{p.schema, "transactions"}.Sanitize()).Scan(&refunded)
					return err == nil && refunded == 1000
				}, 15*time.Second, 20*time.Millisecond)
				access, err := a.alpha.CheckEntitlements(t.Context(), billing.CheckEntitlementsParams{CustomerID: sub.CustomerID, Entitlements: []string{"nmi:engine"}, At: accepted.Renewal.PeriodEnd.Add(-time.Hour)})
				require.NoError(t, err)
				require.False(t, access.Entitlements["nmi:engine"])
				p.money(t, 2, 1000)
			})
		}
	})
	t.Run("OR-J17-S19/canceled-unpaid-renewal-voids-its-invoice", func(t *testing.T) {
		for _, declined := range []bool{false, true} {
			t.Run(fmt.Sprint(declined), func(t *testing.T) {
				a, p, sub := nmiEngineMembership(t)
				if declined {
					p.core.SetDecline("4242", "202")
					p.core.Intercept(nmiSaleRequest, func(r *http.Request, serve func() *http.Response) (*http.Response, error) {
						response := serve()
						response.StatusCode = http.StatusBadGateway
						p.core.QueryUnavailable(true)
						return response, nil
					})
				} else {
					p.core.LoseSales(1)
				}
				row := executeEngineRenewal(t, a, sub)
				require.Equal(t, intents.StatusUnknownNeedsVerify, row.Status)
				accepted, err := subscriptions.DecodeSubscriptionCollectionPayload(row)
				require.NoError(t, err)
				status, raw := a.request(t, http.MethodPost, "/v1/me/subscriptions/"+sub.ID.String()+"/cancel", a.aliceToken, map[string]any{"reason": "Stop future renewals"})
				require.Equal(t, http.StatusOK, status, string(raw))
				p.core.QueryUnavailable(false)
				rt := embedded.Graph(a.engine).Runtime
				require.NoError(t, rt.DB.RunInMerchantConn(merchant.WithID(t.Context(), a.alpha.MerchantID()), func(ctx context.Context) error {
					var err error
					if declined {
						row, err = rt.IntentRunner().VerifyByID(ctx, row.ID)
					} else {
						row, err = rt.IntentRunner().Resolve(ctx, row.ID, intents.Resolution{NotExecuted: true, Actor: "operator", Reason: "Checked empty renewal order"})
					}
					return err
				}))
				require.Equal(t, intents.StatusFailedTerminal, row.Status)
				current := subscriptionState(t, a, sub.ID, "canceled")
				require.Equal(t, sub.CurrentPeriodEndsAt, current.CurrentPeriodEndsAt)
				require.Nil(t, current.Dunning)
				invoice, err := a.alpha.GetInvoice(t.Context(), billing.InvoiceID(accepted.InvoiceID))
				require.NoError(t, err)
				require.Equal(t, billing.InvoiceVoid, invoice.Status)
				a.clock.Advance(8 * 24 * time.Hour)
				fleet := startFleet(t, a.world)
				runRegistered(t, fleet, "openrails.billing_pass", 2)
				var count int
				require.NoError(t, a.pool.QueryRow(t.Context(), "SELECT count(*) FROM "+pgx.Identifier{a.schema, "provider_intents"}.Sanitize()+" WHERE subscription_id=$1 AND intent_type='subscription_collection'", sub.ID.UUID()).Scan(&count))
				require.Equal(t, 1, count)
				p.money(t, 1, 1000)
			})
		}
	})
}
