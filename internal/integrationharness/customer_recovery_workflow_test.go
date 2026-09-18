//go:build integration

package integrationharness

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/internal/db/models"
)

// Customer payment recovery (#809) through the shared Client on every
// deployment shape, against the loopback NMI gateway: pay-now and retry-now
// each create one immutable attempt under the engine's own durable operation,
// a decline is a coded 402 with history intact, a lost provider answer stays
// visibly unresolved and converges through the restarted deployment's own
// verifier, the same key replays the same attempt, and provider-managed rails
// are refused before any provider traffic. Nothing here proves live NMI
// behavior.

func requireDeclined(t *testing.T, err error) *openrails.StatusError {
	t.Helper()
	require.ErrorIs(t, err, openrails.ErrCardDeclined)
	require.ErrorIs(t, err, openrails.ErrPaymentRefused)
	var status *openrails.StatusError
	require.True(t, errors.As(err, &status), "%v", err)
	require.Equal(t, 402, status.Status)
	require.Equal(t, openrails.CodeCardDeclined, status.Code)
	require.NotEmpty(t, status.Metadata["decline_reason"])
	return status
}

func TestCustomerPayNowAcrossDeployments(t *testing.T) {
	ctx := context.Background()
	h := New(t, ctx)
	gateway := NewFakeNMIGateway(t)
	runMoneyDeployments(t, h, gateway, func(t *testing.T, d moneyDeployment) {
		client := d.client()
		payNow := func(f CollectionFixture, key string, method uuid.UUID) (*openrails.InvoicePayNowResult, error) {
			return client.PayInvoiceNow(ctx, openrails.PayInvoiceNowRequest{CustomerID: openrails.CustomerID(f.Customer), InvoiceID: f.Invoice, PaymentMethodID: openrails.PaymentMethodID(method), IdempotencyKey: key})
		}

		t.Run("success", func(t *testing.T) {
			gateway.SetMode(NMISaleApprove)
			gateway.SetVisible(true)
			sales := gateway.SaleCount()
			fixture := h.SeedPastDueInvoice(d.runtime(), d.merchant, "USD", 5_000_000)
			key := "pay-" + uuid.NewString()[:8]
			result, err := payNow(fixture, key, fixture.Method)
			require.NoError(t, err)
			require.False(t, result.Replayed)
			require.Equal(t, "succeeded", result.Operation.Status)
			require.False(t, result.Operation.Unresolved())
			require.Equal(t, "settled", result.Attempt.Status)
			require.Equal(t, openrails.PaymentMethodID(fixture.Method), *result.Attempt.PaymentMethodID)
			require.Equal(t, "paid", result.Invoice.Status)
			require.Zero(t, result.Invoice.AmountDue)
			require.Equal(t, fixture.Amount, result.Invoice.AmountPaid)
			require.NotNil(t, result.Invoice.Recovery)
			require.False(t, result.Invoice.Recovery.Retryable)
			require.Equal(t, openrails.RecoveryBlockedNotDue, result.Invoice.Recovery.BlockedReason)
			require.Equal(t, 1, result.Invoice.Recovery.AttemptCount)
			require.Equal(t, sales+1, gateway.SaleCount())
			sale, ok := gateway.SaleForOrder(result.Operation.ID.String())
			require.True(t, ok, "the wire order id is the operation id")
			require.Equal(t, "5.00", sale.Amount)
			require.Equal(t, fixture.Vault, sale.Vault)
			require.Equal(t, sale.TransactionID, *result.Attempt.RailPaymentID)
			require.Equal(t, 1, h.OwedPaymentTransfers(fixture.Customer))

			replayed, err := payNow(fixture, key, fixture.Method)
			require.NoError(t, err)
			require.True(t, replayed.Replayed)
			require.Equal(t, result.Attempt.ID, replayed.Attempt.ID)
			require.Equal(t, result.Operation.ID, replayed.Operation.ID)
			require.Equal(t, sales+1, gateway.SaleCount(), "a replay never charges")
			_, err = payNow(fixture, "again-"+uuid.NewString()[:8], fixture.Method)
			requireRefusal(t, err, openrails.ErrConflict, openrails.CodeInvoiceNotRetryable)
			require.Equal(t, 1, h.OwedPaymentTransfers(fixture.Customer))
		})

		t.Run("decline", func(t *testing.T) {
			gateway.SetMode(NMISaleDecline)
			gateway.SetVisible(true)
			sales := gateway.SaleCount()
			fixture := h.SeedPastDueInvoice(d.runtime(), d.merchant, "USD", 6_000_000)
			key := "decline-" + uuid.NewString()[:8]
			_, err := payNow(fixture, key, fixture.Method)
			status := requireDeclined(t, err)
			require.Equal(t, fixture.Invoice.String(), status.Metadata["invoice_id"])
			require.NotEmpty(t, status.Metadata["attempt_id"])
			require.NotEmpty(t, status.Metadata["operation_id"])
			require.Equal(t, true, status.Metadata["retryable"], "%v", status.Metadata)
			require.Equal(t, false, status.Metadata["replayed"])
			require.Equal(t, sales, gateway.SaleCount(), "a decline lands nothing")
			require.Equal(t, "failed_terminal", h.LatestCollectionOperation(fixture.Invoice).Status)
			attempts, total, err := client.ListInvoicePaymentAttempts(ctx, fixture.Invoice, 10, 0)
			require.NoError(t, err)
			require.EqualValues(t, 1, total)
			require.Equal(t, "failed", attempts[0].Status)
			require.Equal(t, status.Metadata["attempt_id"], attempts[0].ID.String())
			invoice, err := client.GetMerchantInvoice(ctx, fixture.Invoice)
			require.NoError(t, err)
			require.Equal(t, "past_due", invoice.Status)
			require.EqualValues(t, 1, invoice.CollectionFailureCount)
			require.Nil(t, invoice.CollectionIntentID, "a decline releases the invoice")

			_, err = payNow(fixture, key, fixture.Method)
			replay := requireDeclined(t, err)
			require.Equal(t, status.Metadata["attempt_id"], replay.Metadata["attempt_id"], "the same key answers with the same attempt")
			require.Equal(t, true, replay.Metadata["replayed"])
			_, total, err = client.ListInvoicePaymentAttempts(ctx, fixture.Invoice, 10, 0)
			require.NoError(t, err)
			require.EqualValues(t, 1, total, "a replay records nothing")

			gateway.SetMode(NMISaleApprove)
			paid, err := payNow(fixture, "retry-"+uuid.NewString()[:8], fixture.Method)
			require.NoError(t, err)
			require.Equal(t, "settled", paid.Attempt.Status)
			require.Equal(t, "paid", paid.Invoice.Status)
			require.Equal(t, 2, paid.Invoice.Recovery.AttemptCount)
			attempts, total, err = client.ListInvoicePaymentAttempts(ctx, fixture.Invoice, 10, 0)
			require.NoError(t, err)
			require.EqualValues(t, 2, total, "history is append-only")
			require.Equal(t, "settled", attempts[0].Status)
			require.Equal(t, "failed", attempts[1].Status)
			require.Equal(t, sales+1, gateway.SaleCount())
			require.Equal(t, 1, h.OwedPaymentTransfers(fixture.Customer))
		})

		t.Run("idempotency_key_bound_to_request", func(t *testing.T) {
			gateway.SetMode(NMISaleApprove)
			gateway.SetVisible(true)
			sales := gateway.SaleCount()
			fixture := h.SeedPastDueInvoice(d.runtime(), d.merchant, "USD", 3_000_000)
			second := h.SeedAnotherInvoice(d.runtime(), fixture, 4_000_000)
			key := "bound-" + uuid.NewString()[:8]
			first, err := payNow(fixture, key, fixture.Method)
			require.NoError(t, err)
			require.Equal(t, "settled", first.Attempt.Status)

			_, err = client.PayInvoiceNow(ctx, openrails.PayInvoiceNowRequest{CustomerID: openrails.CustomerID(fixture.Customer), InvoiceID: second, PaymentMethodID: openrails.PaymentMethodID(fixture.Method), IdempotencyKey: key})
			requireRefusal(t, err, openrails.ErrInvoiceRetryIdempotencyConflict, openrails.CodeInvoiceRetryIdempotencyConflict)
			require.Equal(t, sales+1, gateway.SaleCount(), "the same key on another invoice never charges")
			_, total, err := client.ListInvoicePaymentAttempts(ctx, second, 10, 0)
			require.NoError(t, err)
			require.Zero(t, total, "and records nothing on it")

			replayed, err := payNow(fixture, key, fixture.Method)
			require.NoError(t, err)
			require.True(t, replayed.Replayed)
			require.Equal(t, first.Attempt.ID, replayed.Attempt.ID)
			paid, err := client.PayInvoiceNow(ctx, openrails.PayInvoiceNowRequest{CustomerID: openrails.CustomerID(fixture.Customer), InvoiceID: second, PaymentMethodID: openrails.PaymentMethodID(fixture.Method), IdempotencyKey: "second-" + uuid.NewString()[:8]})
			require.NoError(t, err)
			require.Equal(t, "settled", paid.Attempt.Status)
			require.Equal(t, sales+2, gateway.SaleCount())
		})

		t.Run("provider_managed_rail_refused", func(t *testing.T) {
			gateway.SetMode(NMISaleApprove)
			sales := gateway.SaleCount()
			fixture := h.SeedPastDueInvoice(d.runtime(), d.merchant, "USD", 7_000_000)
			stripe := h.SeedStripeMethod(fixture.Merchant, fixture.Customer)
			_, err := payNow(fixture, "stripe-"+uuid.NewString()[:8], stripe)
			requireRefusal(t, err, openrails.ErrPaymentRecoveryRailUnsupported, openrails.CodePaymentRecoveryRailUnsupported)
			require.ErrorIs(t, err, openrails.ErrConflict)
			require.Equal(t, sales, gateway.SaleCount(), "no provider traffic")
			_, total, err := client.ListInvoicePaymentAttempts(ctx, fixture.Invoice, 10, 0)
			require.NoError(t, err)
			require.Zero(t, total, "no attempt is recorded")
			invoice, err := client.GetMerchantInvoice(ctx, fixture.Invoice)
			require.NoError(t, err)
			require.Equal(t, "past_due", invoice.Status)
			require.Nil(t, invoice.CollectionIntentID)
			require.Zero(t, h.OwedPaymentTransfers(fixture.Customer))
		})

		t.Run("other_customer_refused", func(t *testing.T) {
			sales := gateway.SaleCount()
			fixture := h.SeedPastDueInvoice(d.runtime(), d.merchant, "USD", 8_000_000)
			other := h.SeedPastDueInvoice(d.runtime(), d.merchant, "USD", 8_000_000)
			_, err := client.PayInvoiceNow(ctx, openrails.PayInvoiceNowRequest{CustomerID: openrails.CustomerID(other.Customer), InvoiceID: fixture.Invoice, PaymentMethodID: openrails.PaymentMethodID(other.Method), IdempotencyKey: "cross-" + uuid.NewString()[:8]})
			require.ErrorIs(t, err, openrails.ErrNotFound)
			_, err = payNow(fixture, "foreign-method-"+uuid.NewString()[:8], other.Method)
			require.ErrorIs(t, err, openrails.ErrCollectionPaymentMethodInvalid)
			require.Equal(t, sales, gateway.SaleCount())
			_, total, err := client.ListInvoicePaymentAttempts(ctx, fixture.Invoice, 10, 0)
			require.NoError(t, err)
			require.Zero(t, total)
		})

		t.Run("concurrent_same_key_one_submission", func(t *testing.T) {
			for _, tc := range []struct {
				name   string
				mode   NMISaleMode
				status string
			}{{"settles", NMISaleApprove, "settled"}, {"declines", NMISaleDecline, "failed"}} {
				t.Run(tc.name, func(t *testing.T) {
					gateway.SetMode(tc.mode)
					gateway.SetVisible(true)
					sales := gateway.SaleCount()
					fixture := h.SeedPastDueInvoice(d.runtime(), d.merchant, "USD", 4_000_000)
					key := "same-" + uuid.NewString()[:8]
					release := h.HoldRowLock("invoices", fixture.Invoice)
					results := make([]*openrails.InvoicePayNowResult, 2)
					errs := make([]error, 2)
					var wg sync.WaitGroup
					for i := range results {
						wg.Add(1)
						go func(i int) {
							defer wg.Done()
							results[i], errs[i] = payNow(fixture, key, fixture.Method)
						}(i)
					}
					require.Eventually(t, func() bool { return h.LockWaiters() >= 2 }, 20*time.Second, 20*time.Millisecond, "both requests wait on the invoice lock")
					release()
					wg.Wait()

					// The caller that executes answers terminally; the other names
					// the same attempt, terminal or still in flight (202).
					attempt := map[string]bool{}
					replays, terminal := 0, 0
					for i := range results {
						if errs[i] != nil {
							require.Equal(t, "failed", tc.status, "%v", errs[i])
							status := requireDeclined(t, errs[i])
							attempt[status.Metadata["attempt_id"].(string)] = true
							terminal++
							if status.Metadata["replayed"] == true {
								replays++
							}
							continue
						}
						attempt[results[i].Attempt.ID.String()] = true
						if results[i].Replayed {
							replays++
						}
						if results[i].Operation.Unresolved() {
							require.Equal(t, "attempted", results[i].Attempt.Status)
							continue
						}
						require.Equal(t, tc.status, results[i].Attempt.Status)
						terminal++
					}
					require.Len(t, attempt, 1, "both answers name the same attempt")
					require.Equal(t, 1, replays, "one caller enqueued, the other replayed")
					require.GreaterOrEqual(t, terminal, 1)
					_, total, err := client.ListInvoicePaymentAttempts(ctx, fixture.Invoice, 10, 0)
					require.NoError(t, err)
					require.EqualValues(t, 1, total)
					if tc.status == "settled" {
						require.Equal(t, sales+1, gateway.SaleCount(), "one provider submission")
						require.Equal(t, 1, h.OwedPaymentTransfers(fixture.Customer))
					} else {
						require.Equal(t, sales, gateway.SaleCount(), "a decline lands nothing")
						require.Zero(t, h.OwedPaymentTransfers(fixture.Customer))
					}
				})
			}
		})

		t.Run("lost_response_converges_after_restart", func(t *testing.T) {
			gateway.SetMode(NMISaleUncertain)
			gateway.SetVisible(false)
			sales := gateway.SaleCount()
			fixture := h.SeedPastDueInvoice(d.runtime(), d.merchant, "USD", 9_000_000)
			key := "lost-" + uuid.NewString()[:8]
			result, err := payNow(fixture, key, fixture.Method)
			require.NoError(t, err)
			require.Equal(t, "unknown_needs_verify", result.Operation.Status)
			require.True(t, result.Operation.Unresolved())
			require.Equal(t, "attempted", result.Attempt.Status)
			require.Equal(t, "past_due", result.Invoice.Status)
			require.NotNil(t, result.Invoice.Recovery)
			require.False(t, result.Invoice.Recovery.Retryable)
			require.Equal(t, openrails.RecoveryBlockedOutcomeUnknown, result.Invoice.Recovery.BlockedReason)
			require.NotNil(t, result.Invoice.Recovery.Operation)
			require.Equal(t, result.Operation.ID, result.Invoice.Recovery.Operation.ID)
			require.Equal(t, sales+1, gateway.SaleCount())
			op := h.LatestCollectionOperation(fixture.Invoice)
			require.Equal(t, result.Operation.ID, op.ID)
			sale, ok := gateway.SaleForOrder(op.ID.String())
			require.True(t, ok)

			replayed, err := payNow(fixture, key, fixture.Method)
			require.NoError(t, err)
			require.True(t, replayed.Replayed)
			require.Equal(t, result.Attempt.ID, replayed.Attempt.ID)
			require.True(t, replayed.Operation.Unresolved())
			_, err = payNow(fixture, "second-"+uuid.NewString()[:8], fixture.Method)
			requireRefusal(t, err, openrails.ErrConflict, openrails.CodeInvoiceRetryOutcomeUnknown)
			require.Equal(t, sales+1, gateway.SaleCount(), "nothing is resent while the outcome is unknown")

			d.stop()
			gateway.SetVisible(true)
			d.start()
			client = d.client()
			_, err = client.PayInvoiceNow(ctx, openrails.PayInvoiceNowRequest{CustomerID: openrails.CustomerID(fixture.Customer), InvoiceID: fixture.Invoice, PaymentMethodID: openrails.PaymentMethodID(fixture.Method), IdempotencyKey: "after-restart-" + uuid.NewString()[:8]})
			requireRefusal(t, err, openrails.ErrConflict, openrails.CodeInvoiceRetryOutcomeUnknown)
			require.Equal(t, sales+1, gateway.SaleCount(), "the restarted deployment does not resend")

			h.MakeOperationDue(op.ID)
			h.FireProviderIntentVerify(h.Pool())
			require.Eventually(t, func() bool {
				invoice, err := client.GetMerchantInvoice(ctx, fixture.Invoice)
				return err == nil && invoice.Status == "paid"
			}, 90*time.Second, 500*time.Millisecond, "the restarted deployment's verifier settles from the visible receipt")
			settled, err := client.PayInvoiceNow(ctx, openrails.PayInvoiceNowRequest{CustomerID: openrails.CustomerID(fixture.Customer), InvoiceID: fixture.Invoice, PaymentMethodID: openrails.PaymentMethodID(fixture.Method), IdempotencyKey: key})
			require.NoError(t, err)
			require.True(t, settled.Replayed)
			require.Equal(t, "settled", settled.Attempt.Status)
			require.Equal(t, "succeeded", settled.Operation.Status)
			require.Equal(t, sale.TransactionID, *settled.Attempt.RailPaymentID)
			require.Equal(t, sales+1, gateway.SaleCount())
			require.Equal(t, 1, h.OwedPaymentTransfers(fixture.Customer))
		})
	})
}

func TestCustomerRetryNowAcrossDeployments(t *testing.T) {
	ctx := context.Background()
	h := New(t, ctx)
	gateway := NewFakeNMIGateway(t)
	runMoneyDeployments(t, h, gateway, func(t *testing.T, d moneyDeployment) {
		client := d.client()
		retryNow := func(f SubscriptionFixture, key string) (*openrails.SubscriptionRetryNowResult, error) {
			return client.RetrySubscriptionNow(ctx, openrails.RetrySubscriptionNowRequest{CustomerID: openrails.CustomerID(f.Customer), SubscriptionID: openrails.SubscriptionID(f.Subscription), IdempotencyKey: key})
		}
		// seed registers the subscription's NMI plan ($12.00, the fixture
		// price): a rebill names the subscription and NMI charges its plan.
		seed := func(opts ...SubscriptionOption) SubscriptionFixture {
			f := h.SeedPastDueSubscription(d.runtime(), d.merchant, opts...)
			gateway.RegisterPlan(f.RailSubscriptionID, "12.00", "USD")
			return f
		}

		t.Run("success_renews_and_clears_dunning", func(t *testing.T) {
			gateway.SetMode(NMISaleApprove)
			gateway.SetVisible(true)
			sales := gateway.SaleCount()
			fixture := seed()
			before, err := client.GetSubscription(ctx, openrails.SubscriptionID(fixture.Subscription))
			require.NoError(t, err)
			require.Equal(t, "past_due", before.Status)
			require.NotNil(t, before.NextRetryAt)

			key := "retry-" + uuid.NewString()[:8]
			result, err := retryNow(fixture, key)
			require.NoError(t, err)
			require.False(t, result.Replayed)
			require.Equal(t, "succeeded", result.Operation.Status)
			require.Equal(t, "active", result.Subscription.Status)
			require.Nil(t, result.Subscription.NextRetryAt, "the dunning schedule is cleared")
			require.Nil(t, result.Subscription.RetryAttempts)
			require.Nil(t, result.Subscription.LastRetryAt)
			require.NotNil(t, result.Subscription.CurrentPeriodEndsAt)
			require.True(t, result.Subscription.CurrentPeriodEndsAt.After(fixture.PeriodEnd), "the period advanced")
			require.NotNil(t, result.Payment, "the confirmed charge is the renewal payment")
			require.Equal(t, "completed", result.Payment.Status)
			require.Equal(t, fixture.Amount, result.Payment.Amount)
			require.NotNil(t, result.Subscription.Recovery)
			require.False(t, result.Subscription.Recovery.Retryable)
			require.Equal(t, openrails.RecoveryBlockedNotDue, result.Subscription.Recovery.BlockedReason)
			require.Equal(t, sales+1, gateway.SaleCount())
			sale, ok := gateway.SaleForOrder("rebill-" + fixture.Subscription.String() + "-" + strconv.FormatInt(fixture.PeriodEnd.Unix(), 10))
			require.True(t, ok, "the wire order id is the period's rebill reference")
			require.Equal(t, sale.TransactionID, result.Payment.TransactionID)
			require.Equal(t, fixture.Vault, sale.Vault)
			state := h.SubscriptionState(fixture.Subscription)
			require.Equal(t, "active", state.Status)
			require.Nil(t, state.NextRetryAt)
			require.Equal(t, []string{"completed"}, h.SubscriptionPayments(fixture.Subscription))

			replayed, err := retryNow(fixture, key)
			require.NoError(t, err)
			require.True(t, replayed.Replayed)
			require.Equal(t, result.Operation.ID, replayed.Operation.ID)
			require.Equal(t, "active", replayed.Subscription.Status)
			require.Equal(t, sales+1, gateway.SaleCount(), "a replay never charges")
			_, err = retryNow(fixture, "again-"+uuid.NewString()[:8])
			requireRefusal(t, err, openrails.ErrSubscriptionNotRetryable, openrails.CodeSubscriptionNotRetryable)
			require.Equal(t, 1, h.RebillOperations(fixture.Subscription))
			expires := h.OperationExpiry(result.Operation.ID)
			require.NotNil(t, expires, "retry-now bounds its operation by the dunning window (#839)")
			require.WithinDuration(t, fixture.PeriodEnd.Add(14*24*time.Hour), *expires, time.Minute, "monthly window: last retry offset + one day")
		})

		t.Run("contradicting_receipt_stays_unknown", func(t *testing.T) {
			gateway.SetMode(NMISaleUncertain)
			gateway.SetVisible(false)
			sales := gateway.SaleCount()
			fixture := seed()
			result, err := retryNow(fixture, "contradicted-"+uuid.NewString()[:8])
			require.NoError(t, err)
			require.Equal(t, "unknown_needs_verify", result.Operation.Status)
			order := "rebill-" + fixture.Subscription.String() + "-" + strconv.FormatInt(fixture.PeriodEnd.Unix(), 10)
			require.True(t, gateway.TamperSale(order, func(s *NMISale) { s.Vault, s.Amount = "someone-elses-vault", "0.01" }))
			sale, ok := gateway.SaleForOrder(order)
			require.True(t, ok)
			gateway.SetVisible(true)

			h.MakeOperationDue(result.Operation.ID)
			h.FireProviderIntentVerify(h.Pool())
			require.Eventually(t, func() bool {
				return strings.Contains(h.OperationEvidence(result.Operation.ID), "provider_contradiction")
			}, 90*time.Second, 500*time.Millisecond, "the deployment's verifier retains the contradiction")
			require.Equal(t, "unknown_needs_verify", h.LatestRebillOperation(fixture.Subscription).Status, "a contradicting sale never settles")
			require.Equal(t, "past_due", h.SubscriptionState(fixture.Subscription).Status, "no renewal")
			require.Empty(t, h.SubscriptionPayments(fixture.Subscription), "no payment recorded")

			out, err := h.ResolveOperation(gateway.URL, d.merchant, result.Operation.ID, sale.TransactionID, false)
			require.Error(t, err, "the contradicting sale is not this rebill's receipt: %s", out)
			out, err = h.ResolveOperation(gateway.URL, d.merchant, result.Operation.ID, "", true)
			require.Error(t, err, "non-execution cannot be attested against provider evidence: %s", out)
			require.Equal(t, "unknown_needs_verify", h.LatestRebillOperation(fixture.Subscription).Status)
			require.Empty(t, h.SubscriptionPayments(fixture.Subscription))
			require.Equal(t, sales+1, gateway.SaleCount(), "nothing is resent")
			sub, err := client.GetSubscription(ctx, openrails.SubscriptionID(fixture.Subscription))
			require.NoError(t, err)
			require.Equal(t, "past_due", sub.Status)
		})

		t.Run("psp_mismatch_refused_without_provider_traffic", func(t *testing.T) {
			gateway.SetMode(NMISaleApprove)
			gateway.SetVisible(true)
			sales := gateway.SaleCount()
			fixture := seed()
			h.ReattributeToAnotherPSP(fixture)
			_, err := retryNow(fixture, "psp-"+uuid.NewString()[:8])
			requireRefusal(t, err, openrails.ErrPaymentMethodPSPMismatch, openrails.CodePaymentMethodPSPMismatch)
			require.Equal(t, sales, gateway.SaleCount(), "no provider traffic")
			require.Zero(t, h.RebillOperations(fixture.Subscription), "no operation")
			state := h.SubscriptionState(fixture.Subscription)
			require.Equal(t, "past_due", state.Status)
			require.EqualValues(t, 1, *state.RetryAttempts, "nothing counted")
		})

		t.Run("idempotency_key_bound_to_request", func(t *testing.T) {
			gateway.SetMode(NMISaleDecline)
			gateway.SetVisible(true)
			sales := gateway.SaleCount()
			fixture := seed()
			other := seed(SubscriptionForCustomer(fixture.Customer))
			key := "bound-" + uuid.NewString()[:8]
			_, err := retryNow(fixture, key)
			first := requireDeclined(t, err)
			gateway.SetMode(NMISaleApprove)

			_, err = retryNow(other, key)
			requireRefusal(t, err, openrails.ErrSubscriptionRetryIdempotencyConflict, openrails.CodeSubscriptionRetryIdempotencyConflict)
			method := openrails.PaymentMethodID(other.Method) // the payer's, not this subscription's
			_, err = client.RetrySubscriptionNow(ctx, openrails.RetrySubscriptionNowRequest{CustomerID: openrails.CustomerID(fixture.Customer), SubscriptionID: openrails.SubscriptionID(fixture.Subscription), PaymentMethodID: &method, IdempotencyKey: key})
			requireRefusal(t, err, openrails.ErrSubscriptionRetryIdempotencyConflict, openrails.CodeSubscriptionRetryIdempotencyConflict)
			require.Equal(t, sales, gateway.SaleCount(), "a conflict never charges")
			require.Zero(t, h.RebillOperations(other.Subscription))

			_, err = retryNow(fixture, key)
			replay := requireDeclined(t, err)
			require.Equal(t, first.Metadata["operation_id"], replay.Metadata["operation_id"], "the same request still replays")
			require.Equal(t, 1, h.RebillOperations(fixture.Subscription))
		})

		t.Run("dunning_window_and_skewed_lease", func(t *testing.T) {
			gateway.SetMode(NMISaleApprove)
			gateway.SetVisible(true)
			sales := gateway.SaleCount()
			stale := seed()
			h.AgePastDunningWindow(stale)
			_, err := retryNow(stale, "stale-"+uuid.NewString()[:8])
			requireRefusal(t, err, openrails.ErrSubscriptionNotRetryable, openrails.CodeSubscriptionNotRetryable)
			require.Zero(t, h.RebillOperations(stale.Subscription))

			leased := seed()
			h.SkewedWorkerLease(leased)
			_, err = retryNow(leased, "leased-"+uuid.NewString()[:8])
			requireRefusal(t, err, openrails.ErrSubscriptionRetryInProgress, openrails.CodeSubscriptionRetryInProgress)
			require.Zero(t, h.RebillOperations(leased.Subscription), "a live worker lease is never taken over")
			require.Equal(t, sales, gateway.SaleCount())
		})

		t.Run("decline_records_attempt_and_keeps_schedule", func(t *testing.T) {
			gateway.SetMode(NMISaleDecline)
			sales := gateway.SaleCount()
			fixture := seed()
			key := "decline-" + uuid.NewString()[:8]
			_, err := retryNow(fixture, key)
			status := requireDeclined(t, err)
			require.Equal(t, "sub_"+fixture.Subscription.String(), status.Metadata["subscription_id"])
			require.Equal(t, "past_due", status.Metadata["subscription_status"])
			require.Equal(t, true, status.Metadata["retryable"], "%v", status.Metadata)
			require.NotEmpty(t, status.Metadata["next_attempt_at"], "bucket 1 keeps the schedule")
			require.Equal(t, sales, gateway.SaleCount(), "a decline lands nothing")
			state := h.SubscriptionState(fixture.Subscription)
			require.Equal(t, "past_due", state.Status)
			require.EqualValues(t, 2, *state.RetryAttempts, "the decline doctrine advanced the ordinal")
			require.True(t, state.NextRetryAt.After(time.Now().Add(time.Hour)), "the next retry is scheduled, not leased")
			require.Equal(t, []string{"failed"}, h.SubscriptionPayments(fixture.Subscription), "the declined attempt is recorded")
			require.Equal(t, "failed_terminal", h.LatestRebillOperation(fixture.Subscription).Status)
			sub, err := client.GetSubscription(ctx, openrails.SubscriptionID(fixture.Subscription))
			require.NoError(t, err)
			require.Equal(t, "past_due", sub.Status)

			_, err = retryNow(fixture, key)
			replay := requireDeclined(t, err)
			require.Equal(t, status.Metadata["operation_id"], replay.Metadata["operation_id"], "the same key answers with the same attempt")
			require.Equal(t, true, replay.Metadata["replayed"])
			require.Equal(t, []string{"failed"}, h.SubscriptionPayments(fixture.Subscription))
			require.Equal(t, 1, h.RebillOperations(fixture.Subscription))

			gateway.SetMode(NMISaleApprove)
			renewed, err := retryNow(fixture, "retry-"+uuid.NewString()[:8])
			require.NoError(t, err)
			require.Equal(t, "active", renewed.Subscription.Status)
			require.NotNil(t, renewed.Payment)
			require.Equal(t, []string{"completed", "failed"}, h.SubscriptionPayments(fixture.Subscription), "history is append-only")
			require.Equal(t, 2, h.RebillOperations(fixture.Subscription))
			require.Equal(t, sales+1, gateway.SaleCount())
		})

		t.Run("provider_managed_rails_refused", func(t *testing.T) {
			gateway.SetMode(NMISaleApprove)
			sales := gateway.SaleCount()
			for name, opts := range map[string][]SubscriptionOption{
				"stripe":              {SubscriptionOnRail(models.RailStripe)},
				"ccbill":              {SubscriptionOnRail(models.RailCCBill)},
				"nmi_provider_billed": {SubscriptionProviderBilled()},
			} {
				fixture := seed(opts...)
				_, err := retryNow(fixture, name+"-"+uuid.NewString()[:8])
				requireRefusal(t, err, openrails.ErrPaymentRecoveryRailUnsupported, openrails.CodePaymentRecoveryRailUnsupported)
				require.Zero(t, h.RebillOperations(fixture.Subscription), "%s: no operation", name)
				require.Empty(t, h.SubscriptionPayments(fixture.Subscription), "%s: no attempt", name)
				state := h.SubscriptionState(fixture.Subscription)
				require.Equal(t, "past_due", state.Status)
				require.EqualValues(t, 1, *state.RetryAttempts)
				sub, err := client.GetSubscription(ctx, openrails.SubscriptionID(fixture.Subscription))
				require.NoError(t, err)
				require.Equal(t, "past_due", sub.Status)
			}
			require.Equal(t, sales, gateway.SaleCount(), "no provider traffic")
		})

		t.Run("other_customer_refused", func(t *testing.T) {
			sales := gateway.SaleCount()
			fixture := seed()
			other := seed()
			_, err := client.RetrySubscriptionNow(ctx, openrails.RetrySubscriptionNowRequest{CustomerID: openrails.CustomerID(other.Customer), SubscriptionID: openrails.SubscriptionID(fixture.Subscription), IdempotencyKey: "cross-" + uuid.NewString()[:8]})
			require.ErrorIs(t, err, openrails.ErrNotFound)
			foreign := openrails.PaymentMethodID(other.Method)
			_, err = client.RetrySubscriptionNow(ctx, openrails.RetrySubscriptionNowRequest{CustomerID: openrails.CustomerID(fixture.Customer), SubscriptionID: openrails.SubscriptionID(fixture.Subscription), PaymentMethodID: &foreign, IdempotencyKey: "foreign-" + uuid.NewString()[:8]})
			require.ErrorIs(t, err, openrails.ErrCollectionPaymentMethodInvalid)
			require.Equal(t, sales, gateway.SaleCount())
			require.Zero(t, h.RebillOperations(fixture.Subscription))
		})

		t.Run("concurrent_same_key_one_submission", func(t *testing.T) {
			for _, tc := range []struct {
				name    string
				mode    NMISaleMode
				decline bool
			}{{"renews", NMISaleApprove, false}, {"declines", NMISaleDecline, true}} {
				t.Run(tc.name, func(t *testing.T) {
					gateway.SetMode(tc.mode)
					gateway.SetVisible(true)
					sales := gateway.SaleCount()
					fixture := seed()
					key := "same-" + uuid.NewString()[:8]
					release := h.HoldRowLock("subscriptions", fixture.Subscription)
					results := make([]*openrails.SubscriptionRetryNowResult, 2)
					errs := make([]error, 2)
					var wg sync.WaitGroup
					for i := range results {
						wg.Add(1)
						go func(i int) {
							defer wg.Done()
							results[i], errs[i] = retryNow(fixture, key)
						}(i)
					}
					require.Eventually(t, func() bool { return h.LockWaiters() >= 2 }, 20*time.Second, 20*time.Millisecond, "both requests wait on the subscription lock")
					release()
					wg.Wait()

					operations := map[string]bool{}
					replays, terminal := 0, 0
					for i := range results {
						if errs[i] != nil {
							require.True(t, tc.decline, "%v", errs[i])
							status := requireDeclined(t, errs[i])
							operations[status.Metadata["operation_id"].(string)] = true
							terminal++
							if status.Metadata["replayed"] == true {
								replays++
							}
							continue
						}
						operations[results[i].Operation.ID.String()] = true
						if results[i].Replayed {
							replays++
						}
						if results[i].Operation.Unresolved() {
							continue
						}
						require.False(t, tc.decline)
						require.Equal(t, "active", results[i].Subscription.Status)
						terminal++
					}
					require.Len(t, operations, 1, "both answers name the same operation")
					require.Equal(t, 1, replays, "one caller enqueued, the other replayed")
					require.GreaterOrEqual(t, terminal, 1)
					require.Equal(t, 1, h.RebillOperations(fixture.Subscription))
					if tc.decline {
						require.Equal(t, sales, gateway.SaleCount(), "a decline lands nothing")
						require.Equal(t, []string{"failed"}, h.SubscriptionPayments(fixture.Subscription), "one recorded decline")
						require.EqualValues(t, 2, *h.SubscriptionState(fixture.Subscription).RetryAttempts, "the decline doctrine ran once")
					} else {
						require.Equal(t, sales+1, gateway.SaleCount(), "one provider submission")
						require.Equal(t, []string{"completed"}, h.SubscriptionPayments(fixture.Subscription))
					}
				})
			}
		})

		t.Run("lost_response_converges_after_restart", func(t *testing.T) {
			gateway.SetMode(NMISaleUncertain)
			gateway.SetVisible(false)
			sales := gateway.SaleCount()
			fixture := seed()
			key := "lost-" + uuid.NewString()[:8]
			result, err := retryNow(fixture, key)
			require.NoError(t, err)
			require.Equal(t, "unknown_needs_verify", result.Operation.Status)
			require.True(t, result.Operation.Unresolved())
			require.Nil(t, result.Payment)
			require.Equal(t, "past_due", result.Subscription.Status)
			require.NotNil(t, result.Subscription.Recovery)
			require.False(t, result.Subscription.Recovery.Retryable)
			require.Equal(t, openrails.RecoveryBlockedOutcomeUnknown, result.Subscription.Recovery.BlockedReason)
			require.NotNil(t, result.Subscription.Recovery.Operation)
			require.Equal(t, result.Operation.ID, result.Subscription.Recovery.Operation.ID)
			require.Equal(t, sales+1, gateway.SaleCount())
			op := h.LatestRebillOperation(fixture.Subscription)
			require.Equal(t, result.Operation.ID, op.ID)
			require.Equal(t, "unknown_needs_verify", op.Status)
			require.Empty(t, h.SubscriptionPayments(fixture.Subscription), "an unresolved attempt is neither a payment nor a failure")
			state := h.SubscriptionState(fixture.Subscription)
			require.EqualValues(t, 1, *state.RetryAttempts, "an unresolved attempt does not advance the schedule")

			replayed, err := retryNow(fixture, key)
			require.NoError(t, err)
			require.True(t, replayed.Replayed)
			require.Equal(t, op.ID, replayed.Operation.ID)
			require.True(t, replayed.Operation.Unresolved())
			_, err = retryNow(fixture, "second-"+uuid.NewString()[:8])
			requireRefusal(t, err, openrails.ErrSubscriptionRetryOutcomeUnknown, openrails.CodeSubscriptionRetryOutcomeUnknown)
			require.Equal(t, sales+1, gateway.SaleCount(), "nothing is resent while the outcome is unknown")
			require.Equal(t, 1, h.RebillOperations(fixture.Subscription))

			d.stop()
			gateway.SetVisible(true)
			d.start()
			client = d.client()
			_, err = client.RetrySubscriptionNow(ctx, openrails.RetrySubscriptionNowRequest{CustomerID: openrails.CustomerID(fixture.Customer), SubscriptionID: openrails.SubscriptionID(fixture.Subscription), IdempotencyKey: "after-restart-" + uuid.NewString()[:8]})
			requireRefusal(t, err, openrails.ErrSubscriptionRetryOutcomeUnknown, openrails.CodeSubscriptionRetryOutcomeUnknown)
			require.Equal(t, sales+1, gateway.SaleCount(), "the restarted deployment does not resend")

			h.MakeOperationDue(op.ID)
			h.FireProviderIntentVerify(h.Pool())
			require.Eventually(t, func() bool {
				sub, err := client.GetSubscription(ctx, openrails.SubscriptionID(fixture.Subscription))
				return err == nil && sub.Status == "active"
			}, 90*time.Second, 500*time.Millisecond, "the restarted deployment's verifier renews from the visible receipt")
			require.Equal(t, "succeeded", h.LatestRebillOperation(fixture.Subscription).Status)
			settled, err := client.RetrySubscriptionNow(ctx, openrails.RetrySubscriptionNowRequest{CustomerID: openrails.CustomerID(fixture.Customer), SubscriptionID: openrails.SubscriptionID(fixture.Subscription), IdempotencyKey: key})
			require.NoError(t, err)
			require.True(t, settled.Replayed)
			require.Equal(t, "succeeded", settled.Operation.Status)
			require.Equal(t, "active", settled.Subscription.Status)
			require.NotNil(t, settled.Payment)
			require.Equal(t, []string{"completed"}, h.SubscriptionPayments(fixture.Subscription))
			require.Equal(t, sales+1, gateway.SaleCount())
			require.Equal(t, 1, h.RebillOperations(fixture.Subscription))
			state = h.SubscriptionState(fixture.Subscription)
			require.Nil(t, state.NextRetryAt)
		})
	})
}
