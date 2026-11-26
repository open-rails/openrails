//go:build integration

package critical_test

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails/billing"
	embedded "github.com/open-rails/openrails/internal/engine"
	"github.com/open-rails/openrails/internal/failpoint"
	"github.com/open-rails/openrails/internal/intents"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
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
				require.Equal(t, a.clock.Now().Before(accepted.Renewal.PeriodEnd), access.Entitlements["nmi:engine"])
				invoice, err := a.alpha.GetInvoice(t.Context(), billing.InvoiceID(accepted.InvoiceID))
				require.NoError(t, err)
				require.Equal(t, billing.InvoiceComplete, invoice.Status)
				require.Equal(t, billing.InvoicePaymentSucceeded, invoice.Payment.Status)
				for _, at := range []time.Time{accepted.Renewal.PeriodEnd.Add(-time.Microsecond), accepted.Renewal.PeriodEnd, accepted.Renewal.PeriodEnd.Add(time.Hour)} {
					access, err := a.alpha.CheckEntitlements(t.Context(), billing.CheckEntitlementsParams{CustomerID: sub.CustomerID, Entitlements: []string{"nmi:engine"}, At: at})
					require.NoError(t, err)
					require.Equal(t, at.Before(accepted.Renewal.PeriodEnd), access.Entitlements["nmi:engine"])
				}
				if a.clock.Now().Before(accepted.Renewal.PeriodEnd) {
					a.clock.Advance(accepted.Renewal.PeriodEnd.Sub(a.clock.Now()) + time.Hour)
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
			})
		}
	})
	t.Run("OR-J17-S18/revoke-access-cancel-refunds-a-late-paid-renewal", func(t *testing.T) {
		for _, delay := range []time.Duration{0, 8 * 24 * time.Hour} {
			t.Run(delay.String(), func(t *testing.T) {
				a, p, sub := nmiEngineMembership(t)
				p.core.HideSales(1)
				row := executeEngineRenewal(t, a, sub)
				require.Equal(t, intents.StatusUnknownNeedsVerify, row.Status)
				accepted, err := subscriptions.DecodeSubscriptionCollectionPayload(row)
				require.NoError(t, err)
				a.clock.Advance(delay)
				var canceled *billing.Subscription
				// Cancel after verification read the operation, before completion
				// locks the subscription and reloads its cancellation evidence.
				remove := failpoint.Set(func(ctx context.Context, site failpoint.Site) error {
					if site.Point == failpoint.BeforeComplete && site.Operation == row.ID {
						status, raw := a.request(t, http.MethodPost, "/v1/admin/subscriptions/"+sub.ID.String()+"/cancel", a.alphaKey.Secret, map[string]any{"reason": "Cut access immediately", "revoke_access": true})
						require.Equal(t, http.StatusOK, status, string(raw))
						canceled = subscriptionState(t, a, sub.ID, "canceled")
						require.Equal(t, a.clock.Now(), *canceled.EndedAt)
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
