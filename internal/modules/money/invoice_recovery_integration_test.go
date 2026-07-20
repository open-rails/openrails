//go:build integration

package money_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/stretchr/testify/require"
)

type blockingRecoveryCharger struct {
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (c *blockingRecoveryCharger) ChargeSavedMethod(_ context.Context, req money.ChargeRequest) (money.ChargeResult, error) {
	c.once.Do(func() { close(c.entered) })
	<-c.release
	return money.ChargeResult{TransactionID: "tx_" + req.IdempotencyKey}, nil
}

func TestInvoiceRecovery_SelectsSeparateMethodAndSettles(t *testing.T) {
	svc, pool, payer, currency, ctx := moneyInEnv(t)
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.invoice_payments WHERE customer_id = $1", payer.UUID())
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.invoice_items WHERE customer_id = $1", payer.UUID())
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.invoices WHERE customer_id = $1", payer.UUID())
	})
	autoTopupMethod := seedPaymentMethod(t, pool, ctx, payer, string(models.RailStripe))
	_, err := svc.UpsertAccountSettings(ctx, payer, currency, money.AccountSettingsInput{
		BillingMode: strptr(money.BillingModeArrears), AutoTopupPaymentMethod: &autoTopupMethod,
	})
	require.NoError(t, err)
	_, err = svc.AccrueOwed(ctx, payer, currency, "usage", "recovery-success", 5_000_000)
	require.NoError(t, err)
	invoice, err := svc.FinalizeInvoice(ctx, payer, currency, time.Now().Add(-time.Hour), time.Now())
	require.NoError(t, err)

	declined := &fakeCharger{declineAll: true}
	_, err = svc.ChargeOutstanding(ctx, declined, 0)
	require.NoError(t, err)

	collectionMethod := seedPaymentMethod(t, pool, ctx, payer, string(models.RailStripe))
	require.NoError(t, svc.SetInvoiceCollectionPaymentMethod(ctx, payer, currency, collectionMethod))

	var storedAutoTopup, storedCollection *string
	require.NoError(t, pool.QueryRow(ctx, `
		SELECT auto_topup_payment_method_id::text, collection_payment_method_id::text
		FROM openrails.money_settings
		WHERE customer_id = $1 AND currency = $2`, payer.UUID(), currency).Scan(&storedAutoTopup, &storedCollection))
	require.Equal(t, autoTopupMethod.String(), *storedAutoTopup)
	require.Equal(t, collectionMethod.String(), *storedCollection)

	succeeded := &fakeCharger{}
	retried, err := svc.RetryInvoiceCollection(ctx, succeeded, payer, invoice.ID)
	require.NoError(t, err)
	require.Equal(t, "paid", retried.Status)
	require.Nil(t, retried.LastCollectionFailureCode)
	require.Len(t, succeeded.charges, 1)
	require.Equal(t, collectionMethod, succeeded.charges[0].PaymentMethodID)
	require.Equal(t, "invoice:"+invoice.ID.String()+":attempt:1", succeeded.charges[0].IdempotencyKey)

	attempts, total, err := svc.ListInvoicePaymentAttempts(ctx, payer, invoice.ID, 20, 0)
	require.NoError(t, err)
	require.Equal(t, 2, total)
	require.Len(t, attempts, 2)
	require.Equal(t, "settled", attempts[0].Status)
	require.Equal(t, "failed", attempts[1].Status)
}

func TestInvoiceRecovery_IdempotentRetryBindsMethodAndReplays(t *testing.T) {
	svc, pool, payer, currency, ctx := moneyInEnv(t)
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.invoice_payments WHERE customer_id = $1", payer.UUID())
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.invoice_items WHERE customer_id = $1", payer.UUID())
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.invoices WHERE customer_id = $1", payer.UUID())
	})
	defaultMethod := seedPaymentMethod(t, pool, ctx, payer, string(models.RailStripe))
	boundMethod := seedPaymentMethod(t, pool, ctx, payer, string(models.RailStripe))
	_, err := svc.UpsertAccountSettings(ctx, payer, currency, money.AccountSettingsInput{
		BillingMode: strptr(money.BillingModeArrears), AutoTopupPaymentMethod: &defaultMethod,
	})
	require.NoError(t, err)
	_, err = svc.AccrueOwed(ctx, payer, currency, "usage", "recovery-idempotent", 5_000_000)
	require.NoError(t, err)
	invoice, err := svc.FinalizeInvoice(ctx, payer, currency, time.Now().Add(-time.Hour), time.Now())
	require.NoError(t, err)
	_, err = svc.ChargeOutstanding(ctx, &fakeCharger{declineAll: true}, 0)
	require.NoError(t, err)

	request := money.InvoiceCollectionRetryRequest{
		InvoiceID: invoice.ID, IdempotencyKey: "client-retry-key", PaymentMethodID: boundMethod,
	}
	charger := &fakeCharger{}
	result, err := svc.RetryInvoiceCollectionIdempotent(ctx, charger, payer, request)
	require.NoError(t, err)
	require.False(t, result.Replayed)
	require.Equal(t, "paid", result.Invoice.Status)
	require.Equal(t, "settled", result.Attempt.Status)
	require.Equal(t, boundMethod, *result.Attempt.PaymentMethodID)
	require.Len(t, charger.charges, 1)
	require.Equal(t, boundMethod, charger.charges[0].PaymentMethodID)

	replayCharger := &fakeCharger{}
	replayed, err := svc.RetryInvoiceCollectionIdempotent(ctx, replayCharger, payer, request)
	require.NoError(t, err)
	require.True(t, replayed.Replayed)
	require.Equal(t, result.Attempt.ID, replayed.Attempt.ID)
	require.Empty(t, replayCharger.charges)

	request.PaymentMethodID = defaultMethod
	_, err = svc.RetryInvoiceCollectionIdempotent(ctx, replayCharger, payer, request)
	require.ErrorIs(t, err, money.ErrInvoiceRetryIdempotencyConflict)

	_, err = pool.Exec(ctx, "DELETE FROM openrails.payment_methods WHERE id = $1", boundMethod)
	require.NoError(t, err)
	_, err = svc.RetryInvoiceCollectionIdempotent(ctx, replayCharger, payer, request)
	require.ErrorIs(t, err, money.ErrInvoiceRetryIdempotencyConflict)
}

func TestInvoiceRecovery_AmbiguousOutcomeBlocksFurtherRetries(t *testing.T) {
	svc, pool, payer, currency, ctx := moneyInEnv(t)
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.invoice_payments WHERE customer_id = $1", payer.UUID())
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.invoice_items WHERE customer_id = $1", payer.UUID())
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.invoices WHERE customer_id = $1", payer.UUID())
	})
	method := seedPaymentMethod(t, pool, ctx, payer, string(models.RailNMI))
	_, err := svc.UpsertAccountSettings(ctx, payer, currency, money.AccountSettingsInput{
		BillingMode: strptr(money.BillingModeArrears), AutoTopupPaymentMethod: &method,
	})
	require.NoError(t, err)
	_, err = svc.AccrueOwed(ctx, payer, currency, "usage", "recovery-ambiguous", 5_000_000)
	require.NoError(t, err)
	invoice, err := svc.FinalizeInvoice(ctx, payer, currency, time.Now().Add(-time.Hour), time.Now())
	require.NoError(t, err)
	_, err = svc.ChargeOutstanding(ctx, &fakeCharger{declineAll: true}, 0)
	require.NoError(t, err)

	request := money.InvoiceCollectionRetryRequest{
		InvoiceID: invoice.ID, IdempotencyKey: "ambiguous-retry-key", PaymentMethodID: method,
	}
	ambiguous := &fakeCharger{ambiguous: true}
	_, err = svc.RetryInvoiceCollectionIdempotent(ctx, ambiguous, payer, request)
	require.ErrorIs(t, err, money.ErrInvoiceRetryOutcomeUnknown)
	require.Len(t, ambiguous.charges, 1)

	blocked, err := svc.GetInvoiceByID(ctx, payer, invoice.ID)
	require.NoError(t, err)
	require.Equal(t, "past_due", blocked.Status)
	require.Equal(t, int32(1), blocked.CollectionFailureCount)
	require.Equal(t, "collection_outcome_unknown", *blocked.LastCollectionFailureCode)
	require.Nil(t, blocked.NextCollectionAttemptAt)

	attempts, total, err := svc.ListInvoicePaymentAttempts(ctx, payer, invoice.ID, 20, 0)
	require.NoError(t, err)
	require.Equal(t, 2, total)
	require.Equal(t, "attempted", attempts[0].Status)
	require.Equal(t, method, *attempts[0].PaymentMethodID)
	require.Nil(t, attempts[0].FailureReason)

	second := &fakeCharger{}
	_, err = svc.RetryInvoiceCollectionIdempotent(ctx, second, payer, request)
	require.ErrorIs(t, err, money.ErrInvoiceRetryOutcomeUnknown)
	request.IdempotencyKey = "different-key"
	_, err = svc.RetryInvoiceCollectionIdempotent(ctx, second, payer, request)
	require.ErrorIs(t, err, money.ErrInvoiceRetryOutcomeUnknown)
	require.Empty(t, second.charges)
}

func TestInvoiceRecovery_CleanFailureRestoresPriorDunningState(t *testing.T) {
	svc, pool, payer, currency, ctx := moneyInEnv(t)
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.invoice_payments WHERE customer_id = $1", payer.UUID())
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.invoice_items WHERE customer_id = $1", payer.UUID())
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.invoices WHERE customer_id = $1", payer.UUID())
	})
	method := seedPaymentMethod(t, pool, ctx, payer, string(models.RailStripe))
	_, err := svc.UpsertAccountSettings(ctx, payer, currency, money.AccountSettingsInput{
		BillingMode: strptr(money.BillingModeArrears), AutoTopupPaymentMethod: &method,
	})
	require.NoError(t, err)
	_, err = svc.AccrueOwed(ctx, payer, currency, "usage", "recovery-clean-error", 5_000_000)
	require.NoError(t, err)
	invoice, err := svc.FinalizeInvoice(ctx, payer, currency, time.Now().Add(-time.Hour), time.Now())
	require.NoError(t, err)
	_, err = svc.ChargeOutstanding(ctx, &fakeCharger{declineAll: true}, 0)
	require.NoError(t, err)
	before, err := svc.GetInvoiceByID(ctx, payer, invoice.ID)
	require.NoError(t, err)

	cleanFailure := &fakeCharger{transientFailures: 1}
	_, err = svc.RetryInvoiceCollection(ctx, cleanFailure, payer, invoice.ID)
	require.Error(t, err)
	after, err := svc.GetInvoiceByID(ctx, payer, invoice.ID)
	require.NoError(t, err)
	require.Equal(t, before.Status, after.Status)
	require.Equal(t, before.CollectionFailureCount, after.CollectionFailureCount)
	require.Equal(t, before.NextCollectionAttemptAt, after.NextCollectionAttemptAt)
	require.Equal(t, before.LastCollectionFailureCode, after.LastCollectionFailureCode)

	attempts, total, err := svc.ListInvoicePaymentAttempts(ctx, payer, invoice.ID, 20, 0)
	require.NoError(t, err)
	require.Equal(t, 1, total)
	require.Len(t, attempts, 1, "a confirmed pre-send failure must not create a provider attempt")
}

func TestInvoiceRecovery_BackgroundAmbiguityBlocksAnotherSweep(t *testing.T) {
	svc, pool, payer, currency, ctx := moneyInEnv(t)
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.invoice_payments WHERE customer_id = $1", payer.UUID())
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.invoice_items WHERE customer_id = $1", payer.UUID())
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.invoices WHERE customer_id = $1", payer.UUID())
	})
	method := seedPaymentMethod(t, pool, ctx, payer, string(models.RailNMI))
	_, err := svc.UpsertAccountSettings(ctx, payer, currency, money.AccountSettingsInput{
		BillingMode: strptr(money.BillingModeArrears), AutoTopupPaymentMethod: &method,
	})
	require.NoError(t, err)
	_, err = svc.AccrueOwed(ctx, payer, currency, "usage", "background-ambiguous", 5_000_000)
	require.NoError(t, err)
	invoice, err := svc.FinalizeInvoice(ctx, payer, currency, time.Now().Add(-time.Hour), time.Now())
	require.NoError(t, err)

	first := &fakeCharger{ambiguous: true}
	_, err = svc.ChargeOutstanding(ctx, first, 0)
	require.Error(t, err)
	require.Len(t, first.charges, 1)

	second := &fakeCharger{}
	charged, err := svc.ChargeOutstanding(ctx, second, 0)
	require.NoError(t, err)
	require.Zero(t, charged)
	require.Empty(t, second.charges)

	blocked, err := svc.GetInvoiceByID(ctx, payer, invoice.ID)
	require.NoError(t, err)
	require.Equal(t, "collection_outcome_unknown", *blocked.LastCollectionFailureCode)
}

func TestInvoiceRecovery_ActiveClaimRejectsCompetingInvoiceMutations(t *testing.T) {
	svc, pool, payer, currency, ctx := moneyInEnv(t)
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.invoice_payments WHERE customer_id = $1", payer.UUID())
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.invoice_items WHERE customer_id = $1", payer.UUID())
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.invoices WHERE customer_id = $1", payer.UUID())
	})
	method := seedPaymentMethod(t, pool, ctx, payer, string(models.RailStripe))
	_, err := svc.UpsertAccountSettings(ctx, payer, currency, money.AccountSettingsInput{
		BillingMode: strptr(money.BillingModeArrears), AutoTopupPaymentMethod: &method,
	})
	require.NoError(t, err)
	_, err = svc.AccrueOwed(ctx, payer, currency, "usage", "active-claim-guards", 5_000_000)
	require.NoError(t, err)
	invoice, err := svc.FinalizeInvoice(ctx, payer, currency, time.Now().Add(-time.Hour), time.Now())
	require.NoError(t, err)
	_, err = svc.ChargeOutstanding(ctx, &fakeCharger{declineAll: true}, 0)
	require.NoError(t, err)

	charger := &blockingRecoveryCharger{entered: make(chan struct{}), release: make(chan struct{})}
	t.Cleanup(func() {
		select {
		case <-charger.release:
		default:
			close(charger.release)
		}
	})
	retryResult := make(chan error, 1)
	go func() {
		_, retryErr := svc.RetryInvoiceCollection(ctx, charger, payer, invoice.ID)
		retryResult <- retryErr
	}()
	<-charger.entered

	_, err = svc.VoidInvoice(ctx, payer, invoice.ID)
	require.Error(t, err)
	_, err = svc.MarkInvoiceUncollectible(ctx, payer, invoice.ID)
	require.Error(t, err)
	_, err = svc.RecordOutOfBandInvoicePayment(ctx, payer, invoice.ID, 1, "wire-during-active-claim")
	require.ErrorContains(t, err, "invoice collection is in progress")

	close(charger.release)
	require.NoError(t, <-retryResult)
	paid, err := svc.GetInvoiceByID(ctx, payer, invoice.ID)
	require.NoError(t, err)
	require.Equal(t, "paid", paid.Status)
}

func TestInvoiceRecovery_RejectsAnotherPayersPaymentMethod(t *testing.T) {
	svc, _, payer, currency, ctx := moneyInEnv(t)
	_, otherPool, otherPayer, _, otherCtx := moneyInEnv(t)
	otherMethod := seedPaymentMethod(t, otherPool, otherCtx, otherPayer, string(models.RailStripe))

	err := svc.SetInvoiceCollectionPaymentMethod(ctx, payer, currency, otherMethod)
	require.ErrorIs(t, err, money.ErrCollectionPaymentMethodInvalid)
}

func TestInvoiceRecovery_RejectsUnknownRail(t *testing.T) {
	svc, pool, payer, currency, ctx := moneyInEnv(t)
	method := seedPaymentMethod(t, pool, ctx, payer, "unknown")

	err := svc.SetInvoiceCollectionPaymentMethod(ctx, payer, currency, method)
	require.ErrorIs(t, err, money.ErrCollectionPaymentMethodInvalid)
}
